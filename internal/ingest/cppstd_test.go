package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempFile creates a file at dir/name with the given content.
func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeTempFile %s: %v", name, err)
	}
}

// writeTempSubfile creates dir/subdir/name with the given content,
// creating the subdirectory if necessary.
func writeTempSubfile(t *testing.T, dir, subdir, name, content string) {
	t.Helper()
	sub := filepath.Join(dir, subdir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}
	if err := os.WriteFile(filepath.Join(sub, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeTempSubfile %s/%s: %v", subdir, name, err)
	}
}

// ---------------------------------------------------------------------------
// DetectCPPStandard: single-source cases
// ---------------------------------------------------------------------------

func TestDetectCPPStandard_CompileCommandsRoot(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=c++20 -o foo.o foo.cpp", "file": "foo.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("got %q, want %q", got, "C++20")
	}
}

func TestDetectCPPStandard_CompileCommandsBuildSubdir(t *testing.T) {
	dir := t.TempDir()
	writeTempSubfile(t, dir, "build", "compile_commands.json", `[
		{"command": "g++ -std=c++17 -o bar.o bar.cpp", "file": "bar.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("got %q, want %q", got, "C++17")
	}
}

func TestDetectCPPStandard_CompileCommandsArguments(t *testing.T) {
	// "arguments" array form (preferred over "command" string).
	dir := t.TempDir()
	writeTempFile(t, dir, "compile_commands.json", `[
		{"arguments": ["g++", "-std=c++14", "-o", "baz.o", "baz.cpp"], "file": "baz.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++14" {
		t.Errorf("got %q, want %q", got, "C++14")
	}
}

func TestDetectCPPStandard_GnuPlusPlusNormalisesToCPP(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=gnu++17 -o a.o a.cpp", "file": "a.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("gnu++17 should normalise to C++17, got %q", got)
	}
}

func TestDetectCPPStandard_CMakeSet(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "CMakeLists.txt",
		"cmake_minimum_required(VERSION 3.14)\nset(CMAKE_CXX_STANDARD 20)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("got %q, want %q", got, "C++20")
	}
}

func TestDetectCPPStandard_CMakeSetCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	// CMake is case-insensitive; some projects use lowercase keywords.
	writeTempFile(t, dir, "CMakeLists.txt",
		"SET(cmake_cxx_standard 17)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("got %q, want %q", got, "C++17")
	}
}

func TestDetectCPPStandard_CMakeTargetCompileFeatures(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "CMakeLists.txt",
		"target_compile_features(mylib PUBLIC cxx_std_17)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("got %q, want %q", got, "C++17")
	}
}

func TestDetectCPPStandard_Meson(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "meson.build",
		"project('myproject', 'cpp', default_options: ['cpp_std=c++20'])\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("got %q, want %q", got, "C++20")
	}
}

func TestDetectCPPStandard_MesonGnuPlusPlus(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "meson.build",
		"project('x', 'cpp', default_options: ['cpp_std=gnu++17'])\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("gnu++17 in meson should normalise to C++17, got %q", got)
	}
}

func TestDetectCPPStandard_Makefile(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "Makefile",
		"CXXFLAGS = -Wall -std=c++14 -O2\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++14" {
		t.Errorf("got %q, want %q", got, "C++14")
	}
}

func TestDetectCPPStandard_GNUMakefile(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "GNUmakefile",
		"override CXXFLAGS += -std=c++11\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++11" {
		t.Errorf("got %q, want %q", got, "C++11")
	}
}

// ---------------------------------------------------------------------------
// DetectCPPStandard: confidence order
// ---------------------------------------------------------------------------

// TestDetectCPPStandard_ConfidenceOrder_CompileCommandsBeats verifies that
// compile_commands.json (source 1) wins over CMakeLists.txt (source 2) when
// both are present and they disagree.
func TestDetectCPPStandard_ConfidenceOrder_CompileCommandsBeats(t *testing.T) {
	dir := t.TempDir()
	// compile_commands says C++20.
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=c++20 foo.cpp"}
	]`)
	// CMakeLists says C++11 — should be ignored.
	writeTempFile(t, dir, "CMakeLists.txt", "set(CMAKE_CXX_STANDARD 11)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("compile_commands should beat CMakeLists; got %q", got)
	}
}

// TestDetectCPPStandard_ConfidenceOrder_CMakeBeatesMeson verifies that
// CMakeLists.txt (source 2) wins over meson.build (source 3).
func TestDetectCPPStandard_ConfidenceOrder_CMakeBeatesMeson(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "CMakeLists.txt", "set(CMAKE_CXX_STANDARD 17)\n")
	writeTempFile(t, dir, "meson.build", "project('x', 'cpp', default_options: ['cpp_std=c++11'])\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++17" {
		t.Errorf("CMakeLists should beat meson.build; got %q", got)
	}
}

// TestDetectCPPStandard_ConfidenceOrder_MesonBeatesMakefile verifies that
// meson.build (source 3) wins over Makefile (source 4).
func TestDetectCPPStandard_ConfidenceOrder_MesonBeatesMakefile(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "meson.build", "project('x', 'cpp', default_options: ['cpp_std=c++20'])\n")
	writeTempFile(t, dir, "Makefile", "CXXFLAGS = -std=c++11\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("meson.build should beat Makefile; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// DetectCPPStandard: most-common and tie-break rules within compile_commands
// ---------------------------------------------------------------------------

// TestDetectCPPStandard_MostCommon verifies that the majority standard wins
// when multiple TUs appear in compile_commands.json.
func TestDetectCPPStandard_MostCommon(t *testing.T) {
	dir := t.TempDir()
	// Three TUs with C++20, one with C++17.
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=c++20 a.cpp"},
		{"command": "g++ -std=c++20 b.cpp"},
		{"command": "g++ -std=c++20 c.cpp"},
		{"command": "g++ -std=c++17 d.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("most-common (3×C++20 vs 1×C++17) should yield C++20; got %q", got)
	}
}

// TestDetectCPPStandard_TieBrokenByHigher verifies that on a tie the higher
// standard is chosen, as documented in DetectCPPStandard.
func TestDetectCPPStandard_TieBrokenByHigher(t *testing.T) {
	dir := t.TempDir()
	// Equal counts: one TU C++17, one TU C++20.
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=c++17 legacy.cpp"},
		{"command": "g++ -std=c++20 modern.cpp"}
	]`)

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "C++20" {
		t.Errorf("tie should be broken by higher standard (C++20 > C++17); got %q", got)
	}
}

