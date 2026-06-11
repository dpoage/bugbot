package eval

// TestDiffIntentFixture_CatchesIntentGap is the eval fixture for the
// diff-intent lens. It materializes a two-commit repository where:
//
//   - The first commit adds a validate() call that a caller relies on.
//   - The second commit's message says "add input validation" but the diff
//     REMOVES the validate() call — a classic intent-vs-implementation gap.
//
// The test asserts that the diff-intent lens surfaces a finding for this
// regression. It does NOT assert that taxonomy lenses miss it: the eval
// harness only supports Sweep (no ChangeContext), and verifying "missed by
// taxonomy" would require running the taxonomy lenses on the same fixture in
// Targeted mode without ChangeContext. That is a separate test shape the
// harness cannot cheaply express today. The positive assertion (diff-intent
// catches it) is the load-bearing claim.
//
// LIMITATION: the eval harness calls Sweep, which ignores ChangeContext.
// diff-intent fires only on Targeted runs with a non-nil ChangeContext. This
// fixture therefore drives Targeted directly via the lower-level
// runDiffIntentCase helper, bypassing the Sweep-centric RunCase path.
import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// diffIntentFixtureSrc is the base source file: a service that validates its
// input, and a caller that relies on that validation.
const diffIntentFixtureSrc = `package service

// Validate checks that input is non-empty before use.
func Validate(input string) bool {
	return input != ""
}

// Process validates input and then performs the operation.
// Callers assume Validate is called here — they do NOT call it themselves.
func Process(input string) string {
	if !Validate(input) {
		return ""
	}
	return "processed: " + input
}
`

// diffIntentFixtureSrcBad is the "bad" version: the commit message says
// "add input validation" but the diff removes the Validate call from Process.
// Callers that relied on Process calling Validate are now exposed.
const diffIntentFixtureSrcBad = `package service

// Validate checks that input is non-empty before use.
func Validate(input string) bool {
	return input != ""
}

// Process performs the operation WITHOUT validating input first.
// The validate call was removed, breaking callers that depended on it.
func Process(input string) string {
	return "processed: " + input
}
`

// callerSrc is a caller that relies on Process performing validation.
const callerSrc = `package service

// UseProcess calls Process, relying on it to validate before use.
// If Process skips validation, this caller is vulnerable to empty input.
func UseProcess(raw string) string {
	return Process(raw) // no local Validate call — trusts Process to validate
}
`

