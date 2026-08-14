package engine

import (
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestApplyReranked refreshes matched findings' severity and verdict detail from
// the post-sweep set and leaves unmatched findings untouched. This is the
// oracle #3 fix: with the terminal Stage F removed from run(), a scan Result
// carries PRE-sweep severities, and the oneshot summary must reflect the
// re-ranked store state instead.
func TestApplyReranked(t *testing.T) {
	findings := []domain.Finding{
		{ID: "a", Severity: "high", VerdictDetail: "pre-a"},
		{ID: "b", Severity: "medium", VerdictDetail: "pre-b"},
		{ID: "c", Severity: "low", VerdictDetail: "untouched"},
	}
	reranked := map[string]domain.Finding{
		"a": {ID: "a", Severity: "low", VerdictDetail: "downranked: zero non-test callers"},
		"b": {ID: "b", Severity: "critical", VerdictDetail: "escalated"},
		// "c" intentionally absent: nothing re-ranked it this pass.
		"z": {ID: "z", Severity: "high", VerdictDetail: "not in the scan's result set"},
	}

	applyReranked(findings, reranked)

	if findings[0].Severity != "low" || findings[0].VerdictDetail != "downranked: zero non-test callers" {
		t.Errorf("finding a not refreshed from re-ranked set: %+v", findings[0])
	}
	if findings[1].Severity != "critical" || findings[1].VerdictDetail != "escalated" {
		t.Errorf("finding b not refreshed from re-ranked set: %+v", findings[1])
	}
	if findings[2].Severity != "low" || findings[2].VerdictDetail != "untouched" {
		t.Errorf("finding c (absent from re-ranked set) must be left untouched: %+v", findings[2])
	}
}

// TestSandboxRunOpts_HonorsResourceCaps pins the contract that the configured
// sandbox.cpus, sandbox.memory_mb, and sandbox.pids_limit actually reach the
// container backend. Regression: sandboxRunOpts threaded network/timeout/idle
// but dropped cpus/memory_mb, and pids_limit had no config knob at all — so
// every run kept the backend defaults (2 CPUs / 2048 MB / 256 pids). The 256
// pids cap crashed the Bazel JVM ("unable to create native thread") during
// analysis, so every Bazel-repo reproduction failed as environment_error.
// Distinct-from-default values prove the config — not the hardcoded backend
// default — flows through.
func TestSandboxRunOpts_HonorsResourceCaps(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.CPUs = 4
	cfg.Sandbox.MemoryMB = 8192
	cfg.Sandbox.PidsLimit = 4096

	sb := &sandbox.CLI{}
	for _, o := range sandboxRunOpts(cfg) {
		o(sb)
	}

	cpus, mem, pids := sb.Limits()
	if cpus != 4 {
		t.Errorf("cpus = %v, want 4 (sandbox.cpus dropped before reaching backend)", cpus)
	}
	if mem != 8192 {
		t.Errorf("memoryMB = %d, want 8192 (sandbox.memory_mb dropped before reaching backend)", mem)
	}
	if pids != 4096 {
		t.Errorf("pidsLimit = %d, want 4096 (sandbox.pids_limit dropped before reaching backend)", pids)
	}
}

// TestSandboxRunOpts_ExplicitZeroGrowthCeilingDisables pins bugbot-bdqf
// mutation M20: sandboxRunOpts/bwrapRunOpts wire ScratchSizeMB and
// WorkspaceGrowthCeilingMB UNCONDITIONALLY (no "if > 0" guard) specifically
// so an operator's explicit workspace_growth_ceiling_mb: 0 reaches the
// backend as a genuinely disabled (0) ceiling — NOT the backend's own
// non-zero built-in default (2 GiB), which an "if > 0" guard would have
// silently kept. Both backends are checked; scratch size is asserted
// nonzero (always propagated, config.Validate requires > 0) as a control.
func TestSandboxRunOpts_ExplicitZeroGrowthCeilingDisables(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.WorkspaceGrowthCeilingMB = 0
	cfg.Sandbox.ScratchSizeMB = 256

	// Seed a NON-zero baseline first, mirroring NewCLI/NewBwrap's own
	// built-in 2 GiB default — otherwise a bare &sandbox.CLI{} already
	// starts at the Go zero value (0), and the test could not distinguish
	// "sandboxRunOpts explicitly set it to 0" from "an `if > 0` guard
	// skipped the call, leaving whatever was already there" (both look
	// like 0 from an untouched zero-value struct).
	sb := &sandbox.CLI{}
	sandbox.WithWorkspaceGrowthCeilingMB(2048)(sb)
	for _, o := range sandboxRunOpts(cfg) {
		o(sb)
	}
	scratchMB, ceilingBytes := sb.ScratchAndGrowthCeiling()
	if ceilingBytes != 0 {
		t.Errorf("CLI growth ceiling = %d bytes, want 0 (explicit disable did not OVERRIDE the pre-existing non-zero default — an \"if > 0\" guard would silently skip the call)", ceilingBytes)
	}
	if scratchMB != 256 {
		t.Errorf("CLI scratch size = %d MB, want 256 (control: this knob must still propagate)", scratchMB)
	}

	bw := &sandbox.Bwrap{}
	sandbox.WithBwrapWorkspaceGrowthCeilingMB(2048)(bw)
	for _, o := range bwrapRunOpts(cfg) {
		o(bw)
	}
	bwScratchMB, bwCeilingBytes := bw.ScratchAndGrowthCeiling()
	if bwCeilingBytes != 0 {
		t.Errorf("Bwrap growth ceiling = %d bytes, want 0 (explicit disable did not override the pre-existing non-zero default)", bwCeilingBytes)
	}
	if bwScratchMB != 256 {
		t.Errorf("Bwrap scratch size = %d MB, want 256 (control)", bwScratchMB)
	}
}
