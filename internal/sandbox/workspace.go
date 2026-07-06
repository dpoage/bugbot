package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// prepareWorkspace copies the repository snapshot at repoDir into a fresh
// temporary directory and applies any WriteFiles on top. It returns the path
// to the new workspace; the caller is responsible for removing it.
//
// The original repoDir is only ever read, never modified.
func prepareWorkspace(repoDir string, writeFiles map[string][]byte) (string, error) {
	info, err := os.Stat(repoDir)
	if err != nil {
		return "", fmt.Errorf("sandbox: stat repo dir %q: %w", repoDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: repo dir %q is not a directory", repoDir)
	}

	ws, err := os.MkdirTemp("", "bugbot-sandbox-")
	if err != nil {
		return "", fmt.Errorf("sandbox: create workspace: %w", err)
	}

	if err := copyWorkspace(repoDir, ws); err != nil {
		_ = os.RemoveAll(ws)
		return "", fmt.Errorf("sandbox: copy repo into workspace: %w", err)
	}

	if err := applyWriteFiles(ws, writeFiles); err != nil {
		_ = os.RemoveAll(ws)
		return "", err
	}

	return ws, nil
}

// prepareWorkspace resolves the pristine-workspace cache for spec.RepoDir (see
// wsCache) and clones the current pristine into a fresh per-run workspace,
// applying writeFiles on top of the clone. hit reports whether an existing
// pristine could be reused (true) or had to be (re)materialized (false).
//
// Non-git repoDirs have no cheap, reliable content-identity signal to key a
// cache on (no HEAD, no status), so they bypass the cache entirely and fall
// back to the original standalone prepareWorkspace — hit is always false on
// that path.
func (s *CLI) prepareWorkspace(repoDir string, writeFiles map[string][]byte) (ws string, hit bool, err error) {
	info, err := os.Stat(repoDir)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: stat repo dir %q: %w", repoDir, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("sandbox: repo dir %q is not a directory", repoDir)
	}

	key, isRepo, err := workspaceCacheKey(repoDir)
	if err != nil {
		return "", false, err
	}
	if !isRepo {
		ws, err := prepareWorkspace(repoDir, writeFiles)
		return ws, false, err
	}

	ws, hit, err = s.wsCache.clone(repoDir, key)
	if err != nil {
		return "", false, err
	}
	if err := applyWriteFiles(ws, writeFiles); err != nil {
		_ = os.RemoveAll(ws)
		return "", false, err
	}
	return ws, hit, nil
}

// wsCache is a pristine-materialization cache owned by a CLI instance. A
// "pristine" is a read-only-by-convention copy of a repo snapshot (produced
// by copyWorkspace, so it already reflects the git-file-list filtering) keyed
// by repo identity + content state. Exec clones the current pristine into a
// fresh writable per-run workspace instead of re-walking and re-copying the
// source repo from scratch every time.
//
// Only the most recently used key is kept — this is a single-entry cache, not
// an LRU — because a CLI's Execs during any given window overwhelmingly
// target the same repo at the same commit (one scan or repro run against one
// target). Zero value is ready to use; the parent directory is created
// lazily on first use so a CLI that never hits the cached path (no git repos,
// or never Exec'd) never touches the filesystem for it.
type wsCache struct {
	mu sync.Mutex
	// dir is this instance's cache parent (one os.MkdirTemp per CLI); empty
	// until the first cache use.
	dir string
	// key/pristine describe the currently valid pristine; both empty when
	// none has been materialized yet.
	key      string
	pristine string
}

