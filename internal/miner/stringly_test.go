package miner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// --------------------------------------------------------------------------
// Unit tests: passStringConsumers
// --------------------------------------------------------------------------

func TestPassStringConsumers_CaseBasic(t *testing.T) {
	content := `package x

func handle(s string) {
	switch s {
	case "active":
		// ok
	case "inactive":
		// ok
	}
}
`
	sites := passStringConsumers("test.go", content)
	got := map[string]bool{}
	for _, s := range sites {
		got[s.literal] = true
	}
	for _, want := range []string{"active", "inactive"} {
		if !got[want] {
			t.Errorf("expected consumer literal %q", want)
		}
	}
}

func TestPassStringConsumers_SkipsLineComments(t *testing.T) {
	content := `package x
// case "should-be-skipped":
func f(s string) {
	switch s {
	case "real-case":
	}
}
`
	sites := passStringConsumers("test.go", content)
	for _, s := range sites {
		if s.literal == "should-be-skipped" {
			t.Errorf("line-comment literal leaked into consumers: %q", s.literal)
		}
	}
	found := false
	for _, s := range sites {
		if s.literal == "real-case" {
			found = true
		}
	}
	if !found {
		t.Error("expected real-case in consumers")
	}
}

func TestPassStringConsumers_SkipsBlockComments(t *testing.T) {
	content := `package x
/*
 * case "block-comment-case":
 */
func f(s string) {
	switch s {
	case "real-case":
	}
}
`
	sites := passStringConsumers("test.go", content)
	for _, s := range sites {
		if s.literal == "block-comment-case" {
			t.Errorf("block-comment literal leaked into consumers: %q", s.literal)
		}
	}
	found := false
	for _, s := range sites {
		if s.literal == "real-case" {
			found = true
		}
	}
	if !found {
		t.Error("expected real-case in consumers")
	}
}

func TestPassStringConsumers_StoplistFiltered(t *testing.T) {
	// "get", "post", "true" are in the stringyStoplist and should not be consumers.
	content := `package x
func f(s string) {
	switch s {
	case "get":
	case "post":
	case "true":
	case "real-event":
	}
}
`
	sites := passStringConsumers("test.go", content)
	for _, s := range sites {
		switch s.literal {
		case "get", "post", "true":
			t.Errorf("stoplist literal leaked into consumers: %q", s.literal)
		}
	}
	found := false
	for _, s := range sites {
		if s.literal == "real-event" {
			found = true
		}
	}
	if !found {
		t.Error("expected real-event to pass stoplist filter")
	}
}

func TestPassStringConsumers_IdentifierShape(t *testing.T) {
	content := `package x
func f(s string) {
	switch s {
	case "has space":
	case "with%format":
	case "valid-slug":
	case "valid_snake":
	}
}
`
	sites := passStringConsumers("test.go", content)
	rejected := []string{"has space", "with%format"}
	accepted := []string{"valid-slug", "valid_snake"}
	lits := map[string]bool{}
	for _, s := range sites {
		lits[s.literal] = true
	}
	for _, r := range rejected {
		if lits[r] {
			t.Errorf("non-identifier literal should not be a consumer: %q", r)
		}
	}
	for _, a := range accepted {
		if !lits[a] {
			t.Errorf("identifier-shaped literal should be a consumer: %q", a)
		}
	}
}

// TestPassStringConsumers_SwitchID verifies that cases in the same switch block
// share a switchID, and cases in different blocks get different switchIDs.
func TestPassStringConsumers_SwitchID(t *testing.T) {
	content := `package x
func f(s, t string) {
	switch s {
	case "alpha":
	case "beta":
	}
	switch t {
	case "gamma":
	}
}
`
	sites := passStringConsumers("test.go", content)
	byLit := map[string]int{}
	for _, s := range sites {
		byLit[s.literal] = s.switchID
	}
	if byLit["alpha"] != byLit["beta"] {
		t.Errorf("alpha and beta should share switchID: alpha=%d beta=%d", byLit["alpha"], byLit["beta"])
	}
	if byLit["alpha"] == byLit["gamma"] {
		t.Errorf("alpha and gamma should have different switchIDs")
	}
}

// --------------------------------------------------------------------------
// Unit tests: passStringProducers
// --------------------------------------------------------------------------

