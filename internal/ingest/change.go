package ingest

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
)

// ChangeKind classifies how a file changed between two commits.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// Change is a single path-level change between two commits.
//
// Invariant: OldPath is non-empty if and only if Kind == ChangeRenamed.
// Use NewChange or NewRename to produce a valid Change; use Validate to check
// one received from an external source.
type Change struct {
	Kind ChangeKind
	// Path is the post-change path (the new name for renames/additions, the
	// removed path for deletions).
	Path string
	// OldPath is set only for renames: the pre-change path. It is always empty
	// for ChangeAdded, ChangeModified, and ChangeDeleted.
	OldPath string
}

// NewChange constructs a non-rename Change. OldPath is always empty.
// Kind must be ChangeAdded, ChangeModified, or ChangeDeleted.
func NewChange(kind ChangeKind, path string) Change {
	return Change{Kind: kind, Path: path}
}

// NewRename constructs a rename Change with both paths populated.
func NewRename(oldPath, newPath string) Change {
	return Change{Kind: ChangeRenamed, Path: newPath, OldPath: oldPath}
}

// Validate reports an error when the Change violates the OldPath invariant:
// OldPath must be non-empty iff Kind is ChangeRenamed.
func (c Change) Validate() error {
	if c.Kind == ChangeRenamed && c.OldPath == "" {
		return fmt.Errorf("ingest: renamed change for %q must have OldPath set", c.Path)
	}
	if c.Kind != ChangeRenamed && c.OldPath != "" {
		return fmt.Errorf("ingest: non-rename change (kind=%s, path=%q) must not have OldPath=%q", c.Kind, c.Path, c.OldPath)
	}
	return nil
}

// ChangedFiles returns the set of changes between two commits, oldest first.
// fromCommit and toCommit may be any git revision (SHA, branch, tag, HEAD~3,
// etc.). Rename detection is enabled, so a moved file is reported once as a
// ChangeRenamed with both OldPath and Path populated rather than as a
// delete+add pair.
//
// Output is parsed from `git diff --name-status -z -M`, whose NUL-delimited
// framing keeps paths with spaces or newlines intact and disambiguates the
// two-path records that renames and copies produce.
//
// "--end-of-options" is inserted before fromCommit so a ref that starts with
// "-" cannot be parsed as a flag by git. See CommitMessage below for the
// rationale.
func (r *Repo) ChangedFiles(ctx context.Context, fromCommit, toCommit string) ([]Change, error) {
	if fromCommit == "" || toCommit == "" {
		return nil, fmt.Errorf("ingest: ChangedFiles requires both commits (from=%q to=%q)", fromCommit, toCommit)
	}
	raw, err := runGitRaw(ctx, r.root,
		"diff", "--name-status", "-z", "-M", "--end-of-options", fromCommit, toCommit)
	if err != nil {
		return nil, fmt.Errorf("ingest: diff %s..%s: %w", fromCommit, toCommit, err)
	}
	changes, err := parseNameStatusZ(raw)
	if err != nil {
		return nil, fmt.Errorf("ingest: parse diff %s..%s: %w", fromCommit, toCommit, err)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// parseNameStatusZ parses the NUL-delimited output of
// `git diff --name-status -z`.
//
// The stream is a flat sequence of NUL-terminated fields. Each record begins
// with a status field, then:
//   - A/M/D/T: one path field follows.
//   - R/C (rename/copy): a similarity-suffixed status (e.g. "R100") followed by
//     TWO path fields, old then new.
//
// We walk the fields with an index, consuming the right number per record.
func parseNameStatusZ(b []byte) ([]Change, error) {
	fields := splitNULAll(b)
	var changes []Change
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		switch status[0] {
		case 'A', 'M', 'T':
			if i >= len(fields) {
				return nil, fmt.Errorf("status %q missing path", status)
			}
			kind := ChangeModified
			if status[0] == 'A' {
				kind = ChangeAdded
			}
			changes = append(changes, NewChange(kind, fields[i]))
			i++
		case 'D':
			if i >= len(fields) {
				return nil, fmt.Errorf("status %q missing path", status)
			}
			changes = append(changes, NewChange(ChangeDeleted, fields[i]))
			i++
		case 'R', 'C':
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("status %q missing old/new paths", status)
			}
			oldPath, newPath := fields[i], fields[i+1]
			i += 2
			// A copy leaves the original in place; model it as an addition of
			// the new path so dependents of the copy are still scoped. A rename
			// is reported as such with both paths.
			if status[0] == 'C' {
				changes = append(changes, NewChange(ChangeAdded, newPath))
			} else {
				changes = append(changes, NewRename(oldPath, newPath))
			}
		default:
			// Unknown status (e.g. "U" unmerged): treat conservatively as a
			// modification of the single following path if present.
			if i < len(fields) {
				changes = append(changes, Change{Kind: ChangeModified, Path: fields[i]})
				i++
			}
		}
	}
	return changes, nil
}

