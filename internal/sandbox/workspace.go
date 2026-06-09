package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

	if err := copyTree(repoDir, ws); err != nil {
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

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
