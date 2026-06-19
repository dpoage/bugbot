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

func TestPassDocumented_BasicAndTrailingCases(t *testing.T) {
	src := `package x

type Cfg struct {
	// Foo is the thing. 0 = unlimited.
	Foo int

	// Bar cap. Zero means no limit.
	Bar int

	// Baz is the legacy cap.
	// Negative disables.
	Baz int

	// Qux is unrelated.
	Qux int
}
`
	docs := passDocumented("cfg.go", src)
	classes := map[string]sentinelClass{}
	for _, d := range docs {
		classes[d.entity] = d.sClass
	}
	if classes["Foo"] != sentinelZero {
		t.Errorf("Foo sClass = %q, want %q", classes["Foo"], sentinelZero)
	}
	if classes["Bar"] != sentinelZero {
		t.Errorf("Bar sClass = %q, want %q", classes["Bar"], sentinelZero)
	}
	if classes["Baz"] != sentinelNegative {
		t.Errorf("Baz sClass = %q, want %q (got doc entities: %+v)", classes["Baz"], sentinelNegative, classes)
	}
	if _, ok := classes["Qux"]; ok {
		t.Errorf("Qux should not be a doc site, got %+v", classes)
	}
	if _, ok := classes["Negative"]; ok {
		t.Errorf("Negative (from comment text) should not be a doc site entity, got %+v", classes)
	}
}

func TestPassEnforced_BasicAndSnakeEntity(t *testing.T) {
	src := `package x

import "fmt"

func ValidateWidget(c int) error {
	if c <= 0 {
		return fmt.Errorf("widget_limit must be > 0")
	}
	return nil
}

func ValidatePort(s string) error {
	if s == "" {
		return fmt.Errorf("default_port must not be empty")
	}
	return nil
}

func LoopBound(n int) int {
	// 'if n <= 0' without an early return is a loop bound, not a validator.
	if n <= 0 {
		n = 1
	}
	return n
}
`
	cons := passEnforced("v.go", src)
	entityClass := map[string]constraintClass{}
	for _, c := range cons {
		entityClass[c.entity] = c.cClass
	}
	if entityClass["widget_limit"] != constraintRejectsZero {
		t.Errorf("widget_limit cClass = %q, want %q (entity from error string)", entityClass["widget_limit"], constraintRejectsZero)
	}
	if entityClass["default_port"] != constraintRejectsEmpty {
		t.Errorf("default_port cClass = %q, want %q (entity from error string)", entityClass["default_port"], constraintRejectsEmpty)
	}
	if _, ok := entityClass["n"]; ok {
		t.Errorf("loop-bound 'n <= 0' must NOT be a constraint site, got %+v", entityClass)
	}
}

func TestJoin_PrecisionIsTight(t *testing.T) {
	docZero := docSite{
		entity: "Foo", sClass: sentinelZero,
		entities: []string{"Foo", "foo"},
	}
	docEmpty := docSite{
		entity: "Bar", sClass: sentinelEmpty,
		entities: []string{"Bar", "bar"},
	}
	conZero := constraintSite{
		entity: "foo", cClass: constraintRejectsZero,
		entities: []string{"foo", "Foo"},
	}
	conEmpty := constraintSite{
		entity: "bar", cClass: constraintRejectsEmpty,
		entities: []string{"bar", "Bar"},
	}
	if !sentinelContradictsDoc(docZero.sClass, conZero.cClass) {
		t.Error("sentinelContradictsDoc(zero, zero) = false, want true")
	}
	if !sentinelContradictsDoc(docEmpty.sClass, conEmpty.cClass) {
		t.Error("sentinelContradictsDoc(empty, empty) = false, want true")
	}
	if sentinelContradictsDoc(docZero.sClass, conEmpty.cClass) {
		t.Error("sentinelContradictsDoc(zero, empty) = true, want false (different value classes)")
	}
	if sentinelContradictsDoc(docEmpty.sClass, conZero.cClass) {
		t.Error("sentinelContradictsDoc(empty, zero) = true, want false (different value classes)")
	}
	if !entityOverlap(docZero.entities, conZero.entities) {
		t.Error("entityOverlap(Foo, foo) = false, want true (CamelCase <-> snake_case)")
	}
	if !entityOverlap(docEmpty.entities, conEmpty.entities) {
		t.Error("entityOverlap(Bar, bar) = false, want true")
	}
	if entityOverlap(docZero.entities, conEmpty.entities) {
		t.Error("entityOverlap(Foo, bar) = true, want false (different entities)")
	}
}

