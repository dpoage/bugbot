package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// copyTree recursively copies the directory tree rooted at src into dst, which
// must already exist. Regular files and directories are copied with their
// permission bits; symlinks are recreated as symlinks (their targets are not
// followed, which avoids copying outside the tree and preserves repo layout).
func copyTree(src, dst string) error {
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
			return copyFile(path, target, info.Mode().Perm())

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