func TestPassStringProducers_BasicReturn(t *testing.T) {
	content := `package x

func statusFor(code int) string {
	switch code {
	case 1:
		return "active"
	case 2:
		return "inactive"
	}
	return "pending"
}
`
	sites := passStringProducers("test.go", content)
	want := map[string]bool{"active": true, "inactive": true, "pending": true}
	got := map[string]bool{}
	for _, s := range sites {
		got[s.literal] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("expected producer literal %q", w)
		}
	}
}

func TestPassStringProducers_Assignment(t *testing.T) {
	content := `package x

func f() {
	x := "order-created"
	y = "order-fulfilled"
	_ = x
	_ = y
}
`
	sites := passStringProducers("test.go", content)
	got := map[string]bool{}
	for _, s := range sites {
		got[s.literal] = true
	}
	if !got["order-created"] {
		t.Error("expected order-created in producers")
	}
	if !got["order-fulfilled"] {
		t.Error("expected order-fulfilled in producers")
	}
}

// --------------------------------------------------------------------------
// Unit tests: isIdentifierShaped
// --------------------------------------------------------------------------

func TestIsIdentifierShaped(t *testing.T) {
	accept := []string{
		"active", "inactive", "order-created", "user_status",
		"OrderPlaced", "some-slug", "camelCase",
	}
	reject := []string{
		"ab",          // too short (< minStringyLen)
		"has space",   // spaces
		"with%format", // format verb
		"1234",        // pure digits
		"/path",       // leading slash
	}
	for _, s := range accept {
		if !isIdentifierShaped(s) {
			t.Errorf("isIdentifierShaped(%q) = false, want true", s)
		}
	}
	for _, s := range reject {
		if isIdentifierShaped(s) {
			t.Errorf("isIdentifierShaped(%q) = true, want false", s)
		}
	}
}

// --------------------------------------------------------------------------
// Unit tests: minimum length guard
// --------------------------------------------------------------------------

func TestStringlyDrift_MinLengthGuard(t *testing.T) {
	// Short literals should be filtered by isIdentifierShaped before reaching any lead.
	consumers := passStringConsumers("t.go", `package x
func f(s string) {
	switch s {
	case "ok":
	case "no":
	}
}`)
	for _, c := range consumers {
		if c.literal == "ok" || c.literal == "no" {
			t.Errorf("short literal %q should be filtered by minStringyLen", c.literal)
		}
	}
}

// --------------------------------------------------------------------------
// Integration tests via Seed
// --------------------------------------------------------------------------

// TestStringlyDrift_PositiveFixture verifies that a case with a typo literal
// that does not match any const value of the type is flagged with EXACTLY ONE
// lead (type-A: raw literal not in const set), while correctly-matched cases
// produce no additional leads.
//
// testdata/stringly_drift/typo_case.go has:
//   - type Status string with consts "active", "inactive", "pending"
//   - switch on Status with case "activ" (typo), "inactive", "pending"
//
// Expected: one lead for "activ" (literal not matching any const of Status).
func TestStringlyDrift_PositiveFixture(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_drift")
	snap := buildSnapshot(t, dir, []string{"typo_case.go"})
	st := openStore(t)

	ctx := context.Background()
	sum, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	// Only count stringly-drift leads about the "activ" typo.
	var typoLeads []string
	for _, l := range leads {
		if l.PosterLens != stringlyPosterLens {
			continue
		}
		if strings.Contains(l.Note, `"activ"`) {
			typoLeads = append(typoLeads, l.Note)
		}
	}

	if len(typoLeads) != 1 {
		t.Errorf("want exactly 1 stringly-drift lead for typo 'activ', got %d; sum=%+v; all leads=%+v",
			len(typoLeads), sum, leads)
	}
	if sum.StringlyDriftLeads == 0 {
		t.Errorf("StringlyDriftLeads = 0, want > 0")
	}
}

