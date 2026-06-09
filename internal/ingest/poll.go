package ingest

import (
	"context"
	"fmt"
	"strings"
)

// Commit is a lightweight description of a git commit surfaced by polling.
type Commit struct {
	SHA     string
	Summary string // first line of the commit message
}

// PollResult is the outcome of a single Poll. HeadSHA is the tip the poller
// observed this round; the caller persists it and passes it back as lastSeen on
// the next call. NewCommits lists commits between lastSeen (exclusive) and
// HeadSHA (inclusive), oldest first; it is empty when nothing changed.
type PollResult struct {
	HeadSHA    string
	NewCommits []Commit
}

// HasNew reports whether the poll observed any new commits.
func (p PollResult) HasNew() bool { return len(p.NewCommits) > 0 }

// Poller detects new commits on a repository. It performs no looping or
// scheduling of its own: the daemon calls Poll once per tick and owns the
// interval/backoff policy. A Poller is safe for concurrent use.
//
// Two modes are auto-detected per call:
//
//   - Remote-backed: if the current branch has an upstream (or a remote
//     exists), Poll runs `git fetch` and compares the remote-tracking tip to
//     lastSeen, so commits pushed to the remote are detected even before they
//     are merged into the local working branch.
//   - Local-only: with no remote, Poll compares the local HEAD to lastSeen,
//     detecting commits made directly in the working tree.
type Poller struct {
	repo *Repo
	// remoteName is the remote to fetch from in remote mode (default "origin").
	remoteName string
}

// NewPoller returns a Poller for the repository. remoteName selects which
// remote to consult in remote-backed mode; pass "" to default to "origin".
func NewPoller(repo *Repo, remoteName string) *Poller {
	if remoteName == "" {
		remoteName = "origin"
	}
	return &Poller{repo: repo, remoteName: remoteName}
}

// Poll detects commits newer than lastSeen. On the very first call lastSeen is
// typically "" (no watermark yet): in that case the current tip is reported as
// HeadSHA with an empty NewCommits list, establishing a baseline without
// replaying all of history. Pass the returned HeadSHA back as lastSeen next
// time.
func (p *Poller) Poll(ctx context.Context, lastSeen string) (PollResult, error) {
	tip, err := p.resolveTip(ctx)
	if err != nil {
		return PollResult{}, err
	}

	// No prior watermark: establish a baseline silently.
	if lastSeen == "" {
		return PollResult{HeadSHA: tip}, nil
	}
	// Unchanged.
	if lastSeen == tip {
		return PollResult{HeadSHA: tip}, nil
	}

	commits, err := p.commitsBetween(ctx, lastSeen, tip)
	if err != nil {
		// lastSeen may be unknown to this clone (e.g. history rewrite); fall
		// back to reporting just the tip so the daemon makes progress.
		return PollResult{HeadSHA: tip, NewCommits: []Commit{{SHA: tip, Summary: p.summaryOf(ctx, tip)}}}, nil
	}
	return PollResult{HeadSHA: tip, NewCommits: commits}, nil
}

// resolveTip returns the SHA the poller should track this round. In
// remote-backed mode it fetches and reads the upstream/remote-tracking tip; in
// local-only mode it reads local HEAD.
func (p *Poller) resolveTip(ctx context.Context) (string, error) {
	hasRemote, err := p.hasRemote(ctx)
	if err != nil {
		return "", err
	}
	if !hasRemote {
		return p.repo.HeadCommit(ctx)
	}

	// Fetch quietly; ignore fetch failure (offline) and fall through to
	// whatever remote-tracking ref we already have, so transient network
	// problems do not stall polling.
	_, _ = runGit(ctx, p.repo.root, "fetch", "--quiet", p.remoteName)

	if tip, err := p.upstreamTip(ctx); err == nil && tip != "" {
		return tip, nil
	}
	// No configured upstream for the current branch: fall back to the remote's
	// HEAD branch, then to local HEAD as a last resort.
	if tip, err := runGit(ctx, p.repo.root, "rev-parse", p.remoteName+"/HEAD"); err == nil {
		if s := strings.TrimSpace(tip); s != "" {
			return s, nil
		}
	}
	return p.repo.HeadCommit(ctx)
}

// upstreamTip resolves the SHA of the current branch's configured upstream
// (@{upstream}), if any.
func (p *Poller) upstreamTip(ctx context.Context) (string, error) {
	out, err := runGit(ctx, p.repo.root, "rev-parse", "@{upstream}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hasRemote reports whether the repository has at least one configured remote.
func (p *Poller) hasRemote(ctx context.Context) (bool, error) {
	out, err := runGit(ctx, p.repo.root, "remote")
	if err != nil {
		return false, fmt.Errorf("ingest: list remotes: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// commitsBetween lists commits in (from, to], oldest first, with summaries.
func (p *Poller) commitsBetween(ctx context.Context, from, to string) ([]Commit, error) {
	// %x00 separates SHA from summary; records are newline-separated. Using a
	// NUL field separator avoids ambiguity if a summary somehow contains odd
	// characters. --reverse yields oldest-first.
	out, err := runGit(ctx, p.repo.root,
		"log", "--reverse", "--format=%H%x00%s",
		from+".."+to)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		sha, summary, _ := strings.Cut(line, "\x00")
		if sha == "" {
			continue
		}
		commits = append(commits, Commit{SHA: sha, Summary: summary})
	}
	return commits, nil
}

// summaryOf returns the subject line of a commit, or "" on error.
func (p *Poller) summaryOf(ctx context.Context, sha string) string {
	out, err := runGit(ctx, p.repo.root, "log", "-1", "--format=%s", sha)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
