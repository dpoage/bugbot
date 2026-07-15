package repro

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestAttempt_RejectsPlanRequiringUnavailableEcosystem is regression 6(c): a
// plan whose cmd requires an unavailable ecosystem (npx/vitest on a node-less
// image) must be rejected BEFORE any sandbox launch, with feedback naming the
// missing toolchain, and the agent must get a revision request — never a bare
// environment_error indistinguishable from a real infra failure.
func TestAttempt_RejectsPlanRequiringUnavailableEcosystem(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedTSFinding(t, st)
	repoDir := newRepoDir(t)

	badPlan := Plan{
		Files:  map[string]string{"app.test.ts": "test('x', () => { throw new Error('boom'); });\n"},
		Cmd:    []string{"npx", "vitest", "run", "app.test.ts"},
		Expect: "the bug manifests as a failing vitest assertion",
	}
	goodPlanJS := Plan{
		Files:  map[string]string{"app.test.ts": "test('x', () => { throw new Error('boom'); });\n"},
		Cmd:    []string{"npx", "vitest", "run", "app.test.ts"},
		Expect: "the bug manifests as a failing vitest assertion",
	}
	// First round proposes the unavailable-ecosystem plan; second round (after
	// revision feedback) proposes the same plan again — the test only cares
	// that round 1 never reaches the sandbox, not that round 2 succeeds.
	client := newScriptedClient(planBody(t, badPlan), planBody(t, goodPlanJS))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: nodeUnavailableCaps(), // js/node: false
		MaxAttempts:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	if att.Promoted {
		t.Fatal("Attempt should not promote: every round proposed an unavailable-ecosystem plan")
	}
	if !strings.Contains(att.Reason, "blocked_toolchain") {
		t.Errorf("Attempt.Reason = %q, want it to mention blocked_toolchain", att.Reason)
	}
	if !strings.Contains(att.Reason, "js") {
		t.Errorf("Attempt.Reason = %q, want it to name the missing ecosystem (js)", att.Reason)
	}

	// The sandbox must never have been launched: both rounds proposed a plan
	// requiring "js", which was unavailable both times.
	if n := sb.CallCount(); n != 0 {
		t.Errorf("sandbox CallCount = %d, want 0 — a plan requiring an unavailable ecosystem must never launch the sandbox", n)
	}

	// The revision feedback sent back to the model must name the missing
	// toolchain, not a bare environment_error.
	reqs := client.allRequests()
	if len(reqs) < 2 {
		t.Fatalf("want at least 2 model requests (initial + revision), got %d", len(reqs))
	}
	lastTask := client.taskText(len(reqs) - 1)
	if !strings.Contains(lastTask, "js") {
		t.Errorf("revision request should name the missing ecosystem 'js', got: %s", lastTask)
	}
	if strings.Contains(strings.ToLower(lastTask), "environment_error") {
		t.Error("revision feedback must not be a bare environment_error — the agent must be told WHICH toolchain is missing")
	}
}

// TestRejectUnavailableEcosystemPlan_ListsAvailableAlternatives verifies the
// helper names both the missing toolchain and any available alternatives.
func TestRejectUnavailableEcosystemPlan_ListsAvailableAlternatives(t *testing.T) {
	caps := sandbox.CapabilitySet{
		"js":     {"node": false},
		"python": {"python": true},
	}
	plan := &Plan{Cmd: []string{"npx", "vitest", "run"}}
	msg := rejectUnavailableEcosystemPlan(plan, caps)
	if msg == "" {
		t.Fatal("want non-empty rejection message for an unavailable js plan")
	}
	if !strings.Contains(msg, "js") {
		t.Errorf("message must name the missing ecosystem: %s", msg)
	}
	if !strings.Contains(msg, "python") {
		t.Errorf("message must list the available alternative (python): %s", msg)
	}
}

// TestRejectUnavailableEcosystemPlan_NoAlternatives verifies the no-alternatives
// branch still names the missing toolchain and never panics on an empty caps set.
func TestRejectUnavailableEcosystemPlan_NoAlternatives(t *testing.T) {
	caps := sandbox.CapabilitySet{"js": {"node": false}}
	plan := &Plan{Cmd: []string{"node", "--test"}}
	msg := rejectUnavailableEcosystemPlan(plan, caps)
	if msg == "" || !strings.Contains(msg, "js") {
		t.Errorf("want a message naming js with no alternatives, got %q", msg)
	}
}

// TestRejectUnavailableEcosystemPlan_UngatedCommandsPass verifies commands
// with no gated ecosystem (go test, make) and an available ecosystem's
// commands are never rejected.
func TestRejectUnavailableEcosystemPlan_UngatedCommandsPass(t *testing.T) {
	caps := sandbox.CapabilitySet{"js": {"node": false}}
	cases := [][]string{
		{"go", "test", "./..."},
		{"make", "test"},
	}
	for _, cmd := range cases {
		if msg := rejectUnavailableEcosystemPlan(&Plan{Cmd: cmd}, caps); msg != "" {
			t.Errorf("cmd %v: want no rejection (ungated), got %q", cmd, msg)
		}
	}
	if msg := rejectUnavailableEcosystemPlan(&Plan{Cmd: []string{"pytest"}}, sandbox.CapabilitySet{"python": {"python": true}}); msg != "" {
		t.Errorf("available ecosystem must not be rejected, got %q", msg)
	}
	if msg := rejectUnavailableEcosystemPlan(&Plan{Cmd: []string{"pytest"}}, nil); msg != "" {
		t.Errorf("nil CapabilitySet must never reject, got %q", msg)
	}
}

// TestRejectUnavailableEcosystemPlan_BazelGated pins bugbot-rj3z: a plan
// reaching for the bazel build driver on a sandbox whose probe reports bazel
// absent is rejected PRE-LAUNCH with feedback naming bazel and the available
// language toolchains — instead of burning a sandbox run into an exit-127
// environment_error (the_cloud: `sh: line 2: exec: bazel: not found`). When
// the probe reports bazel present, the same plan proceeds untouched.
func TestRejectUnavailableEcosystemPlan_BazelGated(t *testing.T) {
	caps := sandbox.CapabilitySet{
		"bazel":  {"bazel": false},
		"python": {"python": true},
	}
	for _, cmd := range [][]string{
		{"bazel", "test", "//molecules/robot-control:all"},
		{"bazelisk", "test", "//..."},
		{"sh", "-c", "bazel test //... --test_output=errors"},
	} {
		msg := rejectUnavailableEcosystemPlan(&Plan{Cmd: cmd}, caps)
		if msg == "" {
			t.Errorf("cmd %v: want pre-launch rejection when bazel is unavailable", cmd)
			continue
		}
		if !strings.Contains(msg, "bazel") {
			t.Errorf("cmd %v: feedback must name bazel, got %q", cmd, msg)
		}
		if !strings.Contains(msg, "python") {
			t.Errorf("cmd %v: feedback must list the available python alternative, got %q", cmd, msg)
		}
	}

	available := sandbox.CapabilitySet{"bazel": {"bazel": true}}
	if msg := rejectUnavailableEcosystemPlan(&Plan{Cmd: []string{"bazel", "test", "//..."}}, available); msg != "" {
		t.Errorf("bazel-available sandbox must not reject bazel plans, got %q", msg)
	}
}
