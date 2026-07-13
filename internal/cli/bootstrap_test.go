package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// newGitRepoDir creates a fresh git repository in a temp directory and
// returns its (already-toplevel) path. Several config.RepoToplevel /
// configPathFromCmd behaviors only make sense inside a real git work tree.
func newGitRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Resolve to the canonical toplevel path (macOS /tmp is a symlink to
	// /private/tmp; git rev-parse --show-toplevel always returns the
	// resolved form, so tests must compare against the same form).
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return string(bytesTrimNewline(out))
}

func bytesTrimNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

// rootCmdWithConfigFlag builds a minimal root cobra command carrying the
// persistent --config flag that configPathFromCmd reads, mirroring how the
// real root command registers it.
func rootCmdWithConfigFlag(t *testing.T, configFlagValue string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "bugbot"}
	root.PersistentFlags().String("config", config.DefaultFileName, "config file")
	if configFlagValue != "" {
		if err := root.PersistentFlags().Set("config", configFlagValue); err != nil {
			t.Fatalf("set --config: %v", err)
		}
	}
	return root
}

// TestConfigPathFromCmd_ExplicitFlagWins verifies that a non-default
// --config value always wins, regardless of what exists on disk.
func TestConfigPathFromCmd_ExplicitFlagWins(t *testing.T) {
	dir := newGitRepoDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)

	// Both a local bugbot.yaml and a stealth config exist; explicit --config
	// still wins.
	writeFile(t, filepath.Join(dir, config.DefaultFileName), "local: true\n")
	stealthPath, err := config.StealthConfigPath(".")
	if err != nil {
		t.Fatalf("StealthConfigPath: %v", err)
	}
	writeFileWithDirs(t, stealthPath, "stealth: true\n")

	root := rootCmdWithConfigFlag(t, "custom.yaml")
	got := configPathFromCmd(root)
	if got != "custom.yaml" {
		t.Errorf("configPathFromCmd = %q, want custom.yaml", got)
	}
}

// TestConfigPathFromCmd_LocalWinsOverStealth verifies that a local
// ./bugbot.yaml takes priority over a stealth config when the --config flag
// is left at its default.
func TestConfigPathFromCmd_LocalWinsOverStealth(t *testing.T) {
	dir := newGitRepoDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)

	writeFile(t, filepath.Join(dir, config.DefaultFileName), "local: true\n")
	stealthPath, err := config.StealthConfigPath(".")
	if err != nil {
		t.Fatalf("StealthConfigPath: %v", err)
	}
	writeFileWithDirs(t, stealthPath, "stealth: true\n")

	root := rootCmdWithConfigFlag(t, "")
	got := configPathFromCmd(root)
	if got != config.DefaultFileName {
		t.Errorf("configPathFromCmd = %q, want %q (local file must win)", got, config.DefaultFileName)
	}
}

// TestConfigPathFromCmd_StealthDiscovered verifies that when no local
// bugbot.yaml exists but a stealth config is present under
// $HOME/.bugbot/<repo-key>/, configPathFromCmd discovers it.
func TestConfigPathFromCmd_StealthDiscovered(t *testing.T) {
	dir := newGitRepoDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)

	stealthPath, err := config.StealthConfigPath(".")
	if err != nil {
		t.Fatalf("StealthConfigPath: %v", err)
	}
	writeFileWithDirs(t, stealthPath, "stealth: true\n")

	root := rootCmdWithConfigFlag(t, "")
	got := configPathFromCmd(root)
	if got != stealthPath {
		t.Errorf("configPathFromCmd = %q, want stealth path %q", got, stealthPath)
	}
}

// TestConfigPathFromCmd_NothingConfigured verifies that with neither a local
// nor a stealth config present, configPathFromCmd falls back to
// config.DefaultFileName, preserving today's "file not found" error message.
func TestConfigPathFromCmd_NothingConfigured(t *testing.T) {
	dir := newGitRepoDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(dir)

	root := rootCmdWithConfigFlag(t, "")
	got := configPathFromCmd(root)
	if got != config.DefaultFileName {
		t.Errorf("configPathFromCmd = %q, want %q", got, config.DefaultFileName)
	}
}

func writeFileWithDirs(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	writeFile(t, path, content)
}
