package miner

// Python config-field contradiction miner (miner:config-field-py).
//
// # Design
//
// Detects contradictions between a pydantic Field(default=X) / dataclass
// field(default=X) default value and a validator in the SAME class that
// rejects exactly that sentinel value.
//
// Join key: (file, class name, field name) — the decorator-to-field binding
// is explicit in pydantic: @validator('fieldname') or @field_validator('fieldname')
// names the field as a string literal, making the join tractable without
// a type checker (unlike TypeScript Zod chains which required only consumer-side
// evidence and was descoped).
//
// # Detection algorithm
//
//  1. passPyCFFields: find class-body assignments of the form:
//       fieldname: T = Field(default=SENTINEL)
//       fieldname: T = field(default=SENTINEL)
//     where SENTINEL is an integer or None. Emit (class, field, sentinel).
//
//  2. passPyCFValidators: find decorated functions of the form:
//       @validator('fieldname') / @field_validator('fieldname')
//       def ...(cls, v):
//           if v OP SENTINEL:
//               raise ...
//     Emit (class, field, sentinel, op) for the first raise-bearing guard.
//
//  3. Join: if a field's default matches a validator's reject sentinel,
//     and the reject condition covers the default (e.g. default=0 + reject
//     v<=0 → contradiction; default=-1 + reject v<0 → contradiction),
//     emit a lead.
//
// # Precision guards
//
//   - Sentinel must be an integer literal or None (strings excluded: too
//     ambiguous without full type resolution).
//   - Only Field()/field() RHS calls (not bare defaults or other callables).
//   - Validator must have a raise statement in the guard body; a bare
//     conditional without raise is not a rejection.
//   - Both sides must be in the SAME class (not cross-class).
//   - Test files are skipped (isPyTestPath gate).
//
// # Implementation approach
//
// Pure regex over Python source text, mirroring the Go configfield.go style.
// Tree-sitter was probed and the AST is clean, but the regex approach is
// sufficient for the structural patterns here (explicit decorator names,
// explicit keyword args) and avoids grammar-query complexity for this pass.
// A tree-sitter upgrade is straightforward if precision issues arise.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	cfPyPosterLens = "miner:config-field-py"
	cfPyTargetLens = "api-contract-misuse"
	cfPyMaxLeads   = 50
)

// ─── regex patterns ───────────────────────────────────────────────────────────

// pyClassDeclRe matches a class definition line.
// Group 1: class name.
var pyClassDeclRe = regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)\s*[:(]`)

// pyFieldDefRe matches pydantic/dataclass field declarations of the form:
//
//	fieldname: T = Field(default=SENTINEL)
//	fieldname: T = field(default=SENTINEL)
//
// Group 1: field name, Group 2: sentinel value (integer or "None").
var pyFieldDefRe = regexp.MustCompile(`^\s+([a-z_][A-Za-z0-9_]*):\s*\S.*=\s*(?:Field|field)\s*\([^)]*\bdefault\s*=\s*(-?\d+|None)\b`)

// pyValidatorDecRe matches pydantic validator decorator lines.
// Captures the field name(s) from @validator('field') or @field_validator('field').
// Group 1: field name string.
var pyValidatorDecRe = regexp.MustCompile(`@(?:validator|field_validator)\s*\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)

// pyGuardRe matches a simple guard of the form:
//
//	if v OP SENTINEL:
//	if value OP SENTINEL:
//
// Group 1: comparison operator, Group 2: sentinel (integer or "None").
// We accept any single-identifier LHS (the parameter name).
var pyGuardRe = regexp.MustCompile(`\bif\s+[a-z_][A-Za-z0-9_]*\s*(==|!=|<=|<|>=|>|is\s+None|is\s+not\s+None)\s*(-?\d+|None)\b`)

// pyRaiseRe matches a raise statement (for guard body detection).
var pyRaiseRe = regexp.MustCompile(`\braise\b`)

// ─── data types ──────────────────────────────────────────────────────────────

type pyCFField struct {
	className string
	fieldName string
	sentinel  string // integer string or "None"
	line      int    // 1-based
}

type pyCFValidator struct {
	className string
	fieldName string
	rejectOp  string // the comparison operator from the guard
	sentinel  string // the rejected value
	line      int    // 1-based line of the guard
}

// ─── pass functions ───────────────────────────────────────────────────────────

func passPyCFFields(path, content string) []pyCFField {
	lines := strings.Split(content, "\n")
	var out []pyCFField
	currentClass := ""
	for i, line := range lines {
		if m := pyClassDeclRe.FindStringSubmatch(line); m != nil {
			currentClass = m[1]
			continue
		}
		if currentClass == "" {
			continue
		}
		// Detect end of class (de-indent to column 0 non-empty non-comment line).
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' && line[0] != '\n' {
			currentClass = ""
			continue
		}
		if m := pyFieldDefRe.FindStringSubmatch(line); m != nil {
			out = append(out, pyCFField{
				className: currentClass,
				fieldName: m[1],
				sentinel:  m[2],
				line:      i + 1,
			})
		}
	}
	return out
}

