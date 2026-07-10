// configfield.go — config-field contract contradiction miner.
//
// Two signals not covered by the doc-sentinel-vs-validator pass:
//
//	(a) DEFAULT-vs-VALIDATION: a config/option struct field whose documented
//	    or coded default value falls outside the range its own validator
//	    enforces. Example: doc/const says "Default: 0" but the validator does
//	    `if x <= 0 { return error }`.
//
//	(b) NORMATIVE-FIELD-NEVER-READ: a struct field whose doc comment carries
//	    an explicit normative claim (must/required/always) BOUND to the field
//	    name, but the field name is never referenced outside its declaration
//	    file.
//
// Detection is purely deterministic: two regex sweeps per file, joined on the
// field entity name. Precision is the priority — a false positive is worse
// than a missed lead.
//
// Leads are posted with PosterLens="miner:config-field", TargetLens="api-contract-misuse".
package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	cfPosterLens = "miner:config-field"
	cfTargetLens = "api-contract-misuse"
	cfMaxLeads   = 200
)

// ── regex patterns ────────────────────────────────────────────────────────────

// cfStructFieldRe matches a Go struct field declaration line (indented,
// exported name). This is used ONLY inside struct-brace tracking; const/var
// blocks are excluded by the harvester logic.
// Group 1: field name (exported, PascalCase), group 2: type (optional).
var cfStructFieldRe = regexp.MustCompile(`^\s+([A-Z][A-Za-z0-9_]*)\s+\S`)

// cfDefaultValRe matches doc-comment or struct-literal patterns that set a
// default numeric value.  Covers:
//   - "Default: -1", "default 0", "defaults to 0"
//   - `Default: 0` in struct literal tags or option constructors
//   - "The default is 0"
//
// Group 1: the numeric literal (may include leading minus).
var cfDefaultValRe = regexp.MustCompile(`(?i)\bdefault(?:s?\s+(?:to|is|value|:))?\s*:?\s*(-?\d+)\b`)

// cfDefaultTagRe matches ` default:"<N>" ` struct tags.
// Group 1: the numeric literal.
var cfDefaultTagRe = regexp.MustCompile(`default:"(-?\d+)"`)

// cfValidatorRejectZeroRe matches guards that REJECT zero.
// Only: X <= 0, X == 0, 0 >= X, 0 == X
// Does NOT match X >= 0 (accept-guard) or 0 <= X (accept-guard).
var cfValidatorRejectZeroRe = regexp.MustCompile(`(?:\b|\.)([A-Z][A-Za-z0-9_]*)\s*(?:<=|==)\s*0\b|\b0\s*(?:>=|==)\s*(?:\w+\.)?([A-Z][A-Za-z0-9_]*)\b`)

// cfValidatorRejectNegRe matches guards that reject negative values.
// Handles both bare `Field < 0` and struct access `c.Field < 0`.
//
//	if x < 0 { … }
var cfValidatorRejectNegRe = regexp.MustCompile(`(?:\b|\.)([A-Z][A-Za-z0-9_]*)\s*<\s*0\b|\b0\s*>\s*(?:\w+\.)?([A-Z][A-Za-z0-9_]*)\b`)

// cfNormativeDocRe detects normative patterns BOUND to the field name.
// Accepts:
//   - "<FieldName> must ..."
//   - "must be set/provided/configured/non-empty/positive/valid/enabled"
//   - "is required" / "is mandatory"
//
// This avoids prose like "required dependencies are missing" where "required"
// describes external dependencies, not the field itself.
var cfNormativeDocRe = regexp.MustCompile(
	`(?i)(?:` +
		`\b[A-Z][A-Za-z0-9_]*\s+must\b` + // FieldName must ...
		`|must\s+be\s+(?:set|provided|configured|non-empty|positive|valid|enabled|specified)` +
		`|is\s+(?:required|mandatory)` +
		`)`)

// cfFieldAccessRe matches dotted field references: `.FieldName` or `cfg.FieldName`.
// Used for the never-read signal (dotted accesses).
var cfFieldAccessRe = regexp.MustCompile(`\.([A-Z][A-Za-z0-9_]*)`)

// cfErrReturnRe matches return statements that return an error value.
// Used by cfHasErrorReturn to distinguish error-returning guards from
// sentinel-idiom guards that just `return` (exit/skip).
var cfErrReturnRe = regexp.MustCompile(
	`\breturn\s+(?:fmt\.Errorf|errors\.New|errors\.As|errors\.Is|\w*[Ee]rr\w*\b)`)

