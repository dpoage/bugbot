package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

var hexRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestRepoKey(t *testing.T) {
	a := RepoKey("/home/user/repo")
	b := RepoKey("/home/user/repo")
	if a != b {
		t.Fatalf("RepoKey not stable: %q vs %q", a, b)
	}

	base, hash, ok := splitRepoKey(a)
	if !ok {
		t.Fatalf("RepoKey %q does not match <base>-<12 hex> format", a)
	}
	if base != "repo" {
		t.Errorf("base = %q, want repo", base)
	}
	if !hexRE.MatchString(hash) {
		t.Errorf("hash %q is not 12 lowercase hex chars", hash)
	}

	other := RepoKey("/home/user/other-repo")
	if other == a {
		t.Errorf("RepoKey(%q) == RepoKey(%q), want different keys", "/home/user/repo", "/home/user/other-repo")
	}

	// Equivalent (uncleaned) path must key identically.
	uncleaned := RepoKey("/home/user//repo/./")
	if uncleaned != a {
		t.Errorf("RepoKey of uncleaned-equivalent path = %q, want %q", uncleaned, a)
	}
}

// splitRepoKey splits a "<base>-<hash>" RepoKey result into its parts,
// tolerating a base name that itself contains hyphens.
func splitRepoKey(key string) (base, hash string, ok bool) {
	i := len(key) - 12
	if i < 1 || key[i-1] != '-' {
		return "", "", false
	}
	return key[:i-1], key[i:], true
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Resolve symlinks (macOS /tmp, some CI) so comparisons against
	// `git rev-parse --show-toplevel` (which prints the resolved path) hold.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = resolved
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return resolved
}

func TestRepoToplevel(t *testing.T) {
	t.Run("inside git repo, from subdirectory", func(t *testing.T) {
		toplevel := initGitRepo(t)
		sub := filepath.Join(toplevel, "a", "b")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		got := RepoToplevel(sub)
		if got != toplevel {
			t.Errorf("RepoToplevel(%q) = %q, want %q", sub, got, toplevel)
		}
	})

	t.Run("outside git repo falls back to abs", func(t *testing.T) {
		dir := t.TempDir()
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		got := RepoToplevel(resolved)
		want, err := filepath.Abs(resolved)
		if err != nil {
			t.Fatalf("Abs: %v", err)
		}
		if got != want {
			t.Errorf("RepoToplevel(%q) = %q, want %q", resolved, got, want)
		}
	})
}

func TestWriteRepoMarker(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	toplevel := "/some/repo/toplevel"

	if err := WriteRepoMarker(stateDir, toplevel); err != nil {
		t.Fatalf("WriteRepoMarker: %v", err)
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", stateDir)
	}

	markerPath := filepath.Join(stateDir, "repo")
	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != toplevel+"\n" {
		t.Errorf("marker content = %q, want %q", got, toplevel+"\n")
	}

	// Idempotent: calling again must not fail and must leave content intact.
	if err := WriteRepoMarker(stateDir, toplevel); err != nil {
		t.Fatalf("WriteRepoMarker (second call): %v", err)
	}
	got2, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker after second call: %v", err)
	}
	if string(got2) != toplevel+"\n" {
		t.Errorf("marker content after second call = %q, want %q", got2, toplevel+"\n")
	}
}