// TestStringlyDrift_NegativeFixture verifies that when every case literal in a
// switch exactly matches a const value of the named string type, zero
// stringly-drift leads are emitted.
//
// testdata/stringly_clean/clean_switch.go has type Status string with consts
// "active", "inactive", "pending" and a switch where all cases match.
func TestStringlyDrift_NegativeFixture(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"clean_switch.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []string
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l.Note)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("want 0 stringly-drift leads for clean fixture, got %d: %v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_Determinism verifies that two identical Seed runs produce
// the same lead set in the same order.
func TestStringlyDrift_Determinism(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_drift")
	snap := buildSnapshot(t, dir, []string{"typo_case.go"})

	ctx := context.Background()

	st1 := openStore(t)
	_, err := Seed(ctx, snap, st1)
	if err != nil {
		t.Fatalf("Seed run1: %v", err)
	}
	leads1, err := st1.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads run1: %v", err)
	}

	st2 := openStore(t)
	_, err = Seed(ctx, snap, st2)
	if err != nil {
		t.Fatalf("Seed run2: %v", err)
	}
	leads2, err := st2.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads run2: %v", err)
	}

	if len(leads1) != len(leads2) {
		t.Fatalf("non-deterministic: run1=%d leads, run2=%d leads", len(leads1), len(leads2))
	}
	for i := range leads1 {
		l1, l2 := leads1[i], leads2[i]
		if l1.File != l2.File || l1.Line != l2.Line || l1.Note != l2.Note {
			t.Errorf("lead[%d] differs:\n  run1=%+v\n  run2=%+v", i, l1, l2)
		}
	}
}

// TestStringlyDrift_RawStringSwitchProducesNoLeads verifies that a switch over
// a raw (untyped) string parameter produces zero stringly-drift leads by
// construction: the closed-enum model only analyzes switches whose scrutinee
// resolves to a named string type in the same file.
func TestStringlyDrift_RawStringSwitchProducesNoLeads(t *testing.T) {
	// This switch decodes OpenAI stop reasons — the scrutinee is a plain
	// `string` parameter, not a named string type. No type+const set exists,
	// so passEnumSwitches returns nothing and no leads are emitted.
	content := `package x

func handleOpenAI(reason string) {
	switch reason {
	case "stop":
		// natural stop
	case "tool_calls":
		// function call requested
	case "content_filter":
		// filtered
	}
}
`
	namedTypes := passNamedStringTypes(content)
	switches := passEnumSwitches("t.go", content, namedTypes)
	if len(switches) != 0 {
		t.Errorf("raw-string switch should produce 0 enumSwitches, got %d", len(switches))
	}
}

// --------------------------------------------------------------------------
// Regression tests for the three oracle-found defects
// --------------------------------------------------------------------------

