package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ghRunner runs a `gh` CLI invocation and returns its stdout. All GitHub API
// interaction in review mode routes through this single seam so that:
//   - authentication is gh's problem (we never touch tokens or raw HTTP),
//   - owner/repo are auto-filled by gh from the repo's origin (no resolution
//     code here), and
//   - tests can inject a fake that routes on args and returns canned JSON.
//
// args are the bare gh arguments (e.g. "api", "repos/{owner}/{repo}/pulls/3").
type ghRunner func(ctx context.Context, args ...string) ([]byte, error)

// realGH is the default ghRunner: it shells out to the `gh` binary on PATH,
// mirroring ingest.runGitRaw's stderr-into-error pattern so failures carry gh's
// own diagnostic (auth errors, 404s, rate limits) rather than a bare exit code.
func realGH(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", scrubNUL(args)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return []byte(stdout.String()), nil
}

// scrubNUL removes NUL bytes from each gh argument. A NUL byte (0x00) makes the
// OS reject the exec call with EINVAL ("invalid argument") before the process
// even starts: Go's forkExec validates every argv element via
// syscall.BytePtrFromString, which returns EINVAL on the first string holding a
// NUL. Model-authored text (issue/comment bodies, titles) occasionally carries
// a stray NUL, which would otherwise abort the entire publish/review run. The
// common case — no NUL in any argument — returns args unchanged with zero
// allocation.
func scrubNUL(args []string) []string {
	dirty := false
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			dirty = true
			break
		}
	}
	if !dirty {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "\x00", "")
	}
	return out
}

// prInfo is the subset of the GitHub pull-request payload review mode needs.
type prInfo struct {
	Number  int
	BaseSHA string
	HeadSHA string
	HeadRef string
}

// resolvePR fetches the PR via `gh api repos/{owner}/{repo}/pulls/N`. gh
// auto-fills {owner}/{repo} from the origin remote, so no owner/repo resolution
// happens here. It parses the base/head SHAs and the head ref.
func resolvePR(ctx context.Context, gh ghRunner, number int) (prInfo, error) {
	raw, err := gh(ctx, "api", fmt.Sprintf("repos/{owner}/{repo}/pulls/%d", number))
	if err != nil {
		return prInfo{}, fmt.Errorf("resolve PR #%d: %w", number, err)
	}

	var payload struct {
		Number int `json:"number"`
		Base   struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return prInfo{}, fmt.Errorf("parse PR #%d payload: %w", number, err)
	}
	if payload.Base.SHA == "" || payload.Head.SHA == "" {
		return prInfo{}, fmt.Errorf("PR #%d payload missing base/head sha", number)
	}

	return prInfo{
		Number:  number,
		BaseSHA: payload.Base.SHA,
		HeadSHA: payload.Head.SHA,
		HeadRef: payload.Head.Ref,
	}, nil
}
