package ingest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// snapFromLangs builds a Snapshot whose Files have the given languages (paths
// are synthetic and irrelevant to language profiling, which reads File.Language
// directly).
func snapFromLangs(langs ...Language) *Snapshot {
	files := make([]File, len(langs))
	for i, l := range langs {
		files[i] = File{Path: "f", Language: l}
	}
	return &Snapshot{Files: files}
}

func TestDominantLanguages(t *testing.T) {
	tests := []struct {
		name string
		snap *Snapshot
		want []Language
	}{
		{
			name: "pure Go",
			snap: snapFromLangs(LangGo, LangGo, LangGo),
			want: []Language{LangGo},
		},
		{
			name: "Go plus Python, Go dominant",
			snap: snapFromLangs(LangGo, LangGo, LangGo, LangPython),
			want: []Language{LangGo, LangPython},
		},
		{
			name: "pure Python",
			snap: snapFromLangs(LangPython, LangPython),
			want: []Language{LangPython},
		},
		{
			name: "empty snapshot",
			snap: snapFromLangs(),
			want: nil,
		},
		{
			name: "nil snapshot",
			snap: nil,
			want: nil,
		},
		{
			name: "only LangOther is excluded as noise",
			snap: snapFromLangs(LangOther, LangOther),
			want: nil,
		},
		{
			name: "LangShell excluded as noise",
			snap: snapFromLangs(LangShell, LangShell, LangGo),
			want: []Language{LangGo},
		},
		{
			name: "noise excluded, source kept",
			snap: snapFromLangs(LangOther, LangShell, LangPython, LangPython),
			want: []Language{LangPython},
		},
		{
			name: "capped at top three by count",
			snap: snapFromLangs(
				LangGo, LangGo, LangGo, LangGo, // 4
				LangPython, LangPython, LangPython, // 3
				LangRust, LangRust, // 2
				LangJava, // 1 — dropped by the cap
			),
			want: []Language{LangGo, LangPython, LangRust},
		},
		{
			name: "tie broken deterministically by language string",
			snap: snapFromLangs(LangRust, LangGo), // equal counts (1 each)
			want: []Language{LangGo, LangRust},    // "go" < "rust"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DominantLanguages(tt.snap)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DominantLanguages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDominantLanguagesDeterministic pins that the ordering is stable across
// repeated calls regardless of map iteration order.
func TestDominantLanguagesDeterministic(t *testing.T) {
	snap := snapFromLangs(LangRust, LangPython, LangGo, LangPython, LangGo, LangGo)
	first := DominantLanguages(snap)
	for i := 0; i < 50; i++ {
		got := DominantLanguages(snap)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("non-deterministic ordering: %v != %v", got, first)
		}
	}
	want := []Language{LangGo, LangPython, LangRust} // 3, 2, 1
	if !reflect.DeepEqual(first, want) {
		t.Errorf("DominantLanguages() = %v, want %v", first, want)
	}
}

func TestPersonaLanguages(t *testing.T) {
	tests := []struct {
		name  string
		langs []Language
		want  string
	}{
		{"empty", nil, "senior software engineer"},
		{"single Go", []Language{LangGo}, "senior Go engineer"},
		{"single Python", []Language{LangPython}, "senior Python engineer"},
		{"single Elixir", []Language{LangElixir}, "senior Elixir engineer"},
		{
			name:  "two languages",
			langs: []Language{LangGo, LangPython},
			want:  "senior software engineer with deep Go and Python expertise",
		},
		{
			name:  "three languages",
			langs: []Language{LangGo, LangPython, LangRust},
			want:  "senior software engineer with deep Go, Python and Rust expertise",
		},
		{
			name:  "display names rendered (TS / C++ / C#)",
			langs: []Language{LangTypeScript, LangCPP},
			want:  "senior software engineer with deep TypeScript and C++ expertise",
		},
		{
			name:  "LangOther has no display name and is skipped to generic",
			langs: []Language{LangOther},
			want:  "senior software engineer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PersonaLanguages(tt.langs, nil); got != tt.want {
				t.Errorf("PersonaLanguages(%v) = %q, want %q", tt.langs, got, tt.want)
			}
		})
	}
}

