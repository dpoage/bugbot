package miner

// Stringly-typed drift miner.
//
// Motivation: Go code frequently dispatches on string values that cross a
// seam — event names, command tokens, codec identifiers — and a rename on
// one side without a corresponding update on the other creates a silent
// mismatch. The classic symptom: `case "activ":` that never fires because
// the producer emits "active".
//
// Detection algorithm (precision-biased, two passes):
//
//  1. passStringConsumers: scan each Go source file for string literals in
//     `case "...":`  switch arms, grouped by the switch block they belong to.
//     Each switch block forms a "consumer family".
//
//  2. passStringProducers: scan each Go source file for string literals in
//     common producer positions (return, assignment RHS, call arguments).
//     Build a repo-global "produced" set.
//
//  3. Join — per consumer family (switch block):
//     a. Seam gate: at least one case literal in the block must appear in
//        the produced set.  Without this, the entire switch is likely reading
//        an external protocol value (OpenAI stop reasons, LSP methods, JSON
//        Schema keywords) and no drift lead is warranted.
//     b. For each case literal in the block that does NOT appear in the
//        produced set, post a type-A lead (consumed-but-never-produced).
//
// This intentionally drops type-B (produced-not-consumed) as repo-global
// scope makes it too noisy to be precision-safe in v1.
//
// Precision guards:
//   - Minimum literal length (minStringyLen = 4).
//   - Combined stoplist (genericEntityStoplist + stringyStoplist).
//   - Identifier-shape filter: literal must look like a programmatic token
//     (snake_case / camelCase / kebab-case / dotted) — not prose, not a
//     file path, not a format string.
//   - Seam gate (described above): at least 1 in-family literal must be
//     produced before any case is flagged.
//   - Dedup: at most one lead per (file, line) pair.
//
// Leads: PosterLens="miner:stringly-drift", TargetLens="api-contract-misuse".
// File/Line points at the consumer (case) site.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	stringlyPosterLens = "miner:stringly-drift"
	stringlyTargetLens = "api-contract-misuse"
	minStringyLen      = 4
)

// stringyStoplist augments the generic entity stoplist with domain words that
// appear frequently in producer positions but carry no dispatch-seam signal.
var stringyStoplist = map[string]bool{
	// HTTP methods
	"get": true, "post": true, "put": true, "delete": true, "patch": true,
	"head": true, "options": true, "connect": true, "trace": true,
	// log levels
	"debug": true, "info": true, "warn": true, "warning": true,
	"error": true, "fatal": true, "panic": true,
	// boolean-ish sentinels
	"true": true, "false": true, "yes": true,
	// common sentinel prose
	"null": true, "none": true, "both": true,
	// JSON schema primitive types — appear in both producer and consumer
	// positions across any JSON-handling code; not discriminating
	"integer": true, "number": true, "boolean": true, "object": true,
	"array": true, "string": true,
	// Code/AST entity kind names — ubiquitous in tooling code
	"function": true, "method": true, "class": true, "module": true,
	"variable": true, "constant": true, "interface": true, "struct": true,
	"field": true, "param": true, "import": true, "package": true,
	// commonly returned action/state words — too generic
	"done": true, "next": true, "stop": true, "fail": true,
	"pass": true, "skip": true, "keep": true, "drop": true,
	"read": true, "write": true, "open": true, "close": true,
	"send": true, "recv": true, "call": true, "wait": true,
	"init": true, "exit": true, "quit": true, "kill": true,
	"load": true, "save": true, "sync": true, "push": true,
	"pull": true, "poll": true, "ping": true, "pong": true,
	"auth": true, "sign": true, "hash": true, "seal": true,
	"list": true, "find": true, "scan": true, "walk": true,
	"dump": true, "view": true, "show": true, "hide": true,
	"move": true, "copy": true, "link": true, "bind": true,
	"lock": true, "free": true, "swap": true, "wrap": true,
	"join": true, "fork": true, "exec": true, "boot": true,
	"idle": true, "busy": true, "live": true,
}

// consumerSite records one string literal in a switch case arm.
type consumerSite struct {
	file    string
	line    int
	literal string
	// switchID is a synthetic identifier for the enclosing switch block,
	// used to group cases into families. It is the line number of the
	// `switch` keyword.
	switchID int
}

