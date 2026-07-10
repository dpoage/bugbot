package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// ── unit tests for cfPassFieldDecls ──────────────────────────────────────────

func TestCfPassFieldDecls_DefaultInDoc(t *testing.T) {
	src := `package x

type Config struct {
	// MaxRetries is the retry cap. Default: 0
	MaxRetries int
	// Timeout in seconds.
	Timeout int
}
`
	lines := strings.Split(src, "\n")
	decls := cfPassFieldDecls("cfg.go", lines)

	byName := make(map[string]cfFieldDecl)
	for _, d := range decls {
		byName[d.name] = d
	}

	d, ok := byName["MaxRetries"]
	if !ok {
		t.Fatal("expected declaration for MaxRetries")
	}
	if d.defaultVal == nil {
		t.Fatal("expected defaultVal for MaxRetries, got nil")
	}
	if *d.defaultVal != 0 {
		t.Errorf("defaultVal = %d, want 0", *d.defaultVal)
	}

	d2, ok := byName["Timeout"]
	if !ok {
		t.Fatal("expected declaration for Timeout")
	}
	if d2.defaultVal != nil {
		t.Errorf("Timeout should have no defaultVal, got %d", *d2.defaultVal)
	}
}

func TestCfPassFieldDecls_DefaultTag(t *testing.T) {
	src := `package x

type Opts struct {
	// Port is the listen port.
	Port int ` + "`" + `default:"8080"` + "`" + `
}
`
	lines := strings.Split(src, "\n")
	decls := cfPassFieldDecls("opts.go", lines)

	for _, d := range decls {
		if d.name == "Port" {
			if d.defaultVal == nil {
				t.Fatal("Port: expected defaultVal from tag, got nil")
			}
			if *d.defaultVal != 8080 {
				t.Errorf("Port defaultVal = %d, want 8080", *d.defaultVal)
			}
			return
		}
	}
	t.Fatal("Port declaration not found")
}