func TestSeed_PostsIg7Contradiction(t *testing.T) {
	dir := filepath.Join("testdata", "ig7_contradiction")
	snap := buildSnapshot(t, dir, []string{"widget_limit.go", "validator.go"})
	st := openStore(t)

	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sum.DocSites == 0 {
		t.Errorf("DocSites = 0, want >0")
	}
	if sum.ConstraintSites == 0 {
		t.Errorf("ConstraintSites = 0, want >0")
	}
	if sum.LeadsPosted != 1 {
		t.Errorf("LeadsPosted = %d, want 1 (exactly one ig7 contradiction)", sum.LeadsPosted)
	}

	leads, err := st.PendingLeads(ctx, "api-contract-misuse")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("PendingLeads(api-contract-misuse) = %d, want 1", len(leads))
	}
	l := leads[0]
	if l.PosterLens != "miner:doc-contradiction" {
		t.Errorf("PosterLens = %q, want miner:doc-contradiction", l.PosterLens)
	}
	if l.TargetLens != "api-contract-misuse" {
		t.Errorf("TargetLens = %q, want api-contract-misuse", l.TargetLens)
	}
	if l.File != "validator.go" {
		t.Errorf("File = %q, want validator.go (validator site, not doc site)", l.File)
	}
	if !strings.Contains(l.Note, "widget_limit") && !strings.Contains(l.Note, "WidgetLimit") {
		t.Errorf("Note does not name the entity: %q", l.Note)
	}
	if !strings.Contains(l.Note, "widget_limit.go") {
		t.Errorf("Note does not name the doc site: %q", l.Note)
	}
	if !strings.Contains(l.Note, "validator.go") {
		t.Errorf("Note does not name the validator site: %q", l.Note)
	}
}

func TestSeed_CleanFixtureProducesZeroLeads(t *testing.T) {
	dir := filepath.Join("testdata", "clean_no_contradiction")
	snap := buildSnapshot(t, dir, []string{"clean.go"})
	st := openStore(t)

	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sum.LeadsPosted != 0 {
		t.Errorf("LeadsPosted = %d, want 0 (clean fixture has no contradiction)", sum.LeadsPosted)
	}
	leads, err := st.PendingLeads(ctx, "api-contract-misuse")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 0 {
		t.Errorf("leads in clean fixture = %d, want 0", len(leads))
	}
}

func TestSeed_NilSnapshotErrors(t *testing.T) {
	st := openStore(t)
	if _, err := Seed(context.Background(), nil, st); err == nil {
		t.Error("Seed(nil snap) = nil error, want non-nil")
	}
	if _, err := Seed(context.Background(), &ingest.Snapshot{}, nil); err == nil {
		t.Error("Seed(nil store) = nil error, want non-nil")
	}
}

func TestSeed_RealBugbotRepo(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	files, err := collectGoFiles(repoRoot)
	if err != nil {
		t.Fatalf("collect go files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("collected 0 .go files under the bugbot repo; check the test path")
	}
	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   repoRoot,
		Files:  files,
	}

	st := openStore(t)
	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	t.Logf("real-repo summary: docSites=%d constraintSites=%d leadsPosted=%d",
		sum.DocSites, sum.ConstraintSites, sum.LeadsPosted)

	leads, err := st.PendingLeads(ctx, "api-contract-misuse")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	// NOTE: the live config.go bugbot-ig7 contradiction was fixed in the
	// real repo, so this run no longer posts that lead. Positive
	// regression coverage for the contradiction miner lives in
	// TestSeed_PostsIg7Contradiction (testdata/ig7_contradiction).
	for _, l := range leads {
		t.Logf("lead: file=%s line=%d note=%q", l.File, l.Line, l.Note)
	}
	const maxLeadsAllowed = 10
	if len(leads) > maxLeadsAllowed {
		t.Errorf("real-repo lead count = %d, want <= %d (precision over recall)", len(leads), maxLeadsAllowed)
	}
}

// --------------------------------------------------------------------------
// Enum / const-drift pass tests
// --------------------------------------------------------------------------