// producerSite records one string literal in a producer position.
type producerSite struct {
	file    string
	line    int
	literal string
}

// switchKeyRe matches the `switch` keyword that opens a switch block.
var switchKeyRe = regexp.MustCompile(`^\s*switch\b`)

// caseStringRe matches `case "...":`  (no escape complexity in the literal).
// Capture group 1 = the literal content (without quotes).
var caseStringRe = regexp.MustCompile(`\bcase\s+"([^"\\]{1,128})"`)

// producerStringRe matches string literals in common producer positions:
//
//	return "..."   = "..."   := "..."   f("...")   , "..."
var producerStringRe = regexp.MustCompile(`(?:return|=|:=|,|\()\s*"([^"\\]{1,128})"`)

// isInBlockComment reports whether line (0-indexed) in lines is inside a
// /* ... */ block comment.  We track state by scanning from the top.
// This is a best-effort approximation: it handles standard /* */ comments
// that don't span strings containing "/*" or "*/".
func buildBlockCommentMask(lines []string) []bool {
	mask := make([]bool, len(lines))
	inBlock := false
	for i, line := range lines {
		if inBlock {
			mask[i] = true
			if idx := strings.Index(line, "*/"); idx >= 0 {
				inBlock = false
			}
		} else {
			if idx := strings.Index(line, "/*"); idx >= 0 {
				// Check it's not inside a string — best-effort: count quotes before /*
				before := line[:idx]
				if strings.Count(before, `"`)%2 == 0 {
					inBlock = true
					mask[i] = true
					if strings.Contains(line[idx+2:], "*/") {
						inBlock = false
					}
				}
			}
		}
	}
	return mask
}

// isIdentifierShaped returns true when s looks like a programmatic token:
// snake_case, camelCase, kebab-case, or dot-separated (e.g. "user.created").
// It rejects prose (spaces), format strings (%), path prefixes (/),
// pure numbers, and strings that are too short or too long.
func isIdentifierShaped(s string) bool {
	if len(s) < minStringyLen || len(s) > 64 {
		return false
	}
	// Reject if it contains spaces, tabs, format verbs, newlines.
	if strings.ContainsAny(s, " \t%\r\n") {
		return false
	}
	// Reject pure numbers.
	allDigit := true
	for _, c := range s {
		if c < '0' || c > '9' {
			allDigit = false
			break
		}
	}
	if allDigit {
		return false
	}
	// Must start with a letter (not / or . or digit — those are paths/URLs/numbers).
	first := rune(s[0])
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	// Allowed interior characters: letters, digits, underscore, hyphen, dot, slash.
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// isStringyStopped returns true if s appears in either stoplist.
func isStringyStopped(s string) bool {
	lower := strings.ToLower(s)
	return genericEntityStoplist[lower] || stringyStoplist[lower]
}

// passStringConsumers extracts consumer string literals from switch case arms,
// grouped by switch block (switchID = line of the `switch` keyword).
func passStringConsumers(path, content string) []consumerSite {
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)

	var out []consumerSite
	currentSwitchID := -1

	for lineIdx, line := range lines {
		lineNo := lineIdx + 1

		// Skip block-comment lines.
		if blockMask[lineIdx] {
			continue
		}
		// Skip line comments.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		// Track switch keyword to assign switchID.
		if switchKeyRe.MatchString(line) {
			currentSwitchID = lineNo
		}

		// Extract case string literals.
		for _, m := range caseStringRe.FindAllStringSubmatch(line, -1) {
			lit := m[1]
			if !isIdentifierShaped(lit) || isStringyStopped(lit) {
				continue
			}
			id := currentSwitchID
			if id < 0 {
				id = lineNo // fallback: use the case line itself
			}
			out = append(out, consumerSite{
				file:     path,
				line:     lineNo,
				literal:  lit,
				switchID: id,
			})
		}
	}
	return out
}

// passStringProducers extracts producer string literals from Go source content.
func passStringProducers(path, content string) []producerSite {
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)

	var out []producerSite
	for lineIdx, line := range lines {
		lineNo := lineIdx + 1
		if blockMask[lineIdx] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, m := range producerStringRe.FindAllStringSubmatch(line, -1) {
			lit := m[1]
			if !isIdentifierShaped(lit) || isStringyStopped(lit) {
				continue
			}
			out = append(out, producerSite{
				file:    path,
				line:    lineNo,
				literal: lit,
			})
		}
	}
	return out
}