// cfSentinelDocRe matches doc comments that document a sentinel/default
// meaning for zero (unlimited, means X, no limit, use default, disable).
var cfSentinelDocRe = regexp.MustCompile(`(?i)\b(unlimited|means\b|disable[sd]?|no\s+limit|use\s+\w+\s+default|built.in\s+default)\b`)

// ── data types ────────────────────────────────────────────────────────────────

// cfFieldDecl is a struct field declaration site with its associated doc
// comment and (optionally) coded default value.
type cfFieldDecl struct {
	name       string // exported field name
	file       string
	line       int    // 1-based line of the field declaration
	docComment string // full doc comment block (flattened)
	defaultVal *int   // numeric default if present, nil if none found
}

// cfValidatorSite is a validation site that rejects a field value.
type cfValidatorSite struct {
	fieldName   string // field name referenced in the condition
	file        string
	line        int
	rejectsZero bool // true if the guard rejects zero / non-positive
	rejectsNeg  bool // true if the guard rejects negative (< 0)
	snippet     string
}

// ── main entry point ─────────────────────────────────────────────────────────

// seedConfigFieldContradictions is called from Seed after the existing passes.
// It posts leads for config-field contract violations.
func seedConfigFieldContradictions(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	if snap == nil || st == nil {
		return nil
	}

	var posted int

	// Collect all field declarations and validator sites across the snapshot.
	// Also collect a "reference set" per file for the never-read signal.
	allDecls := make(map[string][]cfFieldDecl)    // file → decls
	allVals := make(map[string][]cfValidatorSite) // fieldName → sites

	// referenceSet: fieldName → set of files that reference it
	// Counts both dotted (.FieldName) and bare-identifier references so
	// package-level consts referenced in assignments/comparisons are not
	// falsely flagged.
	referenceSet := make(map[string]map[string]bool)

	for _, f := range snap.Files {
		if f.Language != ingest.LangGo {
			continue
		}
		// Skip test files: embedded inline fixtures cause FPs because
		// string literals containing Go source look like real declarations.
		if strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		path := filepath.Join(snap.Root, f.Path)
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) > maxFileBytes {
			continue
		}
		content := string(raw)
		lines := strings.Split(content, "\n")

		decls := cfPassFieldDecls(f.Path, lines)
		vals := cfPassValidators(f.Path, lines)

		allDecls[f.Path] = decls
		for _, v := range vals {
			allVals[v.fieldName] = append(allVals[v.fieldName], v)
		}

		// Harvest dotted references: .FieldName
		for _, m := range cfFieldAccessRe.FindAllStringSubmatch(content, -1) {
			name := m[1]
			if !isPlausibleEntity(name) {
				continue
			}
			if referenceSet[name] == nil {
				referenceSet[name] = make(map[string]bool)
			}
			referenceSet[name][f.Path] = true
		}

	}

	seen := make(map[leadKey]bool)

	// Signal (a): DEFAULT-vs-VALIDATION contradiction.
	for _, decls := range allDecls {
		for _, d := range decls {
			if !isPlausibleEntity(d.name) {
				continue
			}
			if d.defaultVal == nil {
				continue
			}
			dv := *d.defaultVal
			validators, ok := allVals[d.name]
			if !ok {
				continue
			}
			for _, v := range validators {
				contradiction := false
				reason := ""

				// default 0, but validator rejects zero/non-positive
				if dv == 0 && v.rejectsZero {
					contradiction = true
					reason = fmt.Sprintf("default value is 0 but validator rejects zero/non-positive (%s)", v.snippet)
				}
				// default < 0, but validator rejects negative
				if dv < 0 && v.rejectsNeg {
					contradiction = true
					reason = fmt.Sprintf("default value is %d but validator rejects negative (%s)", dv, v.snippet)
				}
				// default < 0, but validator rejects zero/non-positive
				if dv < 0 && v.rejectsZero {
					contradiction = true
					reason = fmt.Sprintf("default value is %d but validator rejects non-positive (%s)", dv, v.snippet)
				}

				if !contradiction {
					continue
				}

				k := leadKey{TargetLens: cfTargetLens, File: d.file, Line: d.line}
				if seen[k] {
					continue
				}
				seen[k] = true

				docSnip := truncate(strings.ReplaceAll(d.docComment, "\n", " "), 200)
				note := fmt.Sprintf("config-field %s: %s; doc/default: %s", d.name, reason, docSnip)
				note = truncate(strings.ReplaceAll(note, "\n", " "), noteMaxLen)

				if err := st.AddLead(ctx, store.Lead{
					PosterLens: cfPosterLens,
					TargetLens: cfTargetLens,
					File:       d.file,
					Line:       d.line,
					Note:       note,
				}); err != nil {
					return fmt.Errorf("miner: config-field lead %s:%d: %w", d.file, d.line, err)
				}
				sum.LeadsPosted++
				posted++
				if posted >= cfMaxLeads {
					return nil
				}
			}
		}
	}

	// Signal (b): NORMATIVE-FIELD-NEVER-READ.
	for _, decls := range allDecls {
		for _, d := range decls {
			if !isPlausibleEntity(d.name) {
				continue
			}
			if d.docComment == "" {
				continue
			}
			// Precision guard: normative word must BIND to the field in the
			// first doc line (not just appear anywhere in prose).
			firstDocLine := d.docComment
			if nl := strings.Index(d.docComment, "\n"); nl >= 0 {
				firstDocLine = d.docComment[:nl]
			}
			if !cfNormativeDocRe.MatchString(firstDocLine) {
				continue
			}
			// Precision guard: skip if the field is accessed anywhere at all
			// (dotted OR bare identifier references).
			if len(referenceSet[d.name]) > 0 {
				continue
			}

			k := leadKey{TargetLens: cfTargetLens, File: d.file, Line: d.line}
			if seen[k] {
				continue
			}
			seen[k] = true

			docSnip := truncate(strings.ReplaceAll(d.docComment, "\n", " "), 200)
			note := fmt.Sprintf("config-field %s: normative doc (%s) but field is never read outside declaration; doc: %s",
				d.name, cfNormativeNormWords(d.docComment), docSnip)
			note = truncate(strings.ReplaceAll(note, "\n", " "), noteMaxLen)

			if err := st.AddLead(ctx, store.Lead{
				PosterLens: cfPosterLens,
				TargetLens: cfTargetLens,
				File:       d.file,
				Line:       d.line,
				Note:       note,
			}); err != nil {
				return fmt.Errorf("miner: config-field lead %s:%d: %w", d.file, d.line, err)
			}
			sum.LeadsPosted++
			posted++
			if posted >= cfMaxLeads {
				return nil
			}
		}
	}

	return nil
}

