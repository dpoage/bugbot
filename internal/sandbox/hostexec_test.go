package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newHostExecRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHostExec_RunsAgainstWorkspaceCopyNotLiveCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test assumes POSIX /bin/sh")
	}
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	// Mutate the workspace copy; the live checkout must be untouched.
	res, err := h.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Cmd:     []string{"/bin/sh", "-c", "echo mutated > marker.txt; cat marker.txt"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0: stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "mutated") {
		t.Errorf("Stdout = %q, want it to contain the workspace-copy write", res.Stdout)
	}

	live, err := os.ReadFile(filepath.Join(repoDir, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != "original\n" {
		t.Errorf("live checkout was mutated: %q, want unchanged %q", live, "original\n")
	}
}

func TestHostExec_NonZeroExitIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test assumes POSIX /bin/sh")
	}
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	res, err := h.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Cmd:     []string{"/bin/sh", "-c", "exit 3"},
	})
	if err != nil {
		t.Fatalf("Exec should not error on a non-zero exit, got %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestHostExec_LaunchFailureIsAnError(t *testing.T) {
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	_, err := h.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Cmd:     []string{"definitely-not-a-real-binary-xyz"},
	})
	if err == nil {
		t.Fatal("want an infrastructure error for a binary that cannot be launched")
	}
}

func TestHostExec_EnvPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test assumes POSIX /bin/sh")
	}
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	res, err := h.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Cmd:     []string{"/bin/sh", "-c", "echo $BUGBOT_TEST_VAR"},
		Env:     []string{"BUGBOT_TEST_VAR=hello-hostexec"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello-hostexec") {
		t.Errorf("Stdout = %q, want it to contain the injected env value", res.Stdout)
	}
}

func TestHostExec_TimeoutReportsTimedOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test assumes POSIX /bin/sh")
	}
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	res, err := h.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Cmd:     []string{"/bin/sh", "-c", "sleep 5"},
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Error("want TimedOut=true")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
}

func TestHostExec_EmptyCmdIsAnError(t *testing.T) {
	h := NewHostExec()
	if _, err := h.Exec(context.Background(), Spec{RepoDir: t.TempDir()}); err == nil {
		t.Fatal("want an error for an empty Cmd")
	}
}

func TestHostExec_WriteFilesInjected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test assumes POSIX /bin/sh")
	}
	repoDir := newHostExecRepoDir(t)
	h := NewHostExec()

	res, err := h.Exec(context.Background(), Spec{
		RepoDir:    repoDir,
		Cmd:        []string{"/bin/sh", "-c", "cat injected.txt"},
		WriteFiles: map[string][]byte{"injected.txt": []byte("from write files\n")},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "from write files") {
		t.Errorf("Stdout = %q, want the injected file's content", res.Stdout)
	}
	// The live checkout must never see the injected file either.
	if _, err := os.Stat(filepath.Join(repoDir, "injected.txt")); !os.IsNotExist(err) {
		t.Error("WriteFiles must never touch the live checkout")
	}
}
