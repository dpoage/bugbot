package repro

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// Writable local_mounts (bugbot-wjc2) must reach sandbox.Spec.RWMounts on
// BOTH repro execution paths: the official buildSpec/execute() run and the
// workspace-exec iteration preview. A mount that reaches only one path would
// make the preview diverge from the verdict (the bugbot-0zay failure class):
// e.g. a bazel vendor dir writable in iteration but read-only in the official
// run aborts analysis only at submission time.

func rwMountFixture() []sandbox.ROMount {
	return []sandbox.ROMount{{HostPath: "/data/vendor", ContainerPath: "/bazel-vendor", Shared: true}}
}

func TestBuildSpec_CarriesRWMounts(t *testing.T) {
	plan := &Plan{Cmd: []string{"bazelisk", "test", "//pkg:all"}}
	spec := buildSpec(t.TempDir(), plan, "", "none", time.Minute, sandbox.Resolution{
		RWMounts: rwMountFixture(),
	})
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != "/bazel-vendor" {
		t.Fatalf("official-run Spec.RWMounts = %+v, want the /bazel-vendor writable mount", spec.RWMounts)
	}
}

func TestWorkspaceTool_ExecCarriesRWMounts(t *testing.T) {
	repoDir := t.TempDir()
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	ws := &iterationWorkspace{}
	tool := NewWorkspaceTool(sb, repoDir, "", 30*time.Second, nil, rwMountFixture(), nil, nil, sb.MaterializeWorkspace, ws, 3)

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"argv":["exec","true"]}`)); err != nil {
		t.Fatalf("exec: %v", err)
	}
	calls := sb.Calls()
	if len(calls) == 0 {
		t.Fatal("exec never reached the sandbox")
	}
	got := calls[len(calls)-1].Spec.RWMounts
	if len(got) != 1 || got[0].ContainerPath != "/bazel-vendor" {
		t.Fatalf("preview Spec.RWMounts = %+v, want the /bazel-vendor writable mount", got)
	}
}