// ── passes ────────────────────────────────────────────────────────────────────

// cfPassFieldDecls sweeps lines for exported struct field declarations and
// their associated doc comments / default values.
//
// Only lines INSIDE a `type X struct { ... }` block are harvested.
// Lines inside const(...) or var(...) blocks are excluded so that const enum
// values (e.g. SmokeCategory const iota entries) are never treated as fields.
func cfPassFieldDecls(filePath string, lines []string) []cfFieldDecl {
	var out []cfFieldDecl

	// Track whether we are inside a struct body.
	// structDepth > 0 means we're inside at least one struct's braces.
	// We use a simple brace counter gated on seeing `type ... struct {`.
	inStruct := false
	structBraceDepth := 0

	// Track const/var block depth to skip those sections.
	inConstVar := false
	constVarParenDepth := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// ── const/var block tracking ──
		if !inConstVar {
			if strings.HasPrefix(trimmed, "const (") || strings.HasPrefix(trimmed, "var (") ||
				trimmed == "const(" || trimmed == "var(" {
				inConstVar = true
				constVarParenDepth = 1
				continue
			}
		} else {
			for _, ch := range trimmed {
				if ch == '(' {
					constVarParenDepth++
				} else if ch == ')' {
					constVarParenDepth--
					if constVarParenDepth <= 0 {
						inConstVar = false
						constVarParenDepth = 0
						break
					}
				}
			}
			continue // skip all lines inside const/var blocks
		}

		// ── struct body tracking ──
		// Detect `type Foo struct {`
		if strings.Contains(trimmed, "struct {") &&
			strings.HasPrefix(trimmed, "type ") {
			inStruct = true
			structBraceDepth = 1
			continue
		}
		// Detect `} struct {` (embedded struct in struct — simple handling)
		if inStruct {
			for _, ch := range trimmed {
				if ch == '{' {
					structBraceDepth++
				} else if ch == '}' {
					structBraceDepth--
					if structBraceDepth <= 0 {
						inStruct = false
						structBraceDepth = 0
					}
				}
			}
			if !inStruct {
				continue
			}
		}

		if !inStruct {
			continue
		}

		m := cfStructFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if !isPlausibleEntity(name) {
			continue
		}

		// Collect preceding doc comment block (contiguous // lines above).
		var commentLines []string
		for j := i - 1; j >= 0; j-- {
			tr := strings.TrimSpace(lines[j])
			if strings.HasPrefix(tr, "//") {
				commentLines = append([]string{stripComment(tr)}, commentLines...)
			} else {
				break
			}
		}
		docComment := strings.Join(commentLines, "\n")

		// Look for a default value in the doc comment or on the field line itself.
		var defaultVal *int
		combined := docComment + "\n" + line

		// Check struct tag default:"N"
		if tm := cfDefaultTagRe.FindStringSubmatch(line); tm != nil {
			v := parseInt(tm[1])
			if v != nil {
				defaultVal = v
			}
		}
		// Check doc/comment default phrase
		if defaultVal == nil {
			if dm := cfDefaultValRe.FindStringSubmatch(combined); dm != nil {
				v := parseInt(dm[1])
				if v != nil {
					defaultVal = v
				}
			}
		}

		out = append(out, cfFieldDecl{
			name:       name,
			file:       filePath,
			line:       i + 1,
			docComment: docComment,
			defaultVal: defaultVal,
		})
	}
	return out
}

