package tui

import (
	"strings"
	"testing"
	"time"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

func TestRenderTranscript_FailureAndDenial(t *testing.T) {
	failed := completionEv(2, time.Time{}, llmkit.Response{})
	failed.Completion.Err = "rate limited"
	denied := toolRunEv(1, time.Time{}, "sandbox_exec", "", false)
	denied.ToolRun.Denied, denied.ToolRun.DenyReason = true, "policy says no"
	out := renderTranscript(&agent.Transcript{Record: []llmkit.Event{denied, failed}})
	for _, want := range []string{"tool_result(sandbox_exec, denied): policy says no", "[step 2] completion failed: rate limited"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTranscript missing %q:\n%s", want, out)
		}
	}
}