// clone ensures a pristine matching key exists for repoDir (materializing it
// on a miss, evicting the previous one first since only one is kept) and
// clones it into a fresh temp workspace. hit reports whether key already
// matched the cached pristine.
//
// The mutex covers check-and-materialize only: once a valid pristine path is
// resolved, the lock is released before the (potentially slow) tree clone, so
// concurrent Execs against an unchanged repo clone in parallel — the pristine
// is never mutated in place, only replaced wholesale on a miss. This means a
// clone racing exactly against a concurrent miss that evicts its source can
// observe a disappearing pristine; in practice a HEAD/working-tree change
// invalidating the cache happens BETWEEN runs, not between two overlapping
// clones of the same batch, so this is an accepted tradeoff rather than a
// protected invariant.
func (c *wsCache) clone(repoDir, key string) (ws string, hit bool, err error) {
	c.mu.Lock()
	if c.dir == "" {
		dir, mkErr := os.MkdirTemp("", "bugbot-wscache-")
		if mkErr != nil {
			c.mu.Unlock()
			return "", false, fmt.Errorf("sandbox: create workspace cache dir: %w", mkErr)
		}
		c.dir = dir
	}
	hit = c.key == key
	if !hit {
		if c.pristine != "" {
			_ = os.RemoveAll(c.pristine)
		}
		pristine := filepath.Join(c.dir, key)
		if mkErr := os.Mkdir(pristine, 0o700); mkErr != nil {
			c.key, c.pristine = "", ""
			c.mu.Unlock()
			return "", false, fmt.Errorf("sandbox: create pristine workspace dir: %w", mkErr)
		}
		if cpErr := copyWorkspace(repoDir, pristine); cpErr != nil {
			_ = os.RemoveAll(pristine)
			c.key, c.pristine = "", ""
			c.mu.Unlock()
			return "", false, fmt.Errorf("sandbox: materialize pristine workspace: %w", cpErr)
		}
		c.key, c.pristine = key, pristine
	}
	pristine := c.pristine
	c.mu.Unlock()

	ws, err = os.MkdirTemp("", "bugbot-sandbox-")
	if err != nil {
		return "", false, fmt.Errorf("sandbox: create workspace: %w", err)
	}
	if err := cloneTree(pristine, ws); err != nil {
		_ = os.RemoveAll(ws)
		return "", false, fmt.Errorf("sandbox: clone pristine workspace: %w", err)
	}
	return ws, hit, nil
}

// close removes this cache's parent directory, if one was ever materialized,
// and resets state so a subsequent clone starts clean. Safe to call multiple
// times.
func (c *wsCache) close() error {
	c.mu.Lock()
	dir := c.dir
	c.dir, c.key, c.pristine = "", "", ""
	c.mu.Unlock()
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// isGitWorkTree reports whether src looks like a usable git work tree: it has
// a .git entry and git is on PATH. Shared by gitWorktreeFiles (which needs
// the tracked/untracked file list) and workspaceCacheKey (which only needs
// git's rev-parse/status output) so both agree on when to fall back to a full
// copy / bypass the cache.
func isGitWorkTree(src string) bool {
	if _, statErr := os.Stat(filepath.Join(src, ".git")); statErr != nil {
		return false
	}
	_, lookErr := exec.LookPath("git")
	return lookErr == nil
}

// workspaceCacheKey derives the pristine-cache key for repoDir: a hash of the
// repo's absolute path, its HEAD commit, and a hash of `git status
// --porcelain` (which captures dirty/untracked drift a bare commit hash
// misses). isRepo is false (with a nil error) only when repoDir is not a git
// work tree or git is unavailable — the caller then bypasses the cache
// entirely, matching gitWorktreeFiles' fallback contract. When repoDir IS a
// git work tree but a git command fails, the error is returned rather than
// silently treated as a cache miss.
func workspaceCacheKey(repoDir string) (key string, isRepo bool, err error) {
	if !isGitWorkTree(repoDir) {
		return "", false, nil
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", true, fmt.Errorf("sandbox: resolve repo dir %q: %w", repoDir, err)
	}
	head, headErr := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if headErr != nil {
		return "", true, fmt.Errorf("sandbox: git rev-parse HEAD in %q: %w%s", repoDir, headErr, gitStderr(headErr))
	}
	status, statusErr := exec.Command("git", "-C", repoDir, "status", "--porcelain").Output()
	if statusErr != nil {
		return "", true, fmt.Errorf("sandbox: git status --porcelain in %q: %w%s", repoDir, statusErr, gitStderr(statusErr))
	}
	statusSum := sha256.Sum256(status)
	h := sha256.New()
	h.Write([]byte(abs))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(string(head))))
	h.Write([]byte{0})
	h.Write(statusSum[:])
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// copyTree recursively copies the directory tree rooted at src into dst, which
// must already exist. Regular files and directories are copied with their
// permission bits; symlinks are recreated as symlinks (their targets are not
// followed, which avoids copying outside the tree and preserves repo layout).
func copyTree(src, dst string) error {
	return copyTreeWith(src, dst, copyFile)
}

