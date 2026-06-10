package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// HeadCommit resolves the current HEAD to a full 40-character commit SHA.
func (r *Repo) HeadCommit(ctx context.Context) (string, error) {
	out, err := runGit(ctx, r.root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("ingest: resolve HEAD: %w", err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", errors.New("ingest: resolve HEAD: empty output")
	}
	return sha, nil
}

// Fingerprints returns a map from repo-relative path to the SHA-256 hex digest
// of that file's current on-disk content, for every file in the given
// snapshot. These hashes are content fingerprints (not git blob OIDs): two
// files with identical bytes share a fingerprint regardless of path or history,
// which is what the store's file_state watermarks compare against to decide
// whether a file actually changed.
//
// Computing over the snapshot (rather than re-listing) guarantees the
// fingerprint set is exactly the in-scope, text file set the rest of the
// pipeline reasons about.
func (r *Repo) Fingerprints(ctx context.Context, snap *Snapshot) (map[string]string, error) {
	if snap == nil {
		return nil, errors.New("ingest: Fingerprints: nil snapshot")
	}
	out := make(map[string]string, len(snap.Files))
	for _, f := range snap.Files {
		// Honor cancellation between files for large repositories.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sum, err := hashFile(filepath.Join(r.root, filepath.FromSlash(f.Path)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Raced with a deletion since the snapshot; drop it rather than
				// fail the whole fingerprint pass.
				continue
			}
			return nil, fmt.Errorf("ingest: fingerprint %q: %w", f.Path, err)
		}
		out[f.Path] = sum
	}
	return out, nil
}

// hashFile streams a file through SHA-256 and returns the lowercase hex digest.
func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the lowercase hex SHA-256 digest of b. It is exported as a
// convenience for callers (and tests) that already hold file content in memory
// and want a fingerprint consistent with Fingerprints.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiffFingerprints compares two fingerprint maps (typically an old watermark
// set and a freshly computed one) and reports the paths that were added,
// modified, or deleted. This is a pure, store-free helper: the funnel can use
// it to derive a changed-set from watermarks without a git diff, complementing
// commit-range diffing in ChangedFiles.
func DiffFingerprints(old, current map[string]string) FingerprintDiff {
	var d FingerprintDiff
	for path, newHash := range current {
		oldHash, ok := old[path]
		switch {
		case !ok:
			d.Added = append(d.Added, path)
		case oldHash != newHash:
			d.Modified = append(d.Modified, path)
		}
	}
	for path := range old {
		if _, ok := current[path]; !ok {
			d.Deleted = append(d.Deleted, path)
		}
	}
	sortStrings(d.Added)
	sortStrings(d.Modified)
	sortStrings(d.Deleted)
	return d
}

// FingerprintDiff is the result of comparing two fingerprint maps.
type FingerprintDiff struct {
	Added    []string
	Modified []string
	Deleted  []string
}