// cfPassValidators sweeps lines for guard conditions that reject exported
// field values.
//
// Signal (a) gate: only count a guard as a rejection if the surrounding block
// contains an error-returning statement (fmt.Errorf, errors.New, an *Err*
// variable). A bare `return` without error (sentinel idiom like `if x <= 0 {
// return }` with doc "0 means default") is NOT a rejection.
//
// Sentinel-doc gate: if the field's doc marks zero as a sentinel value
// (contains "unlimited"/"means"/"no limit"/"use ... default"/"disable"), skip.
func cfPassValidators(filePath string, lines []string) []cfValidatorSite {
	var out []cfValidatorSite

	for i, line := range lines {
		// Must look like a guard: if ... { (with early return/error implied)
		if !strings.Contains(line, "if ") {
			continue
		}

		// Reject zero / non-positive: FieldName <= 0, FieldName == 0, 0 >= FieldName, 0 == FieldName
		for _, m := range cfValidatorRejectZeroRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if !isPlausibleEntity(name) {
				continue
			}
			// Must be followed by an ERROR-returning statement (not a bare return).
			if !cfHasErrorReturn(lines, i) {
				continue
			}
			// Skip if doc marks zero as a valid sentinel.
			if cfIsSentinelDoc(lines, i) {
				continue
			}
			out = append(out, cfValidatorSite{
				fieldName:   name,
				file:        filePath,
				line:        i + 1,
				rejectsZero: true,
				snippet:     truncate(strings.TrimSpace(line), 120),
			})
		}

		// Reject negative: FieldName < 0
		for _, m := range cfValidatorRejectNegRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if !isPlausibleEntity(name) {
				continue
			}
			if !cfHasErrorReturn(lines, i) {
				continue
			}
			if cfIsSentinelDoc(lines, i) {
				continue
			}
			// Only add if not already captured by rejectsZero
			// (avoid duplicate entries for the same line).
			dup := false
			for _, existing := range out {
				if existing.file == filePath && existing.line == i+1 && existing.fieldName == name {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, cfValidatorSite{
					fieldName:  name,
					file:       filePath,
					line:       i + 1,
					rejectsNeg: true,
					snippet:    truncate(strings.TrimSpace(line), 120),
				})
			}
		}
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

// parseInt parses a string as an int, returning nil on failure.
func parseInt(s string) *int {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return nil
	}
	return &v
}

// cfNormativeNormWords extracts normative keywords found in s.
func cfNormativeNormWords(s string) string {
	var found []string
	seen := make(map[string]bool)
	for _, m := range cfNormativeDocRe.FindAllString(s, -1) {
		low := strings.ToLower(m)
		if !seen[low] {
			seen[low] = true
			found = append(found, m)
		}
	}
	return strings.Join(found, ", ")
}

// cfHasErrorReturn checks whether the block starting at `from` contains an
// error-returning return statement within the next 6 lines.
// A bare `return` or `return nil` does NOT count — those are sentinel idioms.
func cfHasErrorReturn(lines []string, from int) bool {
	end := from + 6
	if end > len(lines) {
		end = len(lines)
	}
	for _, l := range lines[from:end] {
		t := strings.TrimSpace(l)
		if cfErrReturnRe.MatchString(t) {
			return true
		}
		// panic / os.Exit also count as hard rejections
		if strings.HasPrefix(t, "panic(") || strings.HasPrefix(t, "os.Exit(") {
			return true
		}
	}
	return false
}

// cfIsSentinelDoc checks whether the doc comment lines immediately above
// the guard at `guardLine` contain a sentinel-value marker (unlimited, means,
// no limit, etc.). This suppresses FPs on the `0 = use default` idiom.
func cfIsSentinelDoc(lines []string, guardLine int) bool {
	// Search upward for any comment line near the guard (within 10 lines).
	start := guardLine - 10
	if start < 0 {
		start = 0
	}
	for _, l := range lines[start:guardLine] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "//") && cfSentinelDocRe.MatchString(t) {
			return true
		}
	}
	return false
}
