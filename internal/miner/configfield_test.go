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

// TestCfPassFieldDecls_SkipsConstBlock ensures const/var block entries are
// not harvested as struct fields (DEFECT 3).
func TestCfPassFieldDecls_SkipsConstBlock(t *testing.T) {
	src := `package x

type SmokeCategory string

const (
	// SmokeCategoryDepMissing: the toolchain is present but required
	// dependencies are missing.
	SmokeCategoryDepMissing SmokeCategory = "dep_missing"
)

type Config struct {
	// MaxRetries is the retry cap. Default: 0
	MaxRetries int
}
`
	lines := strings.Split(src, "\n")
	decls := cfPassFieldDecls("x.go", lines)

	for _, d := range decls {
		if d.name == "SmokeCategoryDepMissing" {
			t.Errorf("SmokeCategoryDepMissing should not be harvested as a struct field")
		}
	}
	// MaxRetries should still be found.
	found := false
	for _, d := range decls {
		if d.name == "MaxRetries" {
			found = true
		}
	}
	if !found {
		t.Error("MaxRetries should be harvested from struct block")
	}
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

// TestCfPassValidators_AcceptGuardNotRejection ensures X >= 0 is NOT treated
// as a rejection (DEFECT 1).
func TestCfPassValidators_AcceptGuardNotRejection(t *testing.T) {
	src := `package x

import "fmt"

func Validate(c *Config) error {
	if c.Timeout >= 0 {
		// accept-guard: zero and above is fine
	}
	if c.MaxRetries <= 0 {
		return fmt.Errorf("MaxRetries must be > 0")
	}
	return nil
}
`
	lines := strings.Split(src, "\n")
	vals := cfPassValidators("v.go", lines)

	for _, v := range vals {
		if v.fieldName == "Timeout" {
			t.Errorf("Timeout: X >= 0 is an accept-guard, must not be a rejection")
		}
	}
	// MaxRetries <= 0 with error return SHOULD be captured.
	found := false
	for _, v := range vals {
		if v.fieldName == "MaxRetries" {
			found = true
		}
	}
	if !found {
		t.Error("MaxRetries <= 0 with error return should be a rejection")
	}
}

// TestCfPassValidators_SentinelReturnNotRejection ensures bare `return` without
// error in the body is NOT treated as a rejection (DEFECT 2).
func TestCfPassValidators_SentinelReturnNotRejection(t *testing.T) {
	src := `package x

// Timeout is in seconds.
// Default: 0, 0 means the built-in default.
func Apply(c *Config) {
	if c.Timeout <= 0 {
		return
	}
	_ = c.Timeout
}
`
	lines := strings.Split(src, "\n")
	vals := cfPassValidators("v.go", lines)

	for _, v := range vals {
		if v.fieldName == "Timeout" {
			t.Errorf("Timeout with sentinel bare-return should NOT be a rejection, got: %+v", v)
		}
	}
}

func TestCfPassValidators_RejectsNeg(t *testing.T) {
	src := `package x

import "fmt"

func Validate(c *Config) error {
	if c.Budget < 0 {
		return fmt.Errorf("Budget must be >= 0")
	}
	return nil
}
`
	lines := strings.Split(src, "\n")
	vals := cfPassValidators("v.go", lines)

	var found *cfValidatorSite
	for i := range vals {
		if vals[i].fieldName == "Budget" {
			found = &vals[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected validator site for Budget")
	}
	if !found.rejectsNeg {
		t.Error("expected rejectsNeg=true")
	}
}

// ── integration tests via seedConfigFieldContradictions ───────────────────────

func makeCfStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "leads.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, ctx
}

// TestCfMiner_PositiveFixture verifies that the committed testdata positive
// fixture yields EXACTLY ONE lead with PosterLens "miner:config-field".
// Mutating testdata/cfgfield_positive.go is what flips this test.
func TestCfMiner_PositiveFixture(t *testing.T) {
	fixtureFile := filepath.Join("testdata", "cfgfield_positive.go")
	raw, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", fixtureFile, err)
	}
	src := string(raw)

	dir := t.TempDir()
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

// TestCfMiner_NegativeFixture verifies that the committed testdata negative
// fixture yields ZERO leads. Mutating testdata/cfgfield_negative.go is what
// flips this test.
func TestCfMiner_NegativeFixture(t *testing.T) {
	fixtureFile := filepath.Join("testdata", "cfgfield_negative.go")
	raw, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", fixtureFile, err)
	}
	src := string(raw)

	dir := t.TempDir()
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

// TestCfMiner_NoDoublePostOnDocContradictionFixture verifies that running the
// FULL Seed (both doc-contradiction and config-field passes) on a combined
// fixture produces leads from BOTH passes without clobbering each other.
//
// DEFECT 6: AddLead upserts on (target_lens, file, line); a config-field lead
// at the same (file, line) as a doc-contradiction lead would silently clobber
// the latter. This test asserts both leads coexist.
func TestCfMiner_NoDoublePostOnDocContradictionFixture(t *testing.T) {
	dir := t.TempDir()

	// widget_limit.go: triggers doc-contradiction (0 = unlimited sentinel) and
	// config-field signal (a) would too IF it weren't for the sentinel guard.
	// The doc says "0 = unlimited" so config-field MUST NOT post here.
	widgetSrc := `package config

// Config holds widget limits.
type Config struct {
	// WidgetLimit is the cap on operations.
	// 0 = unlimited.
	WidgetLimit int
}
`

	// validator.go: validates WidgetLimit with an error return.
	// doc-contradiction fires here (the only lead at this file+line).
	validatorSrc := `package config

import "fmt"

func Validate(w *Config) error {
	if w.WidgetLimit <= 0 {
		return fmt.Errorf("widget_limit must be > 0")
	}
	return nil
}
`

	// cfonly.go: triggers config-field signal (a) at a different (file, line)
	// from the doc-contradiction lead.
	cfOnlySrc := `package config

import "fmt"

type ServerConfig struct {
	// MaxConn is the max connections. Default: 0
	MaxConn int
}

func (c *ServerConfig) Validate() error {
	if c.MaxConn <= 0 {
		return fmt.Errorf("MaxConn must be > 0")
	}
	return nil
}
`

	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	writeFile("widget_limit.go", widgetSrc)
	writeFile("validator.go", validatorSrc)
	writeFile("cfonly.go", cfOnlySrc)

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "widget_limit.go", Language: ingest.LangGo},
			{Path: "validator.go", Language: ingest.LangGo},
			{Path: "cfonly.go", Language: ingest.LangGo},
		},
	}

	st, ctx := makeCfStore(t)
	if _, err := Seed(ctx, snap, st); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, cfTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var docLeads, cfLeads []store.Lead
	for _, l := range leads {
		switch l.PosterLens {
		case posterLens: // "miner:doc-contradiction"
			docLeads = append(docLeads, l)
		case cfPosterLens: // "miner:config-field"
			cfLeads = append(cfLeads, l)
		}
	}

	// Doc-contradiction must fire on validator.go (WidgetLimit <= 0 guard).
	if len(docLeads) == 0 {
		t.Error("expected at least one doc-contradiction lead on widget_limit fixture")
	}
	// Config-field must fire on cfonly.go (MaxConn default 0 + rejectsZero).
	hasCfLead := false
	for _, l := range cfLeads {
		if l.File == "cfonly.go" {
			hasCfLead = true
		}
	}
	if !hasCfLead {
		t.Errorf("expected a config-field lead on cfonly.go; got cfLeads=%+v", cfLeads)
	}
	// Config-field must NOT post on validator.go (sentinel doc "0 = unlimited").
	for _, l := range cfLeads {
		if l.File == "validator.go" {
			t.Errorf("config-field must not clobber doc-contradiction on validator.go: %+v", l)
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

// TestCfMiner_NormativeProseNotFlagged ensures that a doc comment where a
// normative word appears in PROSE (not bound to the field) is NOT flagged
// (DEFECT 4: "required dependencies are missing" should not fire).
func TestCfMiner_NormativeProseNotFlagged(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

type SmokeCategory string

const (
	// SmokeCategoryDepMissing: the toolchain is present but required
	// dependencies are missing (missing module, missing package, etc.).
	SmokeCategoryDepMissing SmokeCategory = "dep_missing"
)

type SomeConfig struct {
	// Category is the category. callers must always validate before use.
	Category SmokeCategory
}

func Use(c SomeConfig) {
	_ = c.Category
}
`
	p := filepath.Join(dir, "smoke.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "smoke.go", Language: ingest.LangGo},
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

	// SmokeCategoryDepMissing: inside const block, should not be harvested.
	// Category: is read via c.Category in Use → no never-read lead.
	cfLeads := filterCfLeadsByPoster(leads, cfPosterLens)
	if len(cfLeads) != 0 {
		t.Errorf("want 0 config-field leads for prose/const fixture, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// ── regression tests for oracle-found false-positive sources ─────────────────

// TestCfMiner_D1_SiblingGuardErrorReturnNotBledIn reproduces DEFECT D1:
// a normalize guard (`if x <= 0 { x = defaultX }`) followed by a SEPARATE
// error-returning guard on a DIFFERENT field in the SAME function. Before the
// fix, the error return in the second guard bled into the first guard's 6-line
// window, causing the normalized field to be falsely flagged as a validator
// contradiction. After the fix the block-scoped scan limits the search to the
// first guard's own braces and yields ZERO leads.
func TestCfMiner_D1_SiblingGuardErrorReturnNotBledIn(t *testing.T) {
	src := `package fixture

import "fmt"

// Cfg holds configuration.
type Cfg struct {
	// X is the poll interval. Default: 0
	X int
	// Name is the service name.
	Name string
}

// Validate validates the config.
func (c *Cfg) Validate() error {
	if c.X <= 0 {
		c.X = 10 // normalize, NOT an error return
	}
	if c.Name == "" {
		return fmt.Errorf("Name must not be empty")
	}
	return nil
}
`
	dir := t.TempDir()
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
	if len(cfLeads) != 0 {
		t.Errorf("D1: want 0 leads (X is normalized not rejected), got %d: %+v", len(cfLeads), cfLeads)
	}
}

// TestCfMiner_D2_CrossStructJoinNoFalsePositive reproduces DEFECT D2:
// two structs sharing a field name where only ONE struct's validator rejects
// zero. Before the fix, the bare-fieldName join matched the other struct's
// validator, producing a false lead. After the fix the join is scoped by
// (structType, fieldName) and yields ZERO leads on the unrelated struct.
func TestCfMiner_D2_CrossStructJoinNoFalsePositive(t *testing.T) {
	src := `package fixture

import "fmt"

// ServerConfig holds server configuration.
type ServerConfig struct {
	// Timeout is the server timeout. Default: 0
	Timeout int
}

// ClientConfig holds client configuration.
type ClientConfig struct {
	// Timeout is the client timeout.
	Timeout int
}

// Validate validates ClientConfig — rejects zero Timeout.
func (c *ClientConfig) Validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("Timeout must be > 0")
	}
	return nil
}
`
	dir := t.TempDir()
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
	// ServerConfig.Timeout has Default:0 but NO validator of its own.
	// ClientConfig.Timeout has a validator but NO documented default.
	// Cross-struct join must NOT produce a lead on ServerConfig.Timeout.
	if len(cfLeads) != 0 {
		t.Errorf("D2: want 0 leads (cross-struct join must not fire), got %d: %+v", len(cfLeads), cfLeads)
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
