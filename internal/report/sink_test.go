package report

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

func TestFSSinkWritesTimestampedAndLatest(t *testing.T) {
	dir := t.TempDir()
	sink := &FSSink{Dir: filepath.Join(dir, "reports")}
	r := New(fixtureFindings(), fixtureMeta())

	if err := sink.Write(context.Background(), r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// GeneratedAt is fixedTime (2026-06-09 12:00:00 UTC) -> 20260609T120000Z.
	for _, name := range []string{
		"report-20260609T120000Z.md",
		"report-20260609T120000Z.sarif",
		"latest.md",
		"latest.sarif",
	} {
		path := filepath.Join(sink.Dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s: %v", name, err)
		}
	}

	// latest.md must equal the timestamped md.
	ts, err := os.ReadFile(filepath.Join(sink.Dir, "report-20260609T120000Z.md"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(filepath.Join(sink.Dir, "latest.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ts, latest) {
		t.Error("latest.md differs from timestamped report")
	}

	// latest.sarif must be valid SARIF.
	sarifBytes, err := os.ReadFile(filepath.Join(sink.Dir, "latest.sarif"))
	if err != nil {
		t.Fatal(err)
	}
	var doc SARIFLog
	if err := json.Unmarshal(sarifBytes, &doc); err != nil {
		t.Fatalf("latest.sarif invalid: %v", err)
	}
	if len(doc.Runs) != 1 {
		t.Errorf("sarif runs = %d, want 1", len(doc.Runs))
	}
}

func TestFSSinkCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	sink := &FSSink{Dir: dir}
	if err := sink.Write(context.Background(), New(nil, fixtureMeta())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "latest.md")); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestFSSinkEmptyDirErrors(t *testing.T) {
	sink := &FSSink{Dir: ""}
	if err := sink.Write(context.Background(), New(nil, fixtureMeta())); err == nil {
		t.Error("expected error for empty Dir")
	}
}

func TestFSSinkRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &FSSink{Dir: t.TempDir()}
	if err := sink.Write(ctx, New(nil, fixtureMeta())); err == nil {
		t.Error("expected cancellation error")
	}
}

func TestStdoutSinkWritesMarkdown(t *testing.T) {
	var buf bytes.Buffer
	sink := &StdoutSink{W: &buf}
	if err := sink.Write(context.Background(), New(fixtureFindings(), fixtureMeta())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), "# Bugbot Report") {
		t.Error("stdout sink did not render markdown header")
	}
}

func TestSinksFromConfig(t *testing.T) {
	tests := []struct {
		name      string
		sinks     []string
		wantNames []string
		wantErr   bool
	}{
		{"empty defaults to stdout", nil, []string{"stdout"}, false},
		{"fs and stdout", []string{"fs", "stdout"}, []string{"fs", "stdout"}, false},
		{"legacy markdown alias maps to fs", []string{"markdown"}, []string{"fs"}, false},
		{"legacy sarif alias maps to fs", []string{"sarif"}, []string{"fs"}, false},
		{"markdown+sarif collapse to one fs", []string{"markdown", "sarif"}, []string{"fs"}, false},
		{"unknown errors", []string{"webhook"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Report: config.Report{Dir: "out", Sinks: tc.sinks}}
			sinks, err := SinksFromConfig(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), "valid:") {
					t.Errorf("error should list valid sinks: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var got []string
			for _, s := range sinks {
				got = append(got, s.Name())
			}
			if strings.Join(got, ",") != strings.Join(tc.wantNames, ",") {
				t.Errorf("sinks = %v, want %v", got, tc.wantNames)
			}
		})
	}
}
