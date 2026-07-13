package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoKey returns a stable, filesystem-safe identifier for a repo toplevel
// path: "<basename>-<hex(sha256(absToplevel))[:12]>". The input is resolved
// to an absolute, cleaned path before hashing so that equivalent paths
// (e.g. with a trailing slash, or reached via a different relative prefix)
// key identically.
func RepoKey(toplevel string) string {
	abs, err := filepath.Abs(toplevel)
	if err != nil {
		abs = toplevel
	}
	abs = filepath.Clean(abs)

	sum := sha256.Sum256([]byte(abs))
	hash := hex.EncodeToString(sum[:])[:12]

	base := filepath.Base(abs)
	return base + "-" + hash
}

// StealthStateDir returns the per-repo state directory used by stealth mode:
// $HOME/.bugbot/<RepoKey(toplevel)>.
func StealthStateDir(toplevel string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bugbot", RepoKey(toplevel)), nil
}

// RepoToplevel resolves dir's enclosing git work tree via
// `git -C dir rev-parse --show-toplevel`. When dir is not inside a git work
// tree (or git is unavailable), it falls back to filepath.Abs(dir); if that
// also fails, dir is returned unchanged.
func RepoToplevel(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err == nil {
		if top := strings.TrimSuffix(string(out), "\n"); top != "" {
			return top
		}
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// StealthConfigPath returns the stealth-mode config file path for the repo
// enclosing dir: <StealthStateDir(RepoToplevel(dir))>/bugbot.yaml.
func StealthConfigPath(dir string) (string, error) {
	stateDir, err := StealthStateDir(RepoToplevel(dir))
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "bugbot.yaml"), nil
}

// WriteRepoMarker ensures stateDir exists (0700) and writes a "repo" file
// inside it containing toplevel followed by a newline, so a stealth state
// directory can always be traced back to the repo it belongs to. Idempotent.
func WriteRepoMarker(stateDir, toplevel string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, "repo"), []byte(toplevel+"\n"), 0o644)
}
