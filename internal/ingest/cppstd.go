package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// cppMaxCompileCommandsBytes caps how many bytes we read from compile_commands.json
// before giving up. Compile-command databases can be large; we parse them for
// -std= flags only, so reading a few MB is sufficient for any realistic project.
const cppMaxCompileCommandsBytes = 4 << 20 // 4 MiB

// validCPPStandards is the ordered set of recognised C++ standard year suffixes
// (NN in -std=c++NN / -std=gnu++NN). Order matters only for presentation; the
// canonical string is built as "C++NN".
var validCPPStandards = map[string]bool{
	"98": true, "03": true, "11": true, "14": true,
	"17": true, "20": true, "23": true, "26": true,
}

// stdFlagRE matches -std=c++NN and -std=gnu++NN in a compile flags string.
// Capturing group 1 is the NN suffix (two digits).
var stdFlagRE = regexp.MustCompile(`-std=(?:gnu|c)\+\+(\d{2})`)

// cmakeSetRE matches `set(CMAKE_CXX_STANDARD NN)` with optional surrounding
// whitespace and case-insensitive keyword. Capturing group 1 is NN.
var cmakeSetRE = regexp.MustCompile(`(?i)set\s*\(\s*CMAKE_CXX_STANDARD\s+(\d+)\s*\)`)

// cmakeFeaturesRE matches `target_compile_features(... cxx_std_NN ...)`.
// Capturing group 1 is NN.
var cmakeFeaturesRE = regexp.MustCompile(`cxx_std_(\d+)`)

// mesonCPPStdRE matches `cpp_std=c++NN` and `cpp_std=gnu++NN` inside
// meson.build option dictionaries. Capturing group 1 is NN.
var mesonCPPStdRE = regexp.MustCompile(`cpp_std\s*=\s*(?:gnu|c)\+\+(\d{2,})`)

// makeCXXFlagsRE matches -std=c++NN / -std=gnu++NN on lines that look like
// CXXFLAGS assignments in a Makefile. We do not attempt to evaluate make
// variables; this is a best-effort grep. Capturing group 1 is NN.
var makeCXXFlagsRE = stdFlagRE // same pattern; aliased for clarity

// DetectCPPStandard probes repoDir for the C++ language standard the project
// targets and returns a canonical string like "C++20" when found.
//
// Detection sources, in confidence order (first source that yields a standard
// wins):
//
//  1. compile_commands.json at root or root/build — parse the JSON array,
//     extract -std=c++NN / -std=gnu++NN from "command"/"arguments" fields.
//     Multiple translation units may disagree: take the most-common value; ties
//     are broken by choosing the higher standard (more expressive wins).
//     gnu++NN normalises to C++NN.
//
//  2. CMakeLists.txt — `set(CMAKE_CXX_STANDARD NN)` (whitespace/case tolerant)
//     and `target_compile_features(... cxx_std_NN ...)`.
//
//  3. meson.build — `cpp_std=c++NN` or `cpp_std=gnu++NN`.
//
//  4. Makefile / GNUmakefile — best-effort grep for -std=c++NN in CXXFLAGS-ish
//     lines.
//
// If no source yields a recognised standard, ok is false and the caller should
// treat the C++ standard as unknown (no qualifier).
//
// Pure stdlib; does not recurse the file tree (root and root/build only for
// compile_commands.json).
func DetectCPPStandard(repoDir string) (standard string, ok bool) {
	// 1. compile_commands.json.
	if s, found := detectFromCompileCommands(repoDir); found {
		return s, true
	}

	// 2. CMakeLists.txt.
	if s, found := detectFromCMake(repoDir); found {
		return s, true
	}

	// 3. meson.build.
	if s, found := detectFromMeson(repoDir); found {
		return s, true
	}

	// 4. Makefile / GNUmakefile.
	if s, found := detectFromMakefile(repoDir); found {
		return s, true
	}

	return "", false
}

// detectFromCompileCommands reads compile_commands.json from repoDir or
// repoDir/build, parses it, and returns the most-common C++ standard. On a
// tie the higher standard wins. Malformed JSON or absent file falls through
// (returns "", false) so the caller can try the next source.
func detectFromCompileCommands(repoDir string) (string, bool) {
	for _, subdir := range []string{"", "build"} {
		path := filepath.Join(repoDir, subdir, "compile_commands.json")
		if s, found := parseCompileCommands(path); found {
			return s, true
		}
	}
	return "", false
}