// seedStringlyDrift runs the stringly-typed drift pass over the snapshot and
// posts leads. Called from Seed (miner.go) after the existing passes.
func seedStringlyDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	// Phase 1: collect consumers and producers across all in-scope files.
	var consumers []consumerSite
	var producers []producerSite

	for _, f := range snap.Files {
		if !minerLang(f.Language) {
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
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		content := string(data)

		consumers = append(consumers, passStringConsumers(f.Path, content)...)
		producers = append(producers, passStringProducers(f.Path, content)...)
	}

	// Phase 2: build repo-global produced set.
	producedLiterals := make(map[string]bool, len(producers))
	for _, p := range producers {
		producedLiterals[p.literal] = true
	}

	// Phase 3: group consumers by (file, switchID) — each group is one switch block.
	type switchKey struct {
		file     string
		switchID int
	}
	type switchEntry struct {
		sites []consumerSite
	}
	// Preserve insertion order for determinism.
	switchOrder := make([]switchKey, 0)
	switchGroups := make(map[switchKey]*switchEntry)

	for _, c := range consumers {
		k := switchKey{file: c.file, switchID: c.switchID}
		if _, ok := switchGroups[k]; !ok {
			switchOrder = append(switchOrder, k)
			switchGroups[k] = &switchEntry{}
		}
		switchGroups[k].sites = append(switchGroups[k].sites, c)
	}

	// Sort switchOrder for full determinism (consumers were appended in file order,
	// but files may arrive in any order from the snapshot).
	sort.Slice(switchOrder, func(i, j int) bool {
		a, b := switchOrder[i], switchOrder[j]
		if a.file != b.file {
			return a.file < b.file
		}
		return a.switchID < b.switchID
	})

	seen := make(map[leadKey]bool)

	// Phase 4: per-switch-block join with majority seam gate.
	for _, sk := range switchOrder {
		entry := switchGroups[sk]
		sites := entry.sites

		// Seam gate: the MAJORITY of cases in this block must appear in the
		// produced set, AND at least 2 cases must be produced.
		//
		// Rationale: a switch that decodes an external protocol (OpenAI stop
		// reasons, JSON Schema types, LSP method names) will have FEW or ZERO
		// produced cases because those strings are never emitted internally.
		// A switch that dispatches on an internal enum (status strings, command
		// tokens) will have MOST of its cases produced internally — and the
		// one that isn't produced is the interesting outlier (typo / dead branch).
		//
		// This majority requirement dramatically reduces false positives on
		// protocol-decoder switches while preserving recall on internal-dispatch
		// switches.
		var producedCount, unproducedCount int
		for _, s := range sites {
			if producedLiterals[s.literal] {
				producedCount++
			} else {
				unproducedCount++
			}
		}
		// Gate: produced must strictly outnumber unproduced, and at least 2 produced.
		if producedCount < 2 || producedCount <= unproducedCount {
			continue
		}

		// Flag each case literal that is not in the produced set.
		for _, s := range sites {
			if producedLiterals[s.literal] {
				continue // consumed and produced — no drift
			}
			k := leadKey{TargetLens: stringlyTargetLens, File: s.file, Line: s.line}
			if seen[k] {
				continue
			}
			seen[k] = true

			note := fmt.Sprintf(
				"stringly-drift: case literal %q at %s:%d is consumed "+
					"but never produced anywhere in the snapshot; "+
					"likely a typo or dead branch",
				s.literal, s.file, s.line,
			)
			note = truncate(note, noteMaxLen)

			if err := st.AddLead(ctx, store.Lead{
				PosterLens: stringlyPosterLens,
				TargetLens: stringlyTargetLens,
				File:       s.file,
				Line:       s.line,
				Note:       note,
			}); err != nil {
				return fmt.Errorf("miner: stringly-drift lead %s:%d: %w", s.file, s.line, err)
			}
			sum.StringlyDriftLeads++
			sum.LeadsPosted++
			if sum.LeadsPosted >= maxLeads {
				return nil
			}
		}
	}

	return nil
}
