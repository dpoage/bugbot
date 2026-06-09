package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// FixtureSpec describes a fixture repository the harness materializes into a
// scratch directory and initializes as a real git repo with a single seed
// commit. The funnel runs against real on-disk repos (it ingests via git and
// reads files with its tools), so fixtures must be real repos, not in-memory
// stubs.
type FixtureSpec struct {
	// Files maps repo-relative paths to file contents. Parent directories are
	// created as needed. At least one file is required.
	Files map[string]string
	// Base, when non-empty, is an on-disk directory whose contents are copied in
	// before Files are written (Files override on path collision). It lets a case
	// build on a real example tree without inlining every file. Optional.
	Base string
}

// materialize writes the fixture into a fresh temp dir, copies any Base tree,
// writes Files, and commits everything as one "seed" commit via git. It returns
// the absolute path to the repo root. The caller must cleanup the dir.
//
// git is invoked with explicit -c user.* config (not relying on global git
// config) so the harness works in clean CI environments, mirroring the pattern
// in internal/ingest and internal/funnel tests.
func materialize(spec FixtureSpec) (string, error) {
	if len(spec.Files) == 0 {
		return "", fmt.Errorf("fixture has no files")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not available: %w", err)
	}

	dir, err := os.MkdirTemp("", "bugbot-eval-*")
	if err != nil {
		return "", fmt.Errorf("create scratch dir: %w", err)
	}

	if spec.Base != "" {
		if err := copyTree(spec.Base, dir); err != nil {
			cleanup(dir)
			return "", fmt.Errorf("copy base %q: %w", spec.Base, err)
		}
	}

	// Write Files in sorted order for deterministic behavior.
	paths := make([]string, 0, len(spec.Files))
	for p := range spec.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if err := writeFixtureFile(dir, rel, spec.Files[rel]); err != nil {
			cleanup(dir)
			return "", err
		}
	}

	if err := gitInitCommit(dir); err != nil {
		cleanup(dir)
		return "", err
	}
	return dir, nil
}

// writeFixtureFile writes one repo-relative file, guarding against path
// traversal outside the repo root.
func writeFixtureFile(root, rel, content string) error {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("fixture path %q escapes repo root", rel)
	}
	p := filepath.Join(root, clean)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir for %q: %w", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", rel, err)
	}
	return nil
}

// gitInitCommit runs the init/add/commit sequence with hermetic user config.
func gitInitCommit(dir string) error {
	steps := [][]string{
		{"init", "-q"},
		{"-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "add", "."},
		{"-c", "user.name=bugbot-eval", "-c", "user.email=eval@bugbot.test", "commit", "-q", "-m", "seed"},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Belt-and-suspenders: also set the env author/committer so commits never
		// fall back to (possibly absent) global identity.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=bugbot-eval", "GIT_AUTHOR_EMAIL=eval@bugbot.test",
			"GIT_COMMITTER_NAME=bugbot-eval", "GIT_COMMITTER_EMAIL=eval@bugbot.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// copyTree copies the regular files under src into dst, preserving relative
// layout. It skips any .git directory in the source so a base example repo's
// history never leaks into the fixture.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		// Never copy the base's git metadata.
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// fixtureDBPath returns a file-backed (not :memory:) store path inside dir. A
// file-backed DB is required: an in-memory SQLite DB is private per connection,
// so the migrated schema would be invisible to database/sql's pool connections
// (this is the same reasoning documented in the funnel tests).
func fixtureDBPath(dir string) string {
	return filepath.Join(dir, ".bugbot-eval-state.db")
}

// cleanup removes a materialized fixture dir, ignoring errors (best-effort).
func cleanup(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}