func TestStealthConfigPath(t *testing.T) {
	home := setFakeHome(t)
	toplevel := initGitRepo(t)

	got, err := StealthConfigPath(toplevel)
	if err != nil {
		t.Fatalf("StealthConfigPath: %v", err)
	}
	want := filepath.Join(home, ".bugbot", RepoKey(toplevel), "bugbot.yaml")
	if got != want {
		t.Errorf("StealthConfigPath(%q) = %q, want %q", toplevel, got, want)
	}

	// A subdirectory of the repo resolves to the same path — the key is
	// derived from the repo toplevel, not the passed-in dir.
	sub := filepath.Join(toplevel, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	gotSub, err := StealthConfigPath(sub)
	if err != nil {
		t.Fatalf("StealthConfigPath(sub): %v", err)
	}
	if gotSub != want {
		t.Errorf("StealthConfigPath(%q) = %q, want %q (same as repo toplevel)", sub, gotSub, want)
	}
}

// setFakeHome points $HOME at a fresh temp dir so stealth-mode tests never
// touch the developer's real home directory, and returns it.
func setFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestLoadStealth(t *testing.T) {
	t.Run("stealth true re-roots all three state paths under HOME", func(t *testing.T) {
		home := setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)

		path := writeTemp(t, validYAML+"\nstealth: true\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		wantBase := filepath.Join(home, ".bugbot", RepoKey(toplevel))
		if got := filepath.Dir(cfg.Storage.Path); got != wantBase {
			t.Errorf("storage dir = %q, want %q", got, wantBase)
		}
		if cfg.Report.Dir != filepath.Join(wantBase, "reports") {
			t.Errorf("report.dir = %q, want under %q", cfg.Report.Dir, wantBase)
		}
		if cfg.TranscriptDir != filepath.Join(wantBase, "transcripts") {
			t.Errorf("transcript_dir = %q, want under %q", cfg.TranscriptDir, wantBase)
		}

		// ControlSocketPath derives from Storage.Path and must land in the
		// same stealth directory too.
		if got := filepath.Dir(cfg.ControlSocketPath()); got != wantBase {
			t.Errorf("ControlSocketPath dir = %q, want %q", got, wantBase)
		}
	})

	t.Run("explicit storage.path wins over seeded stealth default", func(t *testing.T) {
		setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)

		explicit := filepath.Join(t.TempDir(), "custom.db")
		path := writeTemp(t, validYAML+"\nstealth: true\nstorage:\n  path: "+explicit+"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Storage.Path != explicit {
			t.Errorf("storage.path = %q, want explicit %q", cfg.Storage.Path, explicit)
		}
	})

	t.Run("BUGBOT_STORAGE_PATH env wins over seeded stealth default", func(t *testing.T) {
		setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)

		explicit := filepath.Join(t.TempDir(), "env.db")
		t.Setenv("BUGBOT_STORAGE_PATH", explicit)
		path := writeTemp(t, validYAML+"\nstealth: true\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Storage.Path != explicit {
			t.Errorf("storage.path = %q, want env override %q", cfg.Storage.Path, explicit)
		}
	})

	t.Run("explicit empty transcript_dir under stealth still disables autosave", func(t *testing.T) {
		setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)

		path := writeTemp(t, validYAML+"\nstealth: true\ntranscript_dir: \"\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.TranscriptDir != "" {
			t.Errorf("transcript_dir = %q, want empty (autosave disabled)", cfg.TranscriptDir)
		}
	})

	t.Run("BUGBOT_STEALTH=true with no yaml key activates stealth", func(t *testing.T) {
		home := setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)
		t.Setenv("BUGBOT_STEALTH", "true")

		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		wantBase := filepath.Join(home, ".bugbot", RepoKey(toplevel))
		if got := filepath.Dir(cfg.Storage.Path); got != wantBase {
			t.Errorf("storage dir = %q, want %q", got, wantBase)
		}
	})

	t.Run("BUGBOT_STEALTH=false overrides yaml stealth:true", func(t *testing.T) {
		setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)
		t.Setenv("BUGBOT_STEALTH", "false")

		path := writeTemp(t, validYAML+"\nstealth: true\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Storage.Path != ".bugbot/state.db" {
			t.Errorf("storage.path = %q, want default .bugbot/state.db (stealth disabled by env)", cfg.Storage.Path)
		}
		if cfg.Stealth {
			t.Error("cfg.Stealth = true, want false (BUGBOT_STEALTH=false must override yaml stealth: true)")
		}
	})

	t.Run("BUGBOT_REPORT_DIR wins over seeded stealth default", func(t *testing.T) {
		setFakeHome(t)
		toplevel := initGitRepo(t)
		t.Chdir(toplevel)

		explicit := filepath.Join(t.TempDir(), "custom-reports")
		t.Setenv("BUGBOT_REPORT_DIR", explicit)
		path := writeTemp(t, validYAML+"\nstealth: true\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Report.Dir != explicit {
			t.Errorf("report.dir = %q, want env override %q", cfg.Report.Dir, explicit)
		}
	})

	t.Run("non-stealth Load defaults unchanged", func(t *testing.T) {
		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Storage.Path != ".bugbot/state.db" {
			t.Errorf("storage.path = %q, want .bugbot/state.db", cfg.Storage.Path)
		}
		if cfg.Report.Dir != ".bugbot/reports" {
			t.Errorf("report.dir = %q, want .bugbot/reports", cfg.Report.Dir)
		}
		if cfg.TranscriptDir != ".bugbot/transcripts" {
			t.Errorf("transcript_dir = %q, want .bugbot/transcripts", cfg.TranscriptDir)
		}
		if cfg.Stealth {
			t.Errorf("Stealth = true, want false")
		}
	})
}