// TestPassConstDecls_IotaAndExplicit verifies that passConstDecls extracts
// both iota-based and explicit integer constant declarations.
func TestPassConstDecls_IotaAndExplicit(t *testing.T) {
	src := `package x

const (
	StatusPending  = iota
	StatusApproved
	StatusRejected
)

const MaxRetries = 3
const minRetries = 0
`
	decls := passConstDecls("f.go", src)
	byName := map[string]int64{}
	for _, d := range decls {
		byName[d.name] = d.value
	}
	// Exported names should be captured.
	if v, ok := byName["StatusPending"]; !ok || v != 0 {
		t.Errorf("StatusPending = %d, want 0", byName["StatusPending"])
	}
	if v, ok := byName["StatusApproved"]; !ok || v != 1 {
		t.Errorf("StatusApproved = %d, want 1", byName["StatusApproved"])
	}
	if v, ok := byName["StatusRejected"]; !ok || v != 2 {
		t.Errorf("StatusRejected = %d, want 2", byName["StatusRejected"])
	}
	if v, ok := byName["MaxRetries"]; !ok || v != 3 {
		t.Errorf("MaxRetries = %d, want 3", byName["MaxRetries"])
	}
	// Unexported constant should NOT be captured (regex requires uppercase start).
	if _, ok := byName["minRetries"]; ok {
		t.Errorf("minRetries should not be captured (not exported)")
	}
}

// TestPassSwitchCaseLiterals_Basic verifies extraction of raw integer case literals.
func TestPassSwitchCaseLiterals_Basic(t *testing.T) {
	src := `package x

func handle(n int) {
	switch n {
	case 0:
		return
	case 1:
		return
	case StatusDone: // named constant: should not match
		return
	}
}
`
	cases := passSwitchCaseLiterals("f.go", src)
	if len(cases) != 2 {
		t.Fatalf("expected 2 case literals, got %d: %+v", len(cases), cases)
	}
	vals := map[int64]bool{}
	for _, c := range cases {
		vals[c.value] = true
	}
	if !vals[0] || !vals[1] {
		t.Errorf("expected case values 0 and 1, got %+v", vals)
	}
}

// TestEnumDrift_DetectsContradiction verifies that the drift pass flags
// switch cases using raw integer literals that match named constants.
func TestEnumDrift_DetectsContradiction(t *testing.T) {
	dir := filepath.Join("testdata", "enum_drift")
	snap := buildSnapshot(t, dir, []string{"status_codes.go"})
	st := openStore(t)

	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sum.EnumDriftLeads < 1 {
		t.Errorf("EnumDriftLeads = %d, want >= 1 (case 0 and case 1 both match named constants)", sum.EnumDriftLeads)
	}

	leads, err := st.PendingLeads(ctx, enumDriftTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads(%s): %v", enumDriftTargetLens, err)
	}
	if len(leads) < 1 {
		t.Fatalf("expected at least 1 enum-drift lead, got 0")
	}
	// Verify attribution.
	for _, l := range leads {
		if l.PosterLens != enumDriftPosterLens {
			t.Errorf("PosterLens = %q, want %q", l.PosterLens, enumDriftPosterLens)
		}
		if l.TargetLens != enumDriftTargetLens {
			t.Errorf("TargetLens = %q, want %q", l.TargetLens, enumDriftTargetLens)
		}
		if !strings.Contains(l.Note, "enum-drift") {
			t.Errorf("Note missing 'enum-drift' prefix: %q", l.Note)
		}
	}
}

// TestEnumDrift_CleanFixtureProducesZeroLeads verifies that code using only
// named constants in switch cases produces no enum-drift leads (precision guard).
func TestEnumDrift_CleanFixtureProducesZeroLeads(t *testing.T) {
	dir := filepath.Join("testdata", "clean_enum")
	snap := buildSnapshot(t, dir, []string{"clean_codes.go"})
	st := openStore(t)

	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sum.EnumDriftLeads != 0 {
		t.Errorf("EnumDriftLeads = %d, want 0 (clean fixture uses named constants)", sum.EnumDriftLeads)
	}

	leads, err := st.PendingLeads(ctx, enumDriftTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads(%s): %v", enumDriftTargetLens, err)
	}
	if len(leads) != 0 {
		t.Errorf("expected 0 enum-drift leads, got %d: %+v", len(leads), leads)
	}
}

func buildSnapshot(t *testing.T, root string, rels []string) *ingest.Snapshot {
	t.Helper()
	files := make([]ingest.File, 0, len(rels))
	for _, rel := range rels {
		abs := filepath.Join(root, rel)
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("stat %s: %v", abs, err)
		}
		files = append(files, ingest.File{
			Path:     filepath.ToSlash(rel),
			Language: ingest.DetectLanguage(rel),
			Size:     fi.Size(),
		})
	}
	return &ingest.Snapshot{
		Commit: "test",
		Root:   root,
		Files:  files,
	}
}

func collectGoFiles(root string) ([]ingest.File, error) {
	var out []ingest.File
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, ingest.File{
			Path:     filepath.ToSlash(rel),
			Language: ingest.DetectLanguage(rel),
			Size:     info.Size(),
		})
		return nil
	})
	return out, err
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
