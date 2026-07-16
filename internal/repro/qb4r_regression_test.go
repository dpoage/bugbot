package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestAttempt_QB4R covers bugbot-qb4r acceptance criterion 4: a source-grep
// test, an import-absence lint test, and a transliteration must never
// promote, while a genuine behavioral failing test still does. MaxAttempts
// is pinned to 1 so a rejected round never needs a second scripted plan.
func TestAttempt_QB4R(t *testing.T) {
	repoDir := newRepoDir(t)

	cases := []struct {
		name            string
		findingFile     string
		plan            Plan
		sandboxOut      sandbox.Result
		wantPromote     bool
		wantReason      VerdictReason
		wantWitnessOnly bool
	}{
		{
			name:        "source grep test never promotes",
			findingFile: "src/store/SelectedContractStore.ts",
			plan: Plan{
				Files: map[string]string{"test_grep.py": `
import unittest

class T(unittest.TestCase):
    def test_uses_get_value(self):
        with open("src/store/SelectedContractStore.ts") as f:
            src = f.read()
        self.assertIn("SelectedContractStore.getValue()", src)
`},
				Cmd:    []string{"python3", "-m", "pytest", "test_grep.py"},
				Expect: "source-grep test",
			},
			wantPromote: false,
			wantReason:  VerdictReasonTargetNotExecuted,
		},
		{
			name:        "import-absence lint test never promotes",
			findingFile: "agent/main.py",
			plan: Plan{
				Files: map[string]string{"test_lint.py": `
def test_main_does_not_use_threading():
    with open("agent/main.py") as f:
        src = f.read()
    assert "threading.Lock" not in src
`},
				Cmd:    []string{"python3", "-m", "pytest", "test_lint.py"},
				Expect: "import-absence lint",
			},
			wantPromote: false,
			wantReason:  VerdictReasonTargetNotExecuted,
		},
		{
			name:        "transliteration never promotes",
			findingFile: "src/scheduler/timeInTask.ts",
			plan: Plan{
				Files: map[string]string{"repro.py": `
def time_in_task(start, now):
    return now - start  # mirrors src/scheduler/timeInTask.ts

if __name__ == "__main__":
    import sys
    result = time_in_task(float("inf"), 0)
    if result == float("-inf"):
        print("BUGBOT_REPRO_DEMONSTRATED")
        sys.exit(1)
`},
				Cmd:    []string{"./repro.py"},
				Expect: "transliteration",
			},
			sandboxOut: sandbox.Result{
				ExitCode: 1,
				Stdout:   "BUGBOT_REPRO_DEMONSTRATED\n",
			},
			// ./repro.py (direct script execution, no recognized launcher
			// token) detects as ecosystem.EcosystemUnknown, which has no
			// coverage-report format at all — the transliteration is caught
			// via the OTHER half of acceptance-3's "rejected or downgraded
			// to witness-only": it is not statically rejected (a bare
			// script isn't in executableEdgeCheckers), but the unknown
			// ecosystem can never provide a witness, so it downgrades
			// instead of reaching unmarked full Tier-1. bugbot-ds90 made
			// bare "python3 repro.py" resolve to EcosystemPython (which
			// DOES have a coverage format), so this fixture switched to a
			// launcher shape that still has none.
			wantPromote:     true,
			wantWitnessOnly: true,
		},
		{
			name:        "genuine behavioral test promotes",
			findingFile: "calc.go",
			plan: Plan{
				Files:  map[string]string{"bug_test.go": "package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},
				Cmd:    []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."},
				Expect: "genuine behavioral failure",
			},
			sandboxOut: sandbox.Result{
				ExitCode: 1,
				Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
			},
			wantPromote: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newScriptedClient(planBody(t, tc.plan))
			sb := sandbox.NewMock(sandbox.MockResponse{Result: tc.sandboxOut})
			r, err := New(client, sb, repoDir, Options{MaxAttempts: 1, ArtifactDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			finding := domain.Finding{
				ID:          "f-" + tc.name,
				Fingerprint: domain.Fingerprint("logic", tc.findingFile, tc.name),
				Title:       tc.name,
				Tier:        2,
				File:        tc.findingFile,
			}
			att, err := r.Attempt(context.Background(), finding)
			if err != nil {
				t.Fatalf("Attempt: %v", err)
			}
			if att.Promoted != tc.wantPromote {
				t.Fatalf("Promoted = %v, want %v (att=%+v)", att.Promoted, tc.wantPromote, att)
			}
			if tc.wantReason != "" && att.Reason != string(tc.wantReason) {
				t.Errorf("Reason = %q, want %q", att.Reason, tc.wantReason)
			}
			if att.WitnessOnly != tc.wantWitnessOnly {
				t.Errorf("WitnessOnly = %v, want %v", att.WitnessOnly, tc.wantWitnessOnly)
			}
		})
	}
}