// ---------------------------------------------------------------------------
// DetectCPPStandard: nothing present → ok=false
// ---------------------------------------------------------------------------

func TestDetectCPPStandard_NothingPresent(t *testing.T) {
	dir := t.TempDir() // completely empty
	_, ok := DetectCPPStandard(dir)
	if ok {
		t.Error("expected ok=false for empty directory")
	}
}

func TestDetectCPPStandard_NoBuildFilesWithCPPStd(t *testing.T) {
	dir := t.TempDir()
	// Has source files but no build system files with -std flags.
	writeTempFile(t, dir, "main.cpp", "int main() {}\n")
	writeTempFile(t, dir, "CMakeLists.txt", "cmake_minimum_required(VERSION 3.10)\n")

	_, ok := DetectCPPStandard(dir)
	if ok {
		t.Error("expected ok=false when no standard is declared")
	}
}

// ---------------------------------------------------------------------------
// DetectCPPStandard: malformed compile_commands.json falls through
// ---------------------------------------------------------------------------

// TestDetectCPPStandard_MalformedCompileCommands verifies that a malformed
// compile_commands.json does not panic and falls through to the next source.
func TestDetectCPPStandard_MalformedCompileCommands(t *testing.T) {
	dir := t.TempDir()
	// Write a truncated / malformed JSON file.
	writeTempFile(t, dir, "compile_commands.json", `[{"command": "g++ -std=c++20 a.cpp"`)

	// Provide CMakeLists.txt as a fallback to confirm fall-through happened.
	writeTempFile(t, dir, "CMakeLists.txt", "set(CMAKE_CXX_STANDARD 17)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true (fallback to CMakeLists.txt should succeed)")
	}
	if got != "C++17" {
		t.Errorf("expected fallback to CMakeLists to yield C++17; got %q", got)
	}
}

// TestDetectCPPStandard_EmptyCompileCommands verifies that an empty JSON array
// (no TUs) falls through cleanly.
func TestDetectCPPStandard_EmptyCompileCommands(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "compile_commands.json", `[]`)
	writeTempFile(t, dir, "CMakeLists.txt", "set(CMAKE_CXX_STANDARD 14)\n")

	got, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true (fallback to CMakeLists)")
	}
	if got != "C++14" {
		t.Errorf("got %q, want C++14", got)
	}
}

// ---------------------------------------------------------------------------
// pickStandard: tie-break and most-common unit tests
// ---------------------------------------------------------------------------

func TestPickStandard_MostCommon(t *testing.T) {
	counts := map[string]int{"17": 5, "20": 2, "14": 1}
	got := pickStandard(counts)
	if got != "C++17" {
		t.Errorf("pickStandard: got %q, want C++17", got)
	}
}

func TestPickStandard_TieHigherWins(t *testing.T) {
	counts := map[string]int{"17": 3, "20": 3}
	got := pickStandard(counts)
	if got != "C++20" {
		t.Errorf("pickStandard tie: got %q, want C++20 (higher standard)", got)
	}
}

func TestPickStandard_C98VsC03TieHigherWins(t *testing.T) {
	// 03 > 98 numerically (2003 > 1998), so C++03 is the "higher" standard.
	counts := map[string]int{"98": 1, "03": 1}
	got := pickStandard(counts)
	if got != "C++03" {
		t.Errorf("pickStandard C++98 vs C++03 tie: got %q, want C++03", got)
	}
}

// ---------------------------------------------------------------------------
// Determinism: same repo → same standard, no map-iteration-order leakage
// ---------------------------------------------------------------------------

func TestDetectCPPStandard_Deterministic(t *testing.T) {
	dir := t.TempDir()
	// Mixed TUs: more C++20 than anything else.
	writeTempFile(t, dir, "compile_commands.json", `[
		{"command": "g++ -std=c++20 a.cpp"},
		{"command": "g++ -std=c++20 b.cpp"},
		{"command": "g++ -std=c++17 c.cpp"}
	]`)

	first, ok := DetectCPPStandard(dir)
	if !ok {
		t.Fatal("expected ok=true")
	}
	for i := 0; i < 50; i++ {
		got, ok := DetectCPPStandard(dir)
		if !ok {
			t.Fatalf("iteration %d: expected ok=true", i)
		}
		if got != first {
			t.Fatalf("non-deterministic: iteration %d got %q, first was %q", i, got, first)
		}
	}
}
