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
// Default: 0
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
// fixture produces leads from BOTH passes without interfering.
//
// The two passes post at DISTINCT (file, line) loci so no upsert collision
// occurs in this fixture. The test guards that each pass fires independently.
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
	// Config-field must NOT post on widget_limit.go (sentinel doc "0 = unlimited").
	for _, l := range cfLeads {
		if l.File == "widget_limit.go" {
			t.Errorf("config-field must not fire on sentinel-doc field: %+v", l)
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
// normative word appears in PROSE (not bound to the field name with a leading
// uppercase letter) is NOT flagged (DEFECT 4).
//
// Fixture: struct field Foo is never read. Its first doc line is
// "foo must handle failures." — "foo" (lowercase) is an unbound normative word.
// Under the old buggy regex (global (?i) making [A-Z] case-insensitive),
// "foo must" would match the FieldName-must alternative and produce a false
// lead. Under the fixed regex ([A-Z] requires PascalCase, no global (?i)),
// "foo must" does not match and zero leads are produced.
func TestCfMiner_NormativeProseNotFlagged(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

type SomeConfig struct {
	// foo must handle failures; this prose is unbound to the field name.
	Foo string
}
`
	p := filepath.Join(dir, "prose.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "prose.go", Language: ingest.LangGo},
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

	// Foo is never read, but "foo must" (lowercase) is unbound prose —
	// it must NOT trigger cfNormativeDocRe with the fixed regex.
	cfLeads := filterCfLeadsByPoster(leads, cfPosterLens)
	if len(cfLeads) != 0 {
		t.Errorf("want 0 config-field leads for unbound prose fixture, got %d: %+v", len(cfLeads), cfLeads)
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

// TestCfMiner_CrossPackageStructNameNoFalsePositive is the regression test for
// the cross-package struct-name collision. Package "server" has Config.Timeout
// with Default:0 and NO validator. Package "client" has Config.Timeout with a
// validator that rejects zero. Because these are DIFFERENT packages, the join
// must NOT produce a lead — the default belongs to server's Config, not
// client's Config.
func TestCfMiner_CrossPackageStructNameNoFalsePositive(t *testing.T) {
	serverSrc := `package server

// Config holds server configuration.
type Config struct {
	// Timeout is the server timeout. Default: 0
	Timeout int
}
`
	clientSrc := `package client

import "fmt"

// Config holds client configuration.
type Config struct {
	Timeout int
}

// Validate validates that client Config is well-formed.
func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("Timeout must be > 0")
	}
	return nil
}
`
	dir := t.TempDir()
	// Place files in separate subdirectories to simulate different packages.
	// Go package identity is the directory; the dir-based join key must not
	// conflate "server/config.go" with "client/config.go" even if both have
	// `type Config struct` and both package clauses happen to differ.
	serverDir := filepath.Join(dir, "server")
	clientDir := filepath.Join(dir, "client")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "config.go"), []byte(serverSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "config.go"), []byte(clientSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "server/config.go", Language: ingest.LangGo},
			{Path: "client/config.go", Language: ingest.LangGo},
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
	// server's Config.Timeout has Default:0 but NO validator.
	// client's Config.Timeout has a validator but NO documented default.
	// Cross-DIRECTORY join must NOT produce a lead on server's Config.Timeout.
	if len(cfLeads) != 0 {
		t.Errorf("cross-package collision: want 0 leads, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// TestCfMiner_SamePackageCrossFileDetectionFires verifies that a same-package
// detection spanning two files (field decl in a.go, validator in b.go) still
// fires a lead — same-package scoping must not accidentally require same-file.
func TestCfMiner_SamePackageCrossFileDetectionFires(t *testing.T) {
	declSrc := `package myapp

// Config holds application configuration.
type Config struct {
	// Timeout is the timeout duration. Default: 0
	Timeout int
}
`
	validatorSrc := `package myapp

import "fmt"

// Validate rejects a zero or negative timeout.
func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("Timeout must be > 0")
	}
	return nil
}
`
	dir := t.TempDir()
	declFile := filepath.Join(dir, "config.go")
	valFile := filepath.Join(dir, "validate.go")
	if err := os.WriteFile(declFile, []byte(declSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valFile, []byte(validatorSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &ingest.Snapshot{
		Root: dir,
		Files: []ingest.File{
			{Path: "config.go", Language: ingest.LangGo},
			{Path: "validate.go", Language: ingest.LangGo},
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
	// Field decl and validator are in different files but the SAME package.
	// The join must still fire exactly one lead on Config.Timeout.
	if len(cfLeads) != 1 {
		t.Errorf("same-package cross-file: want 1 lead, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// TestCfMiner_CrossDirSamePackageNameNoFalseJoin is the regression test for
// PROD FINDING 1: two directories each containing `package config` (same
// package-clause name), same struct name, same field name. One directory has
// a Default:0 doc and NO validator; the other has a rejecting validator and NO
// documented default. The dir-based join key must prevent a false positive.
//
// Fails before the fix (pkg-name key joins across dirs), passes after.
func TestCfMiner_CrossDirSamePackageNameNoFalseJoin(t *testing.T) {
	// dir A: `package config` with Config.Timeout Default:0 and no validator.
	declSrc := `package config