func TestCfPassValidators_RejectsZero(t *testing.T) {
	src := `package x

import "fmt"

func Validate(c *Config) error {
	if c.MaxRetries <= 0 {
		return fmt.Errorf("MaxRetries must be > 0")
	}
	return nil
}
`
	lines := strings.Split(src, "\n")
	vals := cfPassValidators("v.go", lines)

	var found *cfValidatorSite
	for i := range vals {
		if vals[i].fieldName == "MaxRetries" {
			found = &vals[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected validator site for MaxRetries")
	}
	if !found.rejectsZero {
		t.Error("expected rejectsZero=true")
	}
}

func TestCfPassValidators_RejectsNeg(t *testing.T) {
	src := `package x

import "fmt"

func Validate(c *Config) error {
	if c.Timeout < 0 {
		return fmt.Errorf("Timeout must not be negative")
	}
	return nil
}
`
	lines := strings.Split(src, "\n")
	vals := cfPassValidators("v.go", lines)

	var found *cfValidatorSite
	for i := range vals {
		if vals[i].fieldName == "Timeout" {
			found = &vals[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected validator site for Timeout")
	}
	if !found.rejectsNeg {
		t.Error("expected rejectsNeg=true")
	}
}

// ── integration tests via seedConfigFieldContradictions ───────────────────────

func makeCfStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, ctx
}

// TestCfMiner_PositiveFixture verifies that a struct field with Default: 0
// and a validator rejecting 0 yields EXACTLY ONE lead with PosterLens
// "miner:config-field".
func TestCfMiner_PositiveFixture(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

import "fmt"

type ServerConfig struct {
	// MaxConnections is the maximum number of connections allowed.
	// Default: 0
	MaxConnections int
}

func (c *ServerConfig) Validate() error {
	if c.MaxConnections <= 0 {
		return fmt.Errorf("MaxConnections must be > 0")
	}
	return nil
}
`
	p := filepath.Join(dir, "cfg.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "cfg.go", Language: ingest.LangGo},
		},
	}
	st, ctx := makeCfStore(t)
	var sum Summary
	if err := seedConfigFieldContradictions(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldContradictions: %v", err)
	}

	leads, err := st.PendingLeads(ctx, cfTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	cfLeads := filterCfLeadsByPoster(leads, cfPosterLens)
	if len(cfLeads) != 1 {
		t.Errorf("want 1 config-field lead, got %d (all leads: %+v)", len(cfLeads), leads)
	}
	if len(cfLeads) > 0 {
		l := cfLeads[0]
		if l.PosterLens != cfPosterLens {
			t.Errorf("PosterLens = %q, want %q", l.PosterLens, cfPosterLens)
		}
		if l.TargetLens != cfTargetLens {
			t.Errorf("TargetLens = %q, want %q", l.TargetLens, cfTargetLens)
		}
		if l.File != "cfg.go" {
			t.Errorf("File = %q, want cfg.go", l.File)
		}
		if !strings.Contains(l.Note, "MaxConnections") {
			t.Errorf("Note does not mention field name: %q", l.Note)
		}
	}
}

// TestCfMiner_NegativeFixture verifies that a field whose default is inside
// its validated range AND a normative-documented field that IS read produce
// ZERO leads.
func TestCfMiner_NegativeFixture(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

import "fmt"

type WorkerConfig struct {
	// Workers is the number of worker goroutines. defaults to 4
	Workers int

	// QueueSize must be set before calling Start; required for back-pressure.
	QueueSize int
}

func (c *WorkerConfig) Validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("Workers must be > 0")
	}
	return nil
}

func Start(cfg WorkerConfig) {
	_ = cfg.QueueSize
	_ = cfg.Workers
}
`
	p := filepath.Join(dir, "worker.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "worker.go", Language: ingest.LangGo},
		},
	}
	st, ctx := makeCfStore(t)
	var sum Summary
	if err := seedConfigFieldContradictions(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldContradictions: %v", err)
	}

	leads, err := st.PendingLeads(ctx, cfTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	cfLeads := filterCfLeadsByPoster(leads, cfPosterLens)
	// Workers: default 4 is inside the valid range (> 0) → no lead.
	// QueueSize: normative doc but field IS read (cfg.QueueSize in Start) → no lead.
	if len(cfLeads) != 0 {
		t.Errorf("want 0 config-field leads, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// TestCfMiner_NoDoublePostOnDocContradictionFixture checks that the config-field
// miner emits ZERO "miner:config-field" leads on the existing doc-sentinel fixture
// (the ig7_contradiction testdata), ensuring no overlap with miner:doc-contradiction.
func TestCfMiner_NoDoublePostOnDocContradictionFixture(t *testing.T) {
	// Use the existing testdata fixture as the snapshot root.
	fixtureDir := filepath.Join("testdata", "ig7_contradiction")
	if _, err := os.Stat(fixtureDir); err != nil {
		t.Skipf("testdata/ig7_contradiction not present: %v", err)
	}

	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var files []ingest.File
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			files = append(files, ingest.File{Path: e.Name(), Language: ingest.LangGo})
		}
	}

	snap := &ingest.Snapshot{Root: fixtureDir, Files: files}
	st, ctx := makeCfStore(t)
	var sum Summary
	if err := seedConfigFieldContradictions(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldContradictions: %v", err)
	}

	leads, err := st.PendingLeads(ctx, cfTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	for _, l := range leads {
		if l.PosterLens == cfPosterLens {
			t.Errorf("unexpected config-field lead on doc-contradiction fixture: %+v", l)
		}
	}
}

// TestCfMiner_NormativeNeverRead verifies that a struct field with a normative
// doc comment that is never read outside its declaration file gets a lead.
func TestCfMiner_NormativeNeverRead(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

type AuthConfig struct {
	// SecretKey must be set before serving requests; required for token signing.
	SecretKey string
}
`
	p := filepath.Join(dir, "auth.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "auth.go", Language: ingest.LangGo},
		},
	}
	st, ctx := makeCfStore(t)
	var sum Summary
	if err := seedConfigFieldContradictions(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldContradictions: %v", err)
	}

	leads, err := st.PendingLeads(ctx, cfTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	cfLeads := filterCfLeadsByPoster(leads, cfPosterLens)
	if len(cfLeads) != 1 {
		t.Errorf("want 1 normative-never-read lead, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func filterCfLeadsByPoster(leads []store.Lead, poster string) []store.Lead {
	var out []store.Lead
	for _, l := range leads {
		if l.PosterLens == poster {
			out = append(out, l)
		}
	}
	return out
}
