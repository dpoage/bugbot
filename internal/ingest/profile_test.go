package ingest

import (
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
			if got := PersonaLanguages(tt.langs); got != tt.want {
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