// TestStringlyDrift_D1_ScopeSpoof verifies that a raw-string switch on a
// parameter named "mode" in routeCommand does NOT emit leads just because
// a different function (handleMode) has a typed Mode param with the same name.
// Before the fix, resolveScrutineeType scanned the whole file and could match
// the Mode type to the raw-string switch.
func TestStringlyDrift_D1_ScopeSpoof(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_drift")
	snap := buildSnapshot(t, dir, []string{"spoof_scope.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("D1: want 0 stringly-drift leads on spoof_scope.go, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_D2_DefaultSuppressesTypeB verifies that a typed-enum switch
// with explicit cases AND a default: clause emits zero type-B (missing-arm) leads.
// The explicit-subset + default idiom is valid and must not be flagged.
func TestStringlyDrift_D2_DefaultSuppressesTypeB(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"default_clause.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("D2: want 0 stringly-drift leads on default_clause.go (has default:), got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_D3_DeterminismMultiUncovered verifies that when a switch
// has >=2 uncovered enum values, the note emitted is stable across repeated
// Seed runs. This catches the map-range nondeterminism: without sorting,
// which uncovered value appears in the lead's note flips between runs.
// The fixture has 3 uncovered values (blue, green, yellow); after sorting,
// "blue" must always be reported first/only.
func TestStringlyDrift_D3_DeterminismMultiUncovered(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_drift")
	snap := buildSnapshot(t, dir, []string{"multi_uncovered.go"})

	ctx := context.Background()

	const runs = 20
	var firstNotes []string

	for run := 0; run < runs; run++ {
		st := openStore(t)
		_, err := Seed(ctx, snap, st)
		if err != nil {
			t.Fatalf("run %d Seed: %v", run, err)
		}
		leads, err := st.PendingLeads(ctx, stringlyTargetLens)
		if err != nil {
			t.Fatalf("run %d PendingLeads: %v", run, err)
		}
		var notes []string
		for _, l := range leads {
			if l.PosterLens == stringlyPosterLens {
				notes = append(notes, l.Note)
			}
		}
		if run == 0 {
			firstNotes = notes
			// The fixture has 3 uncovered values; at least 1 lead must be emitted.
			if len(firstNotes) < 1 {
				t.Fatalf("D3: want >= 1 type-B lead from multi_uncovered.go, got 0")
			}
			// After sorting, "blue" is the lexicographically first uncovered value
			// and must appear in the note (not "green" or "yellow").
			if !strings.Contains(firstNotes[0], `"blue"`) {
				t.Errorf("D3: expected sorted-first uncovered value \"blue\" in note, got: %s", firstNotes[0])
			}
			continue
		}
		if len(notes) != len(firstNotes) {
			t.Fatalf("D3: run %d got %d leads, run 0 got %d (non-deterministic count)", run, len(notes), len(firstNotes))
		}
		for i := range firstNotes {
			if notes[i] != firstNotes[i] {
				t.Errorf("D3: run %d lead[%d] differs:\n  run0=%q\n  run%d=%q", run, i, firstNotes[i], run, notes[i])
			}
		}
	}
}

// TestStringlyDrift_ClosureShadowsOuterTypedParam verifies that a closure whose
// parameter shadows an outer typed-enum parameter is NOT flagged. The inner
// `mode string` is a raw string; the switch inside cannot be a typed-enum
// switch, so zero leads must be emitted.
//
// Repro: func outer(mode Mode) func(string) { return func(mode string) { switch mode { case "docker":; case "podman": } } }
func TestStringlyDrift_ClosureShadowsOuterTypedParam(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"closure_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ClosureShadow: want 0 stringly-drift leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ClosureOverOuterTypedParam verifies that a closure that
// captures (but does NOT shadow) an outer typed-enum parameter still fires.
// The closure has no params of its own; `m` resolves to the outer `m Mode`.
// The case "activ" is a typo of "active" → exactly 1 type-A lead.
// "inactive" is never covered → exactly 1 type-B lead.
// Expected total: 2 stringly-drift leads.
func TestStringlyDrift_ClosureOverOuterTypedParam(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_drift")
	snap := buildSnapshot(t, dir, []string{"closure_outer_typed.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	// Exactly 2 leads: type-A for "activ" (typo) + type-B for "inactive" (missing arm).
	if len(stringlyLeads) != 2 {
		t.Errorf("ClosureOuterTyped: want exactly 2 stringly-drift leads (1 type-A + 1 type-B), got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaShortDeclClosure verifies that a closure where
// the scrutinee is rebound via := (not typed as the enum) produces ZERO leads.
// Repro C: func outer(mode Mode){ handler:=func(){ mode:=fetchCmd(); switch mode { ... } }; handler() }
func TestStringlyDrift_ShadowViaShortDeclClosure(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"short_decl_closure_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaShortDeclClosure: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaShortDeclNestedBlock verifies that a nested block
// where the scrutinee is rebound via := (not typed as the enum) produces ZERO
// leads (no closure involved — plain if-block).
// Repro D: func handle(mode Mode){ if true { mode:=normalize(); switch mode { ... } } }
func TestStringlyDrift_ShadowViaShortDeclNestedBlock(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"short_decl_nested_block_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaShortDeclNestedBlock: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaVarTyped verifies that a nested block where the
// scrutinee is declared via `var name Type` with a non-enum type produces ZERO
// leads (case E).
// Repro E: func handle(mode Mode){ if true { var mode string = getCmd(); switch mode {case "docker":case "podman":} } }
func TestStringlyDrift_ShadowViaVarTyped(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"var_typed_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaVarTyped: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaVarInferred verifies that a nested block where the
// scrutinee is declared via `var name = expr` (inferred type, non-enum) produces
// ZERO leads (case F).
// Repro F: func handle(mode Mode){ if true { var mode = getCmd(); switch mode {case "docker":case "podman":} } }
func TestStringlyDrift_ShadowViaVarInferred(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"var_inferred_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaVarInferred: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaForRange verifies that a for-range loop where the
// loop variable shadows an outer typed-enum parameter produces ZERO leads.
// Repro 1: func h(mode Mode, items map[string]int){ for mode := range items { switch mode{case "docker":case "podman":} } }
func TestStringlyDrift_ShadowViaForRange(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"for_range_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaForRange: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaGroupedVar verifies that a grouped var declaration
// `var mode, other string = ...` that shadows an outer typed-enum parameter
// produces ZERO leads.
// Repro 2: func h(mode Mode){ if true { var mode, other string = getCmd(),""; switch mode{...} } }
func TestStringlyDrift_ShadowViaGroupedVar(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"grouped_var_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaGroupedVar: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_ShadowViaNonFirstShortDecl verifies that when the scrutinee
// appears as a non-first name in a short declaration (`a, mode := ...`), it is
// detected as a shadow and produces ZERO leads.
// Repro 3: func h(mode Mode){ if true { a, mode := 1, getCmd(); switch mode{...} } }
func TestStringlyDrift_ShadowViaNonFirstShortDecl(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"non_first_short_decl_shadow.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("ShadowViaNonFirstShortDecl: want 0 leads, got %d: %+v",
			len(stringlyLeads), stringlyLeads)
	}
}

// --------------------------------------------------------------------------
// Regression tests for production findings 1, 2, and 3
// --------------------------------------------------------------------------

// TestStringlyDrift_BraceInCaseStringNoFalseLead verifies that a `}` inside a
// string literal in a case body does not decrement braceDepth and pop the
// switch early (Finding 1). Both cases must be recognized; no false type-B.
//
// testdata/stringly_clean/brace_in_case_string.go:
//   - type BraceMode string with consts "a", "b"
//   - switch with case "a" (body has s := "}") and case "b"
//
// Before the fix: the `}` inside the string pops the switch → "b" dropped →
// false type-B lead for missing "b". After the fix: 0 leads.
func TestStringlyDrift_BraceInCaseStringNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"brace_in_case_string.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("BraceInCaseString: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_WrappedCaseArmNoFalseLead verifies that a gofmt-preserved
// wrapped case list (case expression split across two lines) is fully recognized
// and does not produce false type-B leads (Finding 2).
//
// testdata/stringly_clean/wrapped_case_arm.go:
//   - type WrappedMode string with consts "a", "b", "c"
//   - switch with case "a", then case "b",\n\t\t"c":
//
// Before the fix: "b" and "c" dropped → false type-B for both. After: 0 leads.
func TestStringlyDrift_WrappedCaseArmNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"wrapped_case_arm.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("WrappedCaseArm: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_EnumTypeInCommentNotResolved verifies that a `name EnumType`
// word-pair that appears only inside a string literal or a trailing // comment
// does not cause a raw-string scrutinee to resolve to the enum type (Finding 3).
//
// testdata/stringly_clean/enum_type_in_comment.go:
//   - type CommentMode string with consts "run", "stop"
//   - logMode(cmd string): string contains "cmd CommentMode: " and comment says
//     "cmd CommentMode changed" — raw string switch must produce 0 leads.
//
// Before the fix: varDeclRe matches the word-pair in the string/comment →
// scrutinee resolved to CommentMode → false leads for missing arms.
// After the fix: 0 leads.
func TestStringlyDrift_EnumTypeInCommentNotResolved(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"enum_type_in_comment.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("EnumTypeInComment: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_RuneWithQuoteNoFalseLead verifies that a `"` inside a rune
// literal (`'"'`) does NOT open a fake double-quoted string that would blank the
// following `{` and pop the switch early (rune-literal regression).
//
// testdata/stringly_clean/rune_with_quote.go:
//   - type RuneQuoteMode string with consts "a", "b"
//   - switch m { case "a": if c == '"' { c = 0 } case "b": }
//
// Before sanitizeLine rune-tracking: '"' opens a fake string, blanks `{` →
// braceDepth desync → switch pops early → false type-B for missing "b".
// After: 0 leads.
func TestStringlyDrift_RuneWithQuoteNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"rune_with_quote.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("RuneWithQuote: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_RuneWithBraceNoFalseLead verifies that a `}` inside a rune
// literal (`'}'`) is NOT counted by the brace counter, which would pop the
// switch early (rune-literal regression).
//
// testdata/stringly_clean/rune_with_brace.go:
//   - type RuneBraceMode string with consts "a", "b"
//   - switch m { case "a": if r == '}' { r = 0 } case "b": }
//
// Before sanitizeLine rune-tracking: '}' rune is counted as a closing brace →
// braceDepth desync → switch pops early → false type-B for missing "b".
// After: 0 leads.
func TestStringlyDrift_RuneWithBraceNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"rune_with_brace.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("RuneWithBrace: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// --------------------------------------------------------------------------
// Regression tests for single-line backtick raw strings (oracle repros)
// --------------------------------------------------------------------------

// TestStringlyDrift_SingleLineRawStringBraceNoFalseLead verifies that a `}`
// inside a single-line backtick raw string in a case body does NOT decrement
// braceDepth and pop the switch early. Both cases must be recognized; 0 leads.
//
// testdata/stringly_clean/backtick_brace.go:
//   - type BacktickBraceMode string with consts "a", "b"
//   - switch with case "a" (body has s := `}`) and case "b"
//
// Before sanitizeLine backtick-tracking: `}` inside raw string is counted as a
// closing brace → braceDepth desync → switch pops early → false type-B for "b".
// After: 0 leads.
func TestStringlyDrift_SingleLineRawStringBraceNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"backtick_brace.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("SingleLineRawStringBrace: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_SingleLineRawStringWordPairNoFalseLead verifies that a
// `cmd BacktickMode` word-pair inside a single-line backtick raw string does
// NOT cause a raw-string scrutinee switch to resolve to the named enum type.
// Exactly 1 lead expected: the genuine missing-arm for "pause" (BacktickModePause
// is declared but absent from the switch). Without backtick tracking, the
// word-pair inside the raw string would also falsely resolve the raw-string
// switch → additional false leads; with tracking, only the real lead fires.
//
// testdata/stringly_clean/backtick_wordpair.go:
//   - type BacktickMode string with consts "run", "stop", "pause"
//   - func with msg := `cmd BacktickMode` (raw string), then switch cmd (string)
//     with cases "run" and "stop" only (no "pause" case)
//
// Before sanitizeLine backtick-tracking: varDeclRe matches the word-pair in
// the raw string → raw-string scrutinee resolved to BacktickMode → false leads
// on top of the real "pause" lead.
// After: exactly 1 stringly lead (missing "pause" arm).
func TestStringlyDrift_SingleLineRawStringWordPairNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"backtick_wordpair.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	// Exactly 1 genuine lead: missing "pause" arm in the typed-enum switch.
	// Without backtick tracking the word-pair would also fire on the raw-string
	// scrutinee, producing additional false leads; backtick tracking suppresses them.
	if len(stringlyLeads) != 1 {
		t.Errorf("SingleLineRawStringWordPair: want 1 lead (missing pause), got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// --------------------------------------------------------------------------
// Regression tests for single-line /* */ block comments (oracle repros)
// --------------------------------------------------------------------------

// TestStringlyDrift_InlineBlockCommentCaseNoFalseLead verifies that a single-line
// /* inline */ block comment on the same line as a case arm does NOT cause the
// case to be dropped (whole-line masking was replacing the line, dropping
// "case a"). Zero leads expected.
//
// testdata/stringly_clean/inline_block_comment_case.go:
//   - type InlineBlockCommentMode string with consts "a", "b"
//   - switch m { case "a": /* inline */ ; case "b": }
//
// Before: buildBlockCommentMask whole-line-masked the line → case "a" dropped →
// false type-B lead for missing "a". After sanitizeLine handles /* */:  0 leads.
func TestStringlyDrift_InlineBlockCommentCaseNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"inline_block_comment_case.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("InlineBlockCommentCase: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}

// TestStringlyDrift_InlineBlockCommentBraceNoFalseLead verifies that a single-line
// /* inline */ block comment inside an if-block body does NOT desync the brace
// counter. Both cases must be recognized; zero leads expected.
//
// testdata/stringly_clean/inline_block_comment_brace.go:
//   - type InlineBlockCommentBraceMode string with consts "a", "b"
//   - switch m { case "a": if cond { /* inline */ } case "b": }
//
// Before: buildBlockCommentMask whole-line-masked the line → the `{` before
// /* was dropped → braceDepth desync → switch popped early → false type-B for
// missing "b". After sanitizeLine handles /* */: 0 leads.
func TestStringlyDrift_InlineBlockCommentBraceNoFalseLead(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_clean")
	snap := buildSnapshot(t, dir, []string{"inline_block_comment_brace.go"})
	st := openStore(t)

	ctx := context.Background()
	_, err := Seed(ctx, snap, st)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, stringlyTargetLens)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}

	var stringlyLeads []store.Lead
	for _, l := range leads {
		if l.PosterLens == stringlyPosterLens {
			stringlyLeads = append(stringlyLeads, l)
		}
	}
	if len(stringlyLeads) != 0 {
		t.Errorf("InlineBlockCommentBrace: want 0 leads, got %d: %+v", len(stringlyLeads), stringlyLeads)
	}
}