// UnifiedDiff returns the raw unified diff between two commits using the
// symmetric-difference form `git diff from...to`, which compares to against the
// merge base of from and to. This is the diff GitHub anchors PR review comments
// against, so callers that need to know which lines are commentable on a PR
// should parse this output rather than the two-dot `from..to` diff.
//
// Rename detection is enabled (-M) so a moved-and-edited file's hunks carry its
// new path in the "+++ b/<newpath>" header.
//
// "--end-of-options" is inserted before the joined from...to range so a
// from-ref that starts with "-" cannot be parsed as a flag by git (the
// leading character of the joined range follows `from` verbatim). See
// CommitMessage below for the rationale.
func (r *Repo) UnifiedDiff(ctx context.Context, from, to string) ([]byte, error) {
	if from == "" || to == "" {
		return nil, fmt.Errorf("ingest: UnifiedDiff requires both commits (from=%q to=%q)", from, to)
	}
	raw, err := runGitRaw(ctx, r.root, "diff", "-M", "--end-of-options", from+"..."+to)
	if err != nil {
		return nil, fmt.Errorf("ingest: unified diff %s...%s: %w", from, to, err)
	}
	return raw, nil
}

// CommitMessage returns the full commit message for the given revision (any
// git ref: SHA, HEAD, branch, tag). It shells out to `git log -1 --format=%B`
// following the same pattern as ChangedFiles and UnifiedDiff.
//
// "--end-of-options" is inserted before commit so a ref that starts with "-"
// cannot be parsed as a flag by git. This follows the same defence-in-depth
// pattern used throughout the ingest package (git ≥ 2.24 supports the marker;
// older git without it falls back to treating a dash-ref as an unknown option
// and errors rather than silently misinterpreting it).
func (r *Repo) CommitMessage(ctx context.Context, commit string) (string, error) {
	if commit == "" {
		return "", fmt.Errorf("ingest: CommitMessage requires a non-empty commit ref")
	}
	out, err := runGit(ctx, r.root, "log", "-1", "--format=%B", "--end-of-options", commit)
	if err != nil {
		return "", fmt.Errorf("ingest: commit message %s: %w", commit, err)
	}
	return strings.TrimRight(out, "\n"), nil
}

// ReadFileAtRef returns the raw bytes of path as it appears at the given git
// ref. ref may be any revision (SHA, branch, tag, HEAD~3). When the file does
// not exist at ref — git exits non-zero with "fatal: path 'path' does not
// exist (neither on disk nor in the index)" or "fatal: path 'path' exists on
// disk, but not in '<ref>'" — the underlying error is returned unchanged so
// callers can treat the absence as a normal outcome (the regress attribution
// path labels a finding INTRODUCED when its anchored file is absent at the
// range's base ref).
//
// "--end-of-options" is inserted before the joined ref:path argument so a ref
// that starts with "-" cannot be parsed as a flag by git. This follows the
// same defence-in-depth pattern as ChangedFiles / UnifiedDiff / CommitMessage.
func (r *Repo) ReadFileAtRef(ctx context.Context, ref, path string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("ingest: ReadFileAtRef requires a non-empty ref")
	}
	if path == "" {
		return nil, fmt.Errorf("ingest: ReadFileAtRef requires a non-empty path")
	}
	raw, err := runGitRaw(ctx, r.root, "show", "--end-of-options", ref+":"+path)
	if err != nil {
		return nil, fmt.Errorf("ingest: show %s:%s: %w", ref, path, err)
	}
	return raw, nil
}

// AnchorAbsentAtRef reports whether the file:line anchor did not exist at the
// given git ref. It is the attribution primitive behind `bugbot regress` and
// the daemon's regress digest: an anchor absent at a base ref was introduced
// after it. It returns true ("absent at ref") in these cases:
//
//  1. The file is absent at ref (ReadFileAtRef errors for a path untracked at
//     ref).
//  2. The anchored line is past the file's EOF at ref (line < 1 or
//     line > number-of-lines at ref).
//  3. Any other git error, treated conservatively as "absent" so a transient
//     failure biases toward the investigate-first INTRODUCED bucket rather than
//     a spurious PRE-EXISTING label.
//
// A nil receiver, an empty file, or an empty ref also return true (the same
// conservative default).
func (r *Repo) AnchorAbsentAtRef(ctx context.Context, ref, file string, line int) bool {
	if r == nil || file == "" || ref == "" {
		return true
	}
	content, err := r.ReadFileAtRef(ctx, ref, file)
	if err != nil {
		return true
	}
	if line < 1 {
		return true
	}
	// 1-indexed line count. Split on '\n' so a file without a trailing newline
	// still has its last line present ("a\nb" => 2 lines); a trailing newline
	// adds a spurious empty final element, which we drop ("a\nb\n" => 2 lines).
	lines := bytes.Split(content, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return line > len(lines)
}

// ChangedPaths is a convenience that flattens a Change slice to the set of
// post-change paths plus any rename sources, which is the natural input to
// BlastRadius (both the new and old locations of a renamed file are relevant).
func ChangedPaths(changes []Change) []string {
	seen := make(map[string]bool, len(changes))
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, c := range changes {
		add(c.Path)
		add(c.OldPath)
	}
	sortStrings(out)
	return out
}