func passPyCFValidators(path, content string) []pyCFValidator {
	lines := strings.Split(content, "\n")
	var out []pyCFValidator
	currentClass := ""
	pendingField := "" // field name from the most recent @validator decorator
	pendingLine := 0

	for i, line := range lines {
		if m := pyClassDeclRe.FindStringSubmatch(line); m != nil {
			currentClass = m[1]
			pendingField = ""
			continue
		}
		if currentClass == "" {
			continue
		}
		// End of class.
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' && line[0] != '\n' {
			currentClass = ""
			pendingField = ""
			continue
		}
		// @validator or @field_validator decorator.
		if m := pyValidatorDecRe.FindStringSubmatch(line); m != nil {
			pendingField = m[1]
			pendingLine = i + 1
			continue
		}
		// After a validator decorator, look for a guard with raise in the next ~15 lines.
		if pendingField != "" {
			if m := pyGuardRe.FindStringSubmatch(line); m != nil {
				op := strings.TrimSpace(m[1])
				sentinel := m[2]
				// Check if there's a raise in the next few lines (guard body).
				hasRaise := false
				for j := i + 1; j < i+5 && j < len(lines); j++ {
					if pyRaiseRe.MatchString(lines[j]) {
						hasRaise = true
						break
					}
				}
				if hasRaise {
					out = append(out, pyCFValidator{
						className: currentClass,
						fieldName: pendingField,
						rejectOp:  op,
						sentinel:  sentinel,
						line:      pendingLine,
					})
					pendingField = ""
				}
			}
		}
	}
	return out
}

// cfPyContradicts reports whether a field default is contradicted by a validator.
//
// The logic: if the validator rejects the sentinel value and the field's
// default IS that sentinel value, that's a contradiction.
//
// Examples:
//
//	default=0 + reject `v <= 0` → yes (0 <= 0 is true → rejected)
//	default=-1 + reject `v < 0` → yes (-1 < 0)
//	default=0 + reject `v == 0` → yes
//	default=0 + reject `v != 0` → no (validator keeps 0, rejects others)
//	default=None + reject `v is None` → yes
func cfPyContradicts(field pyCFField, val pyCFValidator) bool {
	if field.fieldName != val.fieldName || field.className != val.className {
		return false
	}
	fSentinel := field.sentinel
	vSentinel := val.sentinel
	op := val.rejectOp

	// None sentinel case.
	if fSentinel == "None" {
		return op == "is None"
	}
	if op == "is None" || op == "is not None" {
		return false // numeric default vs None check: no contradiction
	}

	fi, err1 := strconv.ParseInt(fSentinel, 10, 64)
	vi, err2 := strconv.ParseInt(vSentinel, 10, 64)
	if err1 != nil || err2 != nil {
		return false
	}

	switch op {
	case "==":
		return fi == vi
	case "<=":
		return fi <= vi
	case "<":
		return fi < vi
	case ">=":
		return fi >= vi
	case ">":
		return fi > vi
	case "!=":
		// reject v != vi: only vi passes; if default == vi, that's fine.
		// if default != vi, the default is rejected. But this is unusual.
		return fi != vi
	}
	return false
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

func seedConfigFieldPyContradictions(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	leadsPosted := 0
	seen := make(map[leadKey]bool)

	for _, f := range snap.Files {
		if f.Language != ingest.LangPython {
			continue
		}
		if isPyTestPath(f.Path) {
			continue
		}
		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		content := string(raw)

		fields := passPyCFFields(f.Path, content)
		if len(fields) == 0 {
			continue
		}
		validators := passPyCFValidators(f.Path, content)
		if len(validators) == 0 {
			continue
		}

		for _, field := range fields {
			for _, val := range validators {
				if !cfPyContradicts(field, val) {
					continue
				}
				k := leadKey{TargetLens: cfPyTargetLens, File: f.Path, Line: field.line}
				if seen[k] {
					continue
				}
				seen[k] = true
				note := fmt.Sprintf(
					"config-field-py: %s.%s has default=%s but validator at line %d rejects it (%s %s)",
					field.className, field.fieldName, field.sentinel,
					val.line, val.rejectOp, val.sentinel,
				)
				if err := st.AddLead(ctx, store.Lead{
					PosterLens: cfPyPosterLens,
					TargetLens: cfPyTargetLens,
					File:       f.Path,
					Line:       field.line,
					Note:       truncate(note, noteMaxLen),
				}); err != nil {
					return fmt.Errorf("miner: config-field-py lead %s: %w", f.Path, err)
				}
				sum.ConfigFieldPyLeads++
				sum.LeadsPosted++
				leadsPosted++
				if leadsPosted >= cfPyMaxLeads {
					return nil
				}
			}
		}
	}
	return nil
}
