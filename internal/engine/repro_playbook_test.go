package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// nopLLMClient is a minimal llm.Client stub satisfying repro.New's non-nil
// check. Its Complete is never expected to be called by these tests (they
// only exercise construction — PlaybookOnce wiring — not a live agent run).
type nopLLMClient struct{}

func (nopLLMClient) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, nil
}
func (nopLLMClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

var _ llm.Client = nopLLMClient{}

// TestBuildReproducerWithSandbox_WiresPlaybookOnce is the production-
// reachability regression for bugbot-u2v5 (oracle review defect #1):
// PlaybookOnce must actually be invoked as part of building a real
// reproducer, not merely available behind an Options field nobody sets. It
// asserts on the Mock's recorded calls that a playbook battery probe
// ("go version", the canonical Go launcher probe — distinct in argv shape
// from ecosystem.goCapabilityProbe's `/bin/sh -c "command -v go ..."`) ran
// against the SAME sandbox instance buildReproducerWithSandbox constructs
// the reproducer with.
func TestBuildReproducerWithSandbox_WiresPlaybookOnce(t *testing.T) {
	repoDir := t.TempDir()
	writeTestFile(t, filepath.Join(repoDir, "go.mod"), "module example.com/x\n\ngo 1.22\n")

	cfg := testConfig(t)
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "go1.22"}})

	r, err := buildReproducerWithSandbox(ctx, &cfg, st, repoDir, sb, nil, nopLLMClient{})
	if err != nil {
		t.Fatalf("buildReproducerWithSandbox: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	found := false
	for _, call := range sb.Calls() {
		if len(call.Spec.Cmd) == 2 && call.Spec.Cmd[0] == "go" && call.Spec.Cmd[1] == "version" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildReproducerWithSandbox must run the playbook battery (a `go version` probe) against sb; calls = %+v", sb.Calls())
	}
}