// TestDiffIntentFixture_CatchesIntentGap materializes a two-commit repo,
// builds a ChangeContext from the diff, and runs a Targeted scan with a
// scripted finder that emits a diff-intent candidate. It asserts the candidate
// survives triage and verification.
func TestDiffIntentFixture_CatchesIntentGap(t *testing.T) {
	requireGit(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	ctx := context.Background()

	// Materialize the base commit.
	dir, err := materializeTwo(t,
		map[string]string{
			"service.go": diffIntentFixtureSrc,
			"caller.go":  callerSrc,
		},
		"add input validation",
		map[string]string{
			"service.go": diffIntentFixtureSrcBad,
		},
	)
	if err != nil {
		t.Fatalf("materialize two-commit repo: %v", err)
	}
	defer cleanup(dir)

	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	// Resolve the two commits.
	fromSHA := gitOutput(t, dir, "rev-parse", "HEAD~1")
	toSHA := gitOutput(t, dir, "rev-parse", "HEAD")

	// Build the ChangeContext the diff-intent lens needs.
	msg, err := repo.CommitMessage(ctx, toSHA)
	if err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	diff, err := repo.UnifiedDiff(ctx, fromSHA, toSHA)
	if err != nil {
		t.Fatalf("UnifiedDiff: %v", err)
	}
	changes, err := repo.ChangedFiles(ctx, fromSHA, toSHA)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	changed := ingest.ChangedPaths(changes)
	cc := &funnel.ChangeContext{
		FromCommit:   fromSHA,
		ToCommit:     toSHA,
		Message:      msg,
		Diff:         diff,
		ChangedFiles: changed,
		// BlastFiles intentionally absent: derived from targets inside hypothesize.
	}

	// Scripted finder: diff-intent returns a candidate for the validation
	// removal; taxonomy lenses return nothing.
	const intentGapTitle = "validation removed despite commit message claiming to add it"
	diffIntentCand := Candidates(CandidateJSON{
		File:        "service.go",
		Line:        9,
		Title:       intentGapTitle,
		Description: "Process() no longer calls Validate(); callers in caller.go rely on it",
		Severity:    "high",
		Evidence:    "diff removes Validate call from Process; caller.go has no local guard",
		Confidence:  "high",
	})
	finder := NewScriptedClient().OnSystemContains("diff-intent", diffIntentCand)
	verifier := NewScriptedClient().OnTaskContains(intentGapTitle, NotRefutedJSON)

	// Open a fresh store.
	st, err := store.Open(ctx, fixtureDBPath(dir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	f, err := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier}, st, repo,
		funnel.Options{
			MaxParallel:   1,
			ChangeContext: cc,
		})
	if err != nil {
		t.Fatalf("construct funnel: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.Targeted(ctx, changed)
	if err != nil {
		t.Fatalf("Targeted: %v", err)
	}

	// The diff-intent lens must have surfaced the intent gap.
	if res.Stats.Hypothesized == 0 {
		t.Error("diff-intent lens hypothesized nothing; expected at least one candidate")
	}

	found := false
	for _, fnd := range res.Findings {
		if fnd.Lens == "diff-intent" {
			found = true
			if !strings.Contains(fnd.Title, "validation removed") {
				t.Errorf("finding title = %q, want something about validation removal", fnd.Title)
			}
		}
	}
	if !found {
		t.Errorf("no diff-intent finding persisted; stats=%+v findings=%+v", res.Stats, res.Findings)
	}

	// Commit message must contain the intent.
	if !strings.Contains(msg, "add input validation") {
		t.Errorf("commit message should describe adding validation, got: %q", msg)
	}
}

// --- helpers ----------------------------------------------------------------

// materializeTwo creates a scratch git repo with TWO commits:
//   - First commit: initialFiles with message "seed".
//   - Second commit: changes (only the listed files overwritten) with commitMsg.
//
// It returns the absolute path to the repo root.
func materializeTwo(t *testing.T, initialFiles map[string]string, commitMsg string, changes map[string]string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "bugbot-eval-diffintent-*")
	if err != nil {
		return "", err
	}

	write := func(rel, content string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(content), 0o644)
	}
	runGit := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=bugbot-eval", "GIT_AUTHOR_EMAIL=eval@bugbot.test",
			"GIT_COMMITTER_NAME=bugbot-eval", "GIT_COMMITTER_EMAIL=eval@bugbot.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return &gitErr{args: args, out: string(out), err: err}
		}
		return nil
	}

	if err := runGit("init", "-q"); err != nil {
		cleanup(dir)
		return "", err
	}
	for rel, content := range initialFiles {
		if err := write(rel, content); err != nil {
			cleanup(dir)
			return "", err
		}
	}
	if err := runGit("-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "add", "."); err != nil {
		cleanup(dir)
		return "", err
	}
	if err := runGit("-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "commit", "-q", "-m", "seed"); err != nil {
		cleanup(dir)
		return "", err
	}

	// Second commit: apply changes.
	for rel, content := range changes {
		if err := write(rel, content); err != nil {
			cleanup(dir)
			return "", err
		}
	}
	if err := runGit("-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "add", "."); err != nil {
		cleanup(dir)
		return "", err
	}
	if err := runGit("-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "commit", "-q", "-m", commitMsg); err != nil {
		cleanup(dir)
		return "", err
	}
	return dir, nil
}

// gitOutput runs a git command in dir and returns trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// gitErr records a failed git invocation.
type gitErr struct {
	args []string
	out  string
	err  error
}

func (e *gitErr) Error() string {
	return "git " + strings.Join(e.args, " ") + ": " + e.err.Error() + "\n" + e.out
}
