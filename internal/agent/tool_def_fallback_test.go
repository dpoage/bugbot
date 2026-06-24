package agent

import (
	"strings"
	"testing"
)

// TestGrepDef_FramedAsFallback pins that the grep tool advertises itself as a
// fallback that defers to the structural code-navigation tools. The model reads
// tool Def descriptions when selecting tools, so this schema-level signal must
// agree with the finder/verifier system prompts that present grep as a fallback.
func TestGrepDef_FramedAsFallback(t *testing.T) {
	g, err := NewGrep(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := g.Def().Description
	for _, want := range []string{"FALLBACK", "find_references", "read_symbol"} {
		if !strings.Contains(d, want) {
			t.Errorf("grep Def must mention %q to steer toward structural tools; got:\n%s", want, d)
		}
	}
}

// TestReadFileDef_PointsToReadSymbol pins that read_file points callers at
// read_symbol/outline for single-declaration reads rather than defaulting to a
// whole-file read.
func TestReadFileDef_PointsToReadSymbol(t *testing.T) {
	r, err := NewReadFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := r.Def().Description
	for _, want := range []string{"read_symbol", "outline"} {
		if !strings.Contains(d, want) {
			t.Errorf("read_file Def must point to %q for single-declaration reads; got:\n%s", want, d)
		}
	}
}