// cloneTree is copyTree's fast path for cloning a pristine cache entry
// (internal/sandbox's wsCache) into a fresh per-run workspace: it copies
// regular files via reflinkOrCopy, which prefers a copy-on-write reflink over
// a full byte copy where the filesystem supports it.
func cloneTree(src, dst string) error {
	return copyTreeWith(src, dst, reflinkOrCopy)
}

// copyTreeWith is copyTree parameterized over the regular-file copy strategy,
// shared by copyTree (always a full byte copy) and cloneTree (reflink-first).
// Directory/symlink handling — the parts that must always be exact, never
// content-copied — stays identical between the two callers.
func copyTreeWith(src, dst string, copyRegular func(src, dst string, perm fs.FileMode) error) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			// Never copy VCS metadata: it is large, unneeded for any build, and
			// handing full history to untrusted sandbox commands is a footgun.
			// (The git-files copy path already omits it; this guards the
			// full-copy fallback.) A submodule/worktree .git is a FILE, not a
			// dir; SkipDir on a non-dir would skip the rest of the PARENT dir, so
			// only a directory uses SkipDir — a gitfile is skipped on its own.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if rel == "." {
				// dst already exists; align its mode with the source root.
				return os.Chmod(dst, info.Mode().Perm())
			}
			return os.MkdirAll(target, info.Mode().Perm())

		case d.Type()&fs.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)

		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyRegular(path, target, info.Mode().Perm())

		default:
			// Skip irregular files (devices, sockets, named pipes): they have no
			// place in a code snapshot and could be a vector for surprises.
			return nil
		}
	})
}

// copyWorkspace materializes the repo snapshot at src into dst. When src is a
// git work tree and git is on PATH, it copies only what git considers part of
// the work tree — tracked files plus untracked files that are NOT gitignored
// (`git ls-files --cached --others --exclude-standard`). This omits generated,
// gitignored content: most importantly a stale out-of-source build tree (e.g.
// CMake's build/ whose CMakeCache.txt is pinned to a host-absolute path, which
// otherwise poisons an in-sandbox rebuild), the .git directory, and built
// vendor caches. When src is not a git repo (or git is unavailable) it falls
// back to a full recursive copy so non-git checkouts still work.
func copyWorkspace(src, dst string) error {
	files, isRepo, err := gitWorktreeFiles(src)
	if err != nil {
		// src IS a git work tree but listing failed. Falling back to a full copy
		// here would silently reintroduce the gitignored stale build tree this
		// path exists to exclude (the RC2 poisoning), so surface the error
		// rather than degrade to a poisoning copy.
		return err
	}
	if isRepo {
		return copyFileList(src, dst, files)
	}
	return copyTree(src, dst)
}

