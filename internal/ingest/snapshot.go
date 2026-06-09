package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// readHeadBytes is how many bytes we sniff from the head of a file when
// applying the binary-content heuristic.
const readHeadBytes = 8 << 10 // 8 KiB

// File describes a single tracked, text, in-scope file in a snapshot.
type File struct {
	// Path is the repo-relative, forward-slash-separated path.
	Path string
	// Language is the coarse extension-derived classification.
	Language Language
	// Size is the file's size in bytes on disk at snapshot time.
	Size int64
}

// Snapshot is the file inventory of a repository at a single commit.
type Snapshot struct {
	// Commit is the resolved HEAD commit SHA the snapshot was taken at.
	Commit string
	// Files is the in-scope, text, tracked file set, sorted by Path.
	Files []File
}

// ScanFilter selects which tracked files belong in a snapshot. It mirrors the
// shape of config.Scan but is defined locally so this package does not couple
// to the config type's evolution. Empty Include is treated as "match all".
type ScanFilter struct {
	Include []string
	Exclude []string
}

// match reports whether a repo-relative path is in scope: it must match at
// least one Include pattern (or Include is empty) and no Exclude pattern.
func (f ScanFilter) match(path string) bool {
	if matchAny(f.Exclude, path) {
		return false
	}
	if len(f.Include) == 0 {
		return true
	}
	return matchAny(f.Include, path)
}

// Snapshot returns the inventory of tracked files at HEAD that pass the scan
// filter and are not binary. It uses `git ls-files`, which respects .gitignore
// and only lists tracked files, then layers the include/exclude globs and a
// binary-skip heuristic on top.
//
// Binary detection is two-stage: a known-binary extension table (cheap) and,
// for everything else, a null-byte sniff of the first 8 KiB (robust). Files
// that have been deleted from the working tree but are still tracked are
// skipped silently, since we cannot size or sniff them.
func (r *Repo) Snapshot(ctx context.Context, filter ScanFilter) (*Snapshot, error) {
	head, err := r.HeadCommit(ctx)
	if err != nil {
		return nil, err
	}

	paths, err := r.lsFiles(ctx)
	if err != nil {
		return nil, err
	}

	files := make([]File, 0, len(paths))
	for _, p := range paths {
		if !filter.match(p) {
			continue
		}
		if hasBinaryExt(p) {
			continue
		}

		abs := filepath.Join(r.root, filepath.FromSlash(p))
		info, err := os.Stat(abs)
		if err != nil {
			// Tracked but absent from the work tree (e.g. deleted, sparse
			// checkout): not something we can analyze, so skip it.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("ingest: stat %q: %w", p, err)
		}
		if info.IsDir() {
			continue
		}

		binary, err := fileLooksBinary(abs)
		if err != nil {
			return nil, fmt.Errorf("ingest: sniff %q: %w", p, err)
		}
		if binary {
			continue
		}

		files = append(files, File{
			Path:     p,
			Language: DetectLanguage(p),
			Size:     info.Size(),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return &Snapshot{Commit: head, Files: files}, nil
}

// lsFiles returns the tracked file paths at HEAD, repo-relative and
// slash-separated. It uses -z so that paths containing newlines or other odd
// characters survive intact (git otherwise quotes such paths).
func (r *Repo) lsFiles(ctx context.Context) ([]string, error) {
	raw, err := runGitRaw(ctx, r.root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("ingest: list tracked files: %w", err)
	}
	return splitNUL(raw), nil
}

// fileLooksBinary reads the leading bytes of a file and applies the null-byte
// heuristic.
func fileLooksBinary(abs string) (bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, readHeadBytes)
	n, err := io.ReadFull(f, buf)
	// Short reads are expected (files smaller than the sniff window, or empty);
	// only a genuine read error is fatal.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return isBinaryContent(buf[:n]), nil
}

// splitNUL splits NUL-delimited git output into non-empty segments.
func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.Split(string(b), "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