// TestPersona exercises the one-call helper end to end from a snapshot.
func TestPersona(t *testing.T) {
	tests := []struct {
		name string
		snap *Snapshot
		want string
	}{
		{"pure Go", snapFromLangs(LangGo, LangGo), "senior Go engineer"},
		{"pure Python", snapFromLangs(LangPython), "senior Python engineer"},
		{"pure Elixir", snapFromLangs(LangElixir, LangElixir, LangElixir), "senior Elixir engineer"},
		{
			name: "Go+Python mix",
			snap: snapFromLangs(LangGo, LangGo, LangPython),
			want: "senior software engineer with deep Go and Python expertise",
		},
		{"empty", snapFromLangs(), "senior software engineer"},
		{"nil", nil, "senior software engineer"},
		{"only noise", snapFromLangs(LangOther, LangShell), "senior software engineer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Persona(tt.snap); got != tt.want {
				t.Errorf("Persona() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPersonaLanguagesWithQualifiers verifies that the qualifiers map overrides
// the base display name for the specified language, enabling dialect-qualified
// personas such as "senior C++20 engineer".
func TestPersonaLanguagesWithQualifiers(t *testing.T) {
	tests := []struct {
		name       string
		langs      []Language
		qualifiers map[Language]string
		want       string
	}{
		{
			name:       "C++ with C++20 qualifier",
			langs:      []Language{LangCPP},
			qualifiers: map[Language]string{LangCPP: "C++20"},
			want:       "senior C++20 engineer",
		},
		{
			name:       "C++ with C++17 qualifier",
			langs:      []Language{LangCPP},
			qualifiers: map[Language]string{LangCPP: "C++17"},
			want:       "senior C++17 engineer",
		},
		{
			name:       "nil qualifiers falls back to base display name",
			langs:      []Language{LangCPP},
			qualifiers: nil,
			want:       "senior C++ engineer",
		},
		{
			name:       "empty qualifiers falls back to base display name",
			langs:      []Language{LangCPP},
			qualifiers: map[Language]string{},
			want:       "senior C++ engineer",
		},
		{
			name:       "qualifier for non-C++ language (mechanism is general)",
			langs:      []Language{LangJava},
			qualifiers: map[Language]string{LangJava: "Java 21"},
			want:       "senior Java 21 engineer",
		},
		{
			name:       "qualifier applies only to matching language in mix",
			langs:      []Language{LangCPP, LangPython},
			qualifiers: map[Language]string{LangCPP: "C++20"},
			want:       "senior software engineer with deep C++20 and Python expertise",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PersonaLanguages(tt.langs, tt.qualifiers); got != tt.want {
				t.Errorf("PersonaLanguages(%v, %v) = %q, want %q", tt.langs, tt.qualifiers, got, tt.want)
			}
		})
	}
}

// TestPersonaWithCPPStandard verifies end-to-end that a C++-dominant snapshot
// with a CMake C++ standard declaration produces a qualified persona.
// This is the primary acceptance criterion from the bead.
func TestPersonaWithCPPStandard(t *testing.T) {
	dir := t.TempDir()

	// Write CMakeLists.txt declaring C++20.
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nset(CMAKE_CXX_STANDARD 20)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a C++-dominant snapshot using the temp dir as Root.
	snap := &Snapshot{
		Root:  dir,
		Files: snapFromLangs(LangCPP, LangCPP, LangCPP).Files,
	}

	got := Persona(snap)
	if got != "senior C++20 engineer" {
		t.Errorf("Persona() = %q, want %q", got, "senior C++20 engineer")
	}
}

// TestPersonaNoStandardNoQualifier verifies that a C++ snapshot without any
// detectable standard stays plain "senior C++ engineer".
func TestPersonaNoStandardNoQualifier(t *testing.T) {
	dir := t.TempDir() // empty — no build files

	snap := &Snapshot{
		Root:  dir,
		Files: snapFromLangs(LangCPP, LangCPP).Files,
	}

	got := Persona(snap)
	if got != "senior C++ engineer" {
		t.Errorf("Persona() = %q, want %q", got, "senior C++ engineer")
	}
}

// TestPersonaNonCPPUnchanged verifies that non-C++ repos are unaffected by the
// qualifier mechanism.
func TestPersonaNonCPPUnchanged(t *testing.T) {
	dir := t.TempDir()
	// Write a CMakeLists.txt — it should have no effect on a Go-dominant repo.
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"),
		[]byte("set(CMAKE_CXX_STANDARD 20)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{
		Root:  dir,
		Files: snapFromLangs(LangGo, LangGo, LangGo).Files,
	}

	got := Persona(snap)
	if got != "senior Go engineer" {
		t.Errorf("Persona() = %q, want %q", got, "senior Go engineer")
	}
}

// TestDisplayName pins the human-readable rendering for each known language,
// and confirms the LangOther fallback returns the raw Language string.
func TestDisplayName(t *testing.T) {
	cases := []struct {
		lang Language
		want string
	}{
		{LangGo, "Go"},
		{LangPython, "Python"},
		{LangCPP, "C++"},
		{LangCSharp, "C#"},
		{LangElixir, "Elixir"},
		{LangZig, "Zig"},
		{LangGleam, "Gleam"},
		{LangScala, "Scala"},
		{LangDart, "Dart"},
		{LangLua, "Lua"},
		{LangObjC, "Objective-C"},
		{LangOther, string(LangOther)}, // fallback: raw Language string
	}
	for _, tc := range cases {
		if got := DisplayName(tc.lang); got != tc.want {
			t.Errorf("DisplayName(%s) = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

// TestPersonaElixirDominant verifies end-to-end that a snapshot dominated by
// Elixir files yields a persona mentioning Elixir — closing the cross-pkg
// contract with HeatFilter, which excludes only LangOther files from heat.
func TestPersonaElixirDominant(t *testing.T) {
	snap := snapFromLangs(LangElixir, LangElixir, LangElixir, LangElixir)
	got := Persona(snap)
	want := "senior Elixir engineer"
	if got != want {
		t.Errorf("Persona(Elixir-dominant) = %q, want %q", got, want)
	}
}

// TestPersonaZigDominant verifies end-to-end that a snapshot dominated by
// Zig files yields a persona mentioning Zig — confirming it is not excluded
// as LangOther and that DominantLanguages and DisplayName agree.
func TestPersonaZigDominant(t *testing.T) {
	snap := snapFromLangs(LangZig, LangZig, LangZig, LangZig)
	got := Persona(snap)
	want := "senior Zig engineer"
	if got != want {
		t.Errorf("Persona(Zig-dominant) = %q, want %q", got, want)
	}
}

// TestPersonaGleamDominant verifies that Gleam-dominant snapshots produce a
// correctly named persona.
func TestPersonaGleamDominant(t *testing.T) {
	snap := snapFromLangs(LangGleam, LangGleam, LangGleam)
	got := Persona(snap)
	want := "senior Gleam engineer"
	if got != want {
		t.Errorf("Persona(Gleam-dominant) = %q, want %q", got, want)
	}
}

// TestDominantLanguagesNewLangs verifies that the new minor languages are
// counted by DominantLanguages (i.e. not silently dropped as LangOther).
func TestDominantLanguagesNewLangs(t *testing.T) {
	cases := []struct {
		lang Language
		name string
	}{
		{LangZig, "Zig"},
		{LangGleam, "Gleam"},
		{LangScala, "Scala"},
		{LangDart, "Dart"},
		{LangLua, "Lua"},
		{LangObjC, "Objective-C"},
	}
	for _, tc := range cases {
		snap := snapFromLangs(tc.lang, tc.lang, tc.lang)
		langs := DominantLanguages(snap)
		if len(langs) != 1 || langs[0] != tc.lang {
			t.Errorf("DominantLanguages(%s): got %v, want [%s]", tc.name, langs, tc.lang)
		}
		if got := DisplayName(tc.lang); got != tc.name {
			t.Errorf("DisplayName(%s) = %q, want %q", tc.lang, got, tc.name)
		}
	}
}