// gitWorktreeFiles returns the repo-relative paths git considers part of the
// work tree at src: tracked files plus untracked files that are not gitignored.
// isRepo is false (with a nil error) only when src is not a git work tree or
// git is not on PATH — the caller then falls back to a full copy. When src IS a
// git work tree but `git ls-files` fails, the error is returned so the caller
// fails loudly rather than silently full-copying gitignored build artifacts.
// Returned paths use the OS path separator.
func gitWorktreeFiles(src string) (files []string, isRepo bool, err error) {
	if _, statErr := os.Stat(filepath.Join(src, ".git")); statErr != nil {
		return nil, false, nil // not a git work tree: caller does a full copy
	}
	if _, lookErr := exec.LookPath("git"); lookErr != nil {
		return nil, false, nil // git unavailable: caller does a full copy
	}
	// -z NUL-separates entries so paths with spaces/newlines survive intact.
	out, runErr := exec.Command("git", "-C", src, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard").Output()
	if runErr != nil {
		return nil, true, fmt.Errorf("sandbox: git ls-files in %q: %w%s", src, runErr, gitStderr(runErr))
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		files = append(files, filepath.FromSlash(p))
	}
	return files, true, nil
}

// gitStderr extracts captured stderr from an *exec.ExitError for diagnostics.
func gitStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// copyFileList copies the given repo-relative entries from src into dst,
// creating parent directories as needed. Regular files preserve their
// permission bits; symlinks are recreated (targets not followed). A directory
// entry is a git submodule gitlink (ls-files emits the submodule path, which
// resolves on disk to its checked-out working tree) and is recursively copied
// so vendored-as-submodule dependencies reach the sandbox, matching the old
// full-copy behavior. A listed path missing on disk (e.g. a tracked-but-deleted
// file) is skipped rather than aborting the copy.
func copyFileList(src, dst string, files []string) error {
	for _, rel := range files {
		srcPath := filepath.Join(src, rel)
		info, err := os.Lstat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		target := filepath.Join(dst, rel)
		// Parent dirs are created 0o755 (traversable/writable for the build)
		// rather than mirroring source dir perms: the workspace is a throwaway
		// per-run copy, so normalizing to a permissive-but-safe mode is fine.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			linkTarget, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
		case info.IsDir():
			// Submodule gitlink: copy its working tree (copyTree skips the
			// submodule's own .git). MkdirAll the target first so copyTree's
			// root-dir chmod has a directory to act on.
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := copyTree(srcPath, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(srcPath, target, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			// Skip irregular files (devices, sockets, fifos).
		}
	}
	return nil
}

func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// Close the destination on the way out. A close failure on a written file can
	// surface a deferred write error, so propagate it unless we already have one.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// applyWriteFiles writes the supplied files into the workspace. Each key is a
// path relative to the workspace root. Paths that escape the workspace are
// rejected.
func applyWriteFiles(ws string, files map[string][]byte) error {
	for name, content := range files {
		rel, err := sanitizeRelPath(name)
		if err != nil {
			return fmt.Errorf("sandbox: write file %q: %w", name, err)
		}
		dst := filepath.Join(ws, rel)

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("sandbox: write file %q: mkdir: %w", name, err)
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return fmt.Errorf("sandbox: write file %q: %w", name, err)
		}
	}
	return nil
}

// errPathEscape is returned when a WriteFiles key would resolve outside the
// workspace root.
var errPathEscape = errors.New("path escapes workspace")

// sanitizeRelPath validates that name is a relative path that stays within the
// workspace, returning the cleaned path. Absolute paths and any path that
// traverses above the root via ".." are rejected.
func sanitizeRelPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(name) {
		return "", errPathEscape
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errPathEscape
	}
	if clean == "." {
		return "", errors.New("path resolves to workspace root")
	}
	return clean, nil
}

// ValidateWorkspacePath reports whether name is a legal WriteFiles key: a
// non-empty, workspace-relative path that does not resolve to the root and does
// not escape the workspace via an absolute prefix or "..". It is the exported
// guard callers (e.g. the reproducer's plan validator) use to reject an
// escaping injection path BEFORE a sandbox run, so the failure is a recoverable
// "invalid plan" rather than a hard mid-run write error. It shares
// sanitizeRelPath's rule verbatim, so the pre-flight check and the actual
// applyWriteFiles write can never disagree.
func ValidateWorkspacePath(name string) error {
	_, err := sanitizeRelPath(name)
	return err
}

// sanitizeCapturePaths validates and cleans each Spec.CaptureFiles entry via
// sanitizeRelPath — the same workspace-escape guard WriteFiles keys obey — so
// a capture request can never read outside the workspace it wrote into.
// Returns the cleaned relative paths in the same order; a single invalid
// entry fails the whole Exec call up front (before spending a sandbox run),
// mirroring applyWriteFiles' all-or-nothing validation.
func sanitizeCapturePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := sanitizeRelPath(p)
		if err != nil {
			return nil, fmt.Errorf("sandbox: capture file %q: %w", p, err)
		}
		out = append(out, rel)
	}
	return out, nil
}

// captureWorkspaceFiles reads the given (already-validated, workspace-
// relative) paths from ws, capping each at maxBytes like the stdout/stderr
// caps. It must run after the command finishes but before the workspace is
// removed (the caller's deferred cleanup). A path that does not exist — the
// command never produced it, e.g. a test runner crashed before writing its
// junitxml — is silently omitted: CaptureFiles is a best-effort "grab it if
// it's there" contract, not a manifest every run must satisfy. Any other read
// error (permission, path is a directory) is likewise treated as absence
// rather than failing an otherwise-successful run over a capture side
// channel.
func captureWorkspaceFiles(ws string, paths []string, maxBytes int) map[string][]byte {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(paths))
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(ws, rel))
		if err != nil {
			continue
		}
		if maxBytes > 0 && len(data) > maxBytes {
			data = data[:maxBytes]
		}
		out[rel] = data
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