// Config holds configuration.
type Config struct {
	// Timeout is the timeout. Default: 0
	Timeout int
}
`
	// dir B: ALSO `package config` but with a rejecting validator and no default.
	valSrc := `package config

import "fmt"

// Config holds configuration.
type Config struct {
	Timeout int
}

func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("Timeout must be > 0")
	}
	return nil
}
`
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "config.go"), []byte(declSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "config.go"), []byte(valSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &ingest.Snapshot{
		Root: root,
		Files: []ingest.File{
			{Path: "a/config.go", Language: ingest.LangGo},
			{Path: "b/config.go", Language: ingest.LangGo},
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
	// a/config.go has Default:0 but no validator in the same directory.
	// b/config.go has a validator but no documented default.
	// The cross-directory join must NOT fire even though both use `package config`.
	if len(cfLeads) != 0 {
		t.Errorf("cross-dir same-pkg-name: want 0 leads, got %d: %+v", len(cfLeads), cfLeads)
	}
}

// TestCfMiner_NormalizeThenValidateNoFalsePositive is the regression test for
// PROD FINDING 2: a Normalize method (no error return) sets the field to a
// default when it is zero, followed by a Validate method that rejects zero.
// Because zero is normalized before validation, the field is healthy.
// The field doc documents "0 means auto-detect" — a sentinel value — which
// cfSentinelDocRe must match to suppress the false positive.
//
// Fails before the fix (cfSentinelDocRe not consulted on field doc), passes after.
func TestCfMiner_NormalizeThenValidateNoFalsePositive(t *testing.T) {
	src := `package config

import "fmt"

// Cfg holds configuration.
type Cfg struct {
	// Conns is the connection pool size. Default: 0 (0 means auto-detect).
	Conns int
}

// Normalize sets Conns to a safe default when it is zero or negative.
func (c *Cfg) Normalize() {
	if c.Conns <= 0 {
		c.Conns = 4 // auto-detect
	}
}

// Validate rejects zero or negative Conns (called after Normalize).
func (c *Cfg) Validate() error {
	if c.Conns <= 0 {
		return fmt.Errorf("Conns must be positive")
	}
	return nil
}
`
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cfg.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &ingest.Snapshot{
		Root: root,
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
	// Cfg.Conns documents "0 means auto-detect" — cfSentinelDocRe matches "means".
	// Even though Validate rejects zero, the field doc declares zero a sentinel.
	// No lead should be produced.
	if len(cfLeads) != 0 {
		t.Errorf("normalize-then-validate: want 0 leads, got %d: %+v", len(cfLeads), cfLeads)
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