// compileCommand is the subset of each entry in compile_commands.json that we
// care about. Both "command" (a shell string) and "arguments" (a JSON array of
// strings) are optional; we accept whichever is present.
type compileCommand struct {
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

// parseCompileCommands reads path, caps at cppMaxCompileCommandsBytes, and
// extracts the most-common C++ standard from -std= flags. Returns "", false on
// any error or if no standard is found (so callers fall through cleanly).
func parseCompileCommands(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	// Read at most cppMaxCompileCommandsBytes to bound memory use on huge DBs.
	buf := make([]byte, cppMaxCompileCommandsBytes)
	n, _ := f.Read(buf)
	if n == 0 {
		return "", false
	}
	data := buf[:n]

	var cmds []compileCommand
	if err := json.Unmarshal(data, &cmds); err != nil {
		// Malformed JSON (or truncated because we capped the read): fall through
		// to the next detection source rather than returning an error.
		return "", false
	}

	// Count occurrences of each NN suffix across all TUs.
	counts := make(map[string]int)
	for _, cmd := range cmds {
		// Prefer "arguments" (already split); fall back to "command" (shell string).
		var nn string
		if len(cmd.Arguments) > 0 {
			for _, arg := range cmd.Arguments {
				if m := stdFlagRE.FindStringSubmatch(arg); m != nil {
					if validCPPStandards[m[1]] {
						nn = m[1]
						break
					}
				}
			}
		}
		if nn == "" && cmd.Command != "" {
			if m := stdFlagRE.FindStringSubmatch(cmd.Command); m != nil && validCPPStandards[m[1]] {
				nn = m[1]
			}
		}
		if nn != "" {
			counts[nn]++
		}
	}
	if len(counts) == 0 {
		return "", false
	}

	return pickStandard(counts), true
}

// pickStandard selects the most-common NN suffix from counts. Ties are broken
// by choosing the higher standard (more expressive wins). The returned string
// is in canonical "C++NN" form.
//
// Tie-break rationale: when a project mixes standards across TUs (e.g. some
// C files compile with c++17 and most with c++20), the higher standard is the
// one the project is actually targeting for its non-legacy code, and it is the
// standard most relevant to a reviewer's instincts.
func pickStandard(counts map[string]int) string {
	type entry struct {
		nn    string
		count int
	}

	entries := make([]entry, 0, len(counts))
	for nn, c := range counts {
		entries = append(entries, entry{nn, c})
	}
	// Sort descending by count, then descending by standard order (higher standard
	// wins on tie). Using sort.Slice is fine; the slice has at most 8 elements.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		// Higher chronological standard wins; use standardOrder to avoid the
		// "98" > "03" numeric trap (C++2003 is newer than C++1998).
		return standardOrder[entries[i].nn] > standardOrder[entries[j].nn]
	})
	return "C++" + entries[0].nn
}

// standardOrder maps each two-digit year suffix to a monotonically increasing
// integer representing its chronological position in the C++ standard sequence.
// We cannot use the raw two-digit value because "98" (C++1998) is numerically
// larger than "03" (C++2003) yet chronologically older. An explicit rank avoids
// that ambiguity and is forward-safe: adding new entries (e.g. "29") is trivial.
var standardOrder = map[string]int{
	"98": 1,
	"03": 2,
	"11": 3,
	"14": 4,
	"17": 5,
	"20": 6,
	"23": 7,
	"26": 8,
}

// detectFromCMake reads CMakeLists.txt in repoDir and extracts the C++ standard
// from set(CMAKE_CXX_STANDARD ...) or target_compile_features(... cxx_std_NN ...).
func detectFromCMake(repoDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(repoDir, "CMakeLists.txt"))
	if err != nil {
		return "", false
	}
	content := string(data)

	// set(CMAKE_CXX_STANDARD NN) takes priority within this source.
	if m := cmakeSetRE.FindStringSubmatch(content); m != nil {
		if s, ok := canonicalStandard(m[1]); ok {
			return s, true
		}
	}

	// target_compile_features(... cxx_std_NN ...).
	if m := cmakeFeaturesRE.FindStringSubmatch(content); m != nil {
		if s, ok := canonicalStandard(m[1]); ok {
			return s, true
		}
	}

	return "", false
}

// detectFromMeson reads meson.build in repoDir and extracts the C++ standard
// from `cpp_std = c++NN` or `cpp_std = gnu++NN`.
func detectFromMeson(repoDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(repoDir, "meson.build"))
	if err != nil {
		return "", false
	}

	if m := mesonCPPStdRE.FindStringSubmatch(string(data)); m != nil {
		// Normalise: meson uses two or more digits (e.g. "c++20", "c++2a" is
		// non-standard but we only recognise two-digit NN).
		nn := m[1]
		if len(nn) == 2 {
			if s, ok := canonicalStandard(nn); ok {
				return s, true
			}
		}
	}
	return "", false
}

// detectFromMakefile reads Makefile or GNUmakefile in repoDir and extracts
// a C++ standard from -std=c++NN / -std=gnu++NN flags on CXXFLAGS-ish lines.
// This is best-effort: make variables are not expanded.
func detectFromMakefile(repoDir string) (string, bool) {
	for _, name := range []string{"Makefile", "GNUmakefile"} {
		data, err := os.ReadFile(filepath.Join(repoDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			// Only inspect lines that look like flag assignments to avoid matching
			// stray -std= occurrences in comments or recipe bodies unrelated to CXX.
			upper := strings.ToUpper(line)
			if !strings.Contains(upper, "CXXFLAGS") && !strings.Contains(upper, "CXX_FLAGS") && !strings.Contains(upper, "CPPFLAGS") {
				continue
			}
			if m := makeCXXFlagsRE.FindStringSubmatch(line); m != nil {
				if s, ok := canonicalStandard(m[1]); ok {
					return s, true
				}
			}
		}
	}
	return "", false
}

// canonicalStandard converts a raw NN (or NNN) year suffix string into the
// canonical "C++NN" form if it is a recognised standard. Returns "", false for
// unrecognised values.
func canonicalStandard(nn string) (string, bool) {
	// Normalise: strip a leading zero on single-digit-seeming values (shouldn't
	// happen in practice but be defensive). We keep two-digit form for display.
	if len(nn) > 2 {
		// Three-or-more-digit values (e.g. "2a", "202x") are not in our vocabulary.
		return "", false
	}
	if validCPPStandards[nn] {
		return "C++" + nn, true
	}
	return "", false
}
