//go:build integration

// Integration tests exercise the real container runtime. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// They are skipped automatically when no runtime is detected or the test image
// cannot be pulled. Kept under ~60s total.
package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImage = "docker.io/library/alpine:latest"

// newTestCLI builds a CLI against the detected runtime, skipping the test when
// none is available or the image cannot be pulled/used.
func newTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping integration test")
	}
	s, err := NewCLI(rt, testImage,
		WithCPUs(1),
		WithMemoryMB(256),
		WithPidsLimit(128),
		WithTimeout(30*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI: %v", err)
	}
	ensureImage(t, s)
	return s
}

// ensureImage runs a trivial container to force an image pull up front; if it
// fails (e.g. no network to pull), the suite skips rather than failing.
func ensureImage(t *testing.T, s *CLI) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}})
	if err != nil {
		t.Skipf("cannot run test image %q (pull failed?): %v", testImage, err)
	}
}

func TestIntegrationEchoSucceeds(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"echo", "hi"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hi")
	}
	if res.TimedOut {
		t.Error("unexpected TimedOut")
	}
}

func TestIntegrationNonZeroExit(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"sh", "-c", "exit 3"},
	})
	if err != nil {
		t.Fatalf("Exec returned error for non-zero exit (should be reported via ExitCode): %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestIntegrationWriteFilesInjected(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:    t.TempDir(),
		Cmd:        []string{"cat", "repro/marker.txt"},
		WriteFiles: map[string][]byte{"repro/marker.txt": []byte("INJECTED")},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "INJECTED" {
		t.Errorf("Stdout = %q, want INJECTED", res.Stdout)
	}
}

// TestIntegrationCaptureFilesReadBack — bugbot-ym09 — the structured-output
// capture seam: a file the containerized command writes into the workspace is
// read back into Result.Captured before the workspace is torn down. Also
// covers the missing-file case in the same real-podman run: CaptureFiles asks
// for a second path the command never writes, and it must be silently absent
// rather than causing an error.
func TestIntegrationCaptureFilesReadBack(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:      t.TempDir(),
		Cmd:          []string{"sh", "-c", "echo '<testsuites></testsuites>' > report.xml"},
		CaptureFiles: []string{"report.xml", "never-written.xml"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	got, present := res.Captured["report.xml"]
	if !present {
		t.Fatal("Captured[report.xml] absent, want the file the container wrote")
	}
	if strings.TrimSpace(string(got)) != "<testsuites></testsuites>" {
		t.Errorf("Captured[report.xml] = %q", got)
	}
	if _, present := res.Captured["never-written.xml"]; present {
		t.Error("Captured[never-written.xml] present, want silently absent")
	}
}

// TestIntegrationCaptureFilesSymlinkEscapeRejected — bugbot-ym09 review
// finding — proves the fix against a REAL container, not just the host-side
// unit tests: the sandboxed command plants a symlink at the capture path
// whose target is an absolute HOST path outside the workspace (created with
// full write access to its own workspace, which is all `ln -s` needs — the
// target need not even resolve inside the container's own mount namespace).
// After the container exits, readCaptureFile runs on the HOST and must
// refuse to follow that symlink: without the fix this would let a
// container-controlled repro plan read arbitrary host files back through
// Result.Captured.
func TestIntegrationCaptureFilesSymlinkEscapeRejected(t *testing.T) {
	s := newTestCLI(t)

	hostSecretDir := t.TempDir()
	hostSecret := filepath.Join(hostSecretDir, "secret.txt")
	if err := os.WriteFile(hostSecret, []byte("host-only-secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:      t.TempDir(),
		Cmd:          []string{"sh", "-c", "ln -s " + hostSecret + " report.xml"},
		CaptureFiles: []string{"report.xml"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if got, present := res.Captured["report.xml"]; present {
		t.Fatalf("a symlink escaping the workspace to a host path must be refused, got %q (host secret leaked)", got)
	}
}

// TestIntegrationApplyWriteFilesSymlinkEscapeRejected — bugbot-6nqd — proves
// the WRITE-side symlink hardening against a REAL container and a REAL
// workspace REUSE across two Execs, exactly the reproducer's iteration shape
// (bugbot-bkz1):
// call 1's containerized command plants a symlink pointing at an absolute
// HOST path outside the workspace; call 2 asks WriteFiles to write to that
// same relative path. Without the fix, applyWriteFiles (running on the HOST,
// after call 1's container has already exited) would follow the symlink and
// overwrite the host file as the bugbot user — a full sandbox escape, strictly
// worse than the read-side bugbot-ym09 finding above since it is a HOST WRITE.
func TestIntegrationApplyWriteFilesSymlinkEscapeRejected(t *testing.T) {
	s := newTestCLI(t)

	repoDir := t.TempDir()
	ws, err := s.MaterializeWorkspace(repoDir)
	if err != nil {
		t.Fatalf("MaterializeWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws) }()

	hostDir := t.TempDir()
	hostTarget := filepath.Join(hostDir, "authorized_keys")
	if err := os.WriteFile(hostTarget, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Call 1 (iteration run #1): the container plants a symlink named
	// "leak" pointing at an absolute host path outside the workspace — all
	// `ln -s` needs is write access to its own workspace.
	res1, err := s.Exec(context.Background(), Spec{
		RepoDir:   repoDir,
		Workspace: ws,
		Cmd:       []string{"sh", "-c", "ln -s " + hostTarget + " leak"},
	})
	if err != nil {
		t.Fatalf("Exec (plant symlink): %v", err)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("plant symlink ExitCode = %d, stderr=%q", res1.ExitCode, res1.Stderr)
	}

	// Call 2 (iteration run #2, SAME workspace): a WriteFiles entry
	// targets the exact relative path the container just planted a symlink at.
	_, err = s.Exec(context.Background(), Spec{
		RepoDir:    repoDir,
		Workspace:  ws,
		Cmd:        []string{"true"},
		WriteFiles: map[string][]byte{"leak": []byte("pwned\n")},
	})
	if err == nil {
		t.Fatal("Exec should have refused to write through the planted symlink")
	}

	got, readErr := os.ReadFile(hostTarget)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "original\n" {
		t.Errorf("host file must not have been overwritten through the symlink: got %q (sandbox escape)", got)
	}
}

func TestIntegrationTimeoutReapsContainer(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"sleep", "30"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
	if res.Duration > 10*time.Second {
		t.Errorf("Duration = %v, expected to be killed near the 2s timeout", res.Duration)
	}
}

func TestIntegrationNetworkBlocked(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// wget should fail with --network=none (the default).
		Cmd:     []string{"wget", "-T", "3", "-q", "-O-", "http://example.com"},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("network egress should be blocked but wget succeeded; stdout=%q", res.Stdout)
	}
	if res.TimedOut {
		t.Error("network test timed out unexpectedly; expected fast failure under --network=none")
	}
}

func TestIntegrationOriginalRepoReadOnly(t *testing.T) {
	s := newTestCLI(t)
	repo := t.TempDir()
	mustWrite(t, repo+"/original.txt", "orig", 0o644)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		// Mutate the workspace copy; the host original must be untouched.
		Cmd: []string{"sh", "-c", "echo changed > /workspace/original.txt && echo new > /workspace/created.txt"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	assertFileContent(t, repo+"/original.txt", "orig")
	if _, statErr := os.Stat(repo + "/created.txt"); statErr == nil {
		t.Error("host repo was mutated: created.txt should not exist on host")
	}
}

// TestIntegrationIdleWatchdogKillsStall: a run that produces no output and no
// workspace writes is cancelled after the idle window, well before its generous
// absolute ceiling.
func TestIntegrationIdleWatchdogKillsStall(t *testing.T) {
	s := newTestCLI(t)
	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:     t.TempDir(),
		Cmd:         []string{"sleep", "60"},
		Timeout:     50 * time.Second, // generous hard ceiling
		IdleTimeout: 2 * time.Second,  // no progress -> idle kill
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true on idle stall, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on idle kill", res.ExitCode)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("idle watchdog took %v; expected a kill within a few idle windows, far under the 50s ceiling", d)
	}
}

// TestIntegrationIdleWatchdogAllowsProgress: a run whose total time far exceeds
// the idle window survives because it keeps writing to the workspace within each
// window — the dynamic timeout lets a slow-but-progressing build finish.
func TestIntegrationIdleWatchdogAllowsProgress(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// ~6s total, a workspace write every 1s; idle window is 3s.
		Cmd:         []string{"sh", "-c", "i=0; while [ $i -lt 6 ]; do echo step$i >> /workspace/progress.log; i=$((i+1)); sleep 1; done"},
		Timeout:     50 * time.Second,
		IdleTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.TimedOut {
		t.Errorf("run made steady workspace progress but was killed as idle: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Duration < 3*time.Second {
		t.Errorf("Duration = %v; expected the full ~6s progressing run to complete", res.Duration)
	}
}

// TestIntegrationIdleWatchdogCPUBusySurvives: a run that produces NO output and
// NO workspace writes but pegs the CPU (a busy loop, standing in for a compiler
// churning silently on one large translation unit) is kept alive by the CPU
// fallback signal and completes, rather than being falsely idle-killed.
func TestIntegrationIdleWatchdogCPUBusySurvives(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// ~8s of pure CPU spin: no stdout, no filesystem writes. Only the CPU
		// probe can tell this apart from a hang.
		Cmd:         []string{"sh", "-c", "end=$(( $(date +%s) + 8 )); while [ $(date +%s) -lt $end ]; do :; done"},
		Timeout:     50 * time.Second,
		IdleTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.TimedOut {
		t.Errorf("CPU-busy run was falsely idle-killed despite high CPU: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
}

// buildEntrypointUserImage builds (once per call, cleaned up via t.Cleanup)
// a small local image reproducing bugbot-p8y4's incident shape: an
// ENTRYPOINT that mangles any argv it doesn't recognize into a
// "Command <argv[0]> not found" error exactly like
// gcr.io/bazel-public/bazel's own client does when handed bugbot's
// "/bin/sh -c ...; exec "$@"" wrapper (see the buildRunArgs doc comment and
// bugbot-cbm5's incident notes), PLUS a non-root USER — bazel-public now
// ships USER ubuntu. This mirrors the DoctorOracleB #148-round replay
// recipe (FROM debian:stable-slim with ENTRYPOINT + USER, a working
// pattern) without depending on network access to a real bazel image. It
// skips the test — rather than failing it — if the build itself cannot
// complete (e.g. the debian:stable-slim base isn't locally cached and there
// is no network to pull it), matching ensureImage's "pull failure is an
// environment gap, not a code failure" convention.
func buildEntrypointUserImage(t *testing.T, s *CLI) string {
	t.Helper()
	dir := t.TempDir()
	// The fake launcher only inspects argv[1] ($1), matching the real
	// bazel client's behavior of erroring on the first CLI token it does not
	// recognize as one of its own subcommands/startup options.
	dockerfile := "FROM debian:stable-slim\n" +
		"RUN useradd -m -u 1500 bugbot-nonroot \\\n" +
		" && printf '#!/bin/sh\\necho \"Command $1 not found\" >&2\\nexit 127\\n' > /usr/local/bin/fake-launcher \\\n" +
		" && chmod +x /usr/local/bin/fake-launcher\n" +
		"USER bugbot-nonroot\n" +
		"ENTRYPOINT [\"/usr/local/bin/fake-launcher\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	tag := "localhost/bugbot-sandbox-test-entrypoint-user:" + randToken()[:16]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.Runtime(), "build", "-t", tag, dir).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build local ENTRYPOINT+USER test image (offline base image pull?): %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(s.Runtime(), "rmi", "-f", tag).Run()
	})
	return tag
}

// TestIntegrationEntrypointAndNonRootUserWorkspace — bugbot-p8y4 — proves
// both fixes together against a REAL locally built image carrying both
// defects at once, exactly as bazel-public now ships them: an ENTRYPOINT
// that mangles any argv it does not recognize, and a non-root USER.
// Without the fix this would surface as either "Command sh not found"
// (entrypoint mangling firing first) or a workspace read failure (the
// non-root container user cannot read the 0700 root-mapped workspace).
// With the fix: bugbot's own command runs, exits with ITS OWN status, and
// reads both the repo-copied file and a WriteFiles-injected file.
func TestIntegrationEntrypointAndNonRootUserWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	// RepoDir MUST be a real git work tree: a non-git RepoDir bypasses
	// wsCache entirely (workspaceCacheKey's isRepo is false) and falls back
	// to prepareWorkspace's plain copyTree, which inherits RepoDir's own
	// mode onto the workspace root — masking the very defect this test
	// exists to catch. The cached path (wsCache.clone, what CLI.Exec
	// actually uses for any git repo — the realistic case, including the
	// bazel monorepo this bead traces to) always creates its pristine dir
	// at a hardcoded 0o700 and cloneTree preserves that mode onto the final
	// per-run workspace, so only a git RepoDir reproduces the real
	// "0700 root-mapped /workspace" incident shape.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "MODULE.bazel"), []byte("REPO_MARKER"), 0o644); err != nil {
		t.Fatalf("write MODULE.bazel: %v", err)
	}
	gitInit(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir: repo,
		Image:   image,
		Cmd:     []string{"sh", "-c", "cat MODULE.bazel && cat repro/injected.txt"},
		WriteFiles: map[string][]byte{
			"repro/injected.txt": []byte("INJECTED_MARKER"),
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (bugbot's own command should run and succeed); stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "REPO_MARKER") {
		t.Errorf("stdout missing repo-copied MODULE.bazel content; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "INJECTED_MARKER") {
		t.Errorf("stdout missing WriteFiles-injected file content; stdout=%q", res.Stdout)
	}
	if strings.Contains(res.Stderr, "not found") || strings.Contains(res.Stderr, "Permission denied") {
		t.Errorf("stderr shows the un-neutralized-entrypoint or unreadable-workspace symptom; stderr=%q", res.Stderr)
	}
}

// TestIntegrationEntrypointDoesNotMangleWorkingToolchain — bugbot-p8y4
// regression test — proves the sandbox no longer MANUFACTURES the
// "not found" output that fools internal/repro's classifySmoke into a false
// toolchain_missing verdict (classifySmoke itself is out of scope for this
// bead — internal/repro is untouched; the fix makes its misleading input
// unreachable at the source). An ENTRYPOINT-bearing image running an
// ordinary, present command must exit 0 with normal, unmangled output —
// not the "Command <cmd> not found" / exit 127 shape bugbot-cbm5's
// gcr.io/bazel-public/bazel incident produced for a non-bazel launcher.
func TestIntegrationEntrypointDoesNotMangleWorkingToolchain(t *testing.T) {
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir: t.TempDir(),
		Image:   image,
		Cmd:     []string{"echo", "toolchain-ok"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (not the argv-mangled-entrypoint's exit 127); stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "toolchain-ok" {
		t.Errorf("Stdout = %q, want %q (unmangled command output)", res.Stdout, "toolchain-ok")
	}
	if combined := res.Stdout + res.Stderr; strings.Contains(combined, "not found") {
		t.Errorf("output contains the classifySmoke 'not found' trigger phrase the un-neutralized entrypoint would have produced: %q", combined)
	}
}

// TestIntegrationNonRootUserCaptureFilesReadBack — bugbot-p8y4 fix-round
// regression (oracle-review B1) — proves the redesigned identity mechanism
// (--user + --userns=keep-id, no chowning) does NOT silently empty
// Result.Captured the way the rejected ":U" design did: :U left the
// workspace owned by a subordinate UID the HOST-side readCaptureFile (which
// runs as the invoking user, after the container exits) could no longer
// read, so a provably-written file came back as "absent" with no error.
//
// RepoDir MUST be a real git work tree (see
// TestIntegrationEntrypointAndNonRootUserWorkspace's doc comment): a
// non-git RepoDir bypasses wsCache and inherits t.TempDir()'s 0755 mode
// onto the workspace root, which stays world-readable even under the
// rejected :U design — masking exactly the defect this test exists to
// catch (found in oracle re-review: this test passed against a
// reintroduced :U mechanism until this fix).
func TestIntegrationNonRootUserCaptureFilesReadBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("placeholder\n"), 0o644); err != nil {
		t.Fatalf("seed repo file: %v", err)
	}
	gitInit(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir:      repo,
		Image:        image,
		Cmd:          []string{"sh", "-c", "echo '<testsuites></testsuites>' > report.xml"},
		CaptureFiles: []string{"report.xml"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	got, present := res.Captured["report.xml"]
	if !present {
		t.Fatal("Captured[report.xml] absent for a non-root USER image — the host-side capture read was blocked (the :U-chown regression this test guards against)")
	}
	if strings.TrimSpace(string(got)) != "<testsuites></testsuites>" {
		t.Errorf("Captured[report.xml] = %q", got)
	}
}

// TestIntegrationNonRootUserWorkspaceFullyRemoved — bugbot-p8y4 fix-round
// regression (oracle-review B1) — proves the per-run workspace is actually
// removable by the invoking host user after a non-root-USER run. The
// rejected ":U" design left it owned by a subordinate UID the host user
// cannot delete, permanently leaking a directory (some measured in the
// hundreds of MB) per run. TMPDIR is redirected to an isolated per-test
// directory so os.MkdirTemp("", "bugbot-sandbox-*") lands somewhere this
// test can glob cleanly, with no cross-test interference.
func TestIntegrationNonRootUserWorkspaceFullyRemoved(t *testing.T) {
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	isolatedTMP := t.TempDir()
	t.Setenv("TMPDIR", isolatedTMP)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir: t.TempDir(),
		Image:   image,
		Cmd:     []string{"sh", "-c", "echo hi > marker.txt"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}

	entries, err := os.ReadDir(isolatedTMP)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", isolatedTMP, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bugbot-sandbox-") {
			t.Errorf("leaked, undeletable-by-host-user workspace after a non-root-USER run: %s", e.Name())
		}
	}
}

// TestIntegrationNonRootUserReusedWorkspaceSecondExec — bugbot-p8y4 fix-round
// regression (oracle-review B1) — proves the reproducer's iteration-workspace
// shape (MaterializeWorkspace once, several Execs against the same
// Spec.Workspace — see repro/repro.go and workspace_tools.go) still works
// for a non-root USER image. The rejected ":U" design broke this after the
// FIRST run: the workspace's host ownership changed mid-lifecycle, so the
// second Exec's applyWriteFiles (running as the host user) hard-failed with
// permission denied.
func TestIntegrationNonRootUserReusedWorkspaceSecondExec(t *testing.T) {
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	repoDir := t.TempDir()
	ws, err := s.MaterializeWorkspace(repoDir)
	if err != nil {
		t.Fatalf("MaterializeWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws) }()

	ctx := context.Background()
	res1, err := s.Exec(ctx, Spec{
		RepoDir:   repoDir,
		Workspace: ws,
		Image:     image,
		Cmd:       []string{"sh", "-c", "echo first > first.txt"},
	})
	if err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("first Exec ExitCode = %d, stderr=%q", res1.ExitCode, res1.Stderr)
	}

	res2, err := s.Exec(ctx, Spec{
		RepoDir:    repoDir,
		Workspace:  ws,
		Image:      image,
		Cmd:        []string{"cat", "first.txt", "second.txt"},
		WriteFiles: map[string][]byte{"second.txt": []byte("second\n")},
	})
	if err != nil {
		t.Fatalf("second Exec (reused workspace): %v", err)
	}
	if res2.ExitCode != 0 {
		t.Fatalf("second Exec ExitCode = %d, stderr=%q (a reused iteration workspace must stay usable after a non-root run)", res2.ExitCode, res2.Stderr)
	}
	if !strings.Contains(res2.Stdout, "first") || !strings.Contains(res2.Stdout, "second") {
		t.Errorf("Stdout = %q, want both first.txt's and the WriteFiles-injected second.txt's content", res2.Stdout)
	}
}

// TestIntegrationNonRootUserGrowthCeilingStillFires — bugbot-p8y4 fix-round
// regression (oracle-review B1) — proves the workspace-growth-ceiling
// watchdog (bugbot-bdqf, landed one commit before this bead) still observes
// a non-root-USER run's filesystem growth. The rejected ":U" design chowned
// the workspace out from under workspaceProgress's host-side stat walk,
// blinding the ceiling entirely (measured: a run wrote 241 MB past an 8 MB
// ceiling and was never killed). Growth must still trip the ceiling here.
//
// RepoDir MUST be a real git work tree — see
// TestIntegrationNonRootUserCaptureFilesReadBack's doc comment: a non-git
// RepoDir's 0755-inherited workspace stays readable by workspaceProgress's
// stat walk even under the rejected :U design, masking this exact defect.
func TestIntegrationNonRootUserGrowthCeilingStillFires(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping integration test")
	}
	s, err := NewCLI(rt, testImage,
		WithCPUs(1), WithMemoryMB(256), WithPidsLimit(128),
		WithTimeout(30*time.Second), WithIdleTimeout(30*time.Second),
		WithWorkspaceGrowthCeilingMB(1), // 1 MiB ceiling
	)
	if err != nil {
		t.Skipf("NewCLI: %v", err)
	}
	ensureImage(t, s)
	image := buildEntrypointUserImage(t, s)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("placeholder\n"), 0o644); err != nil {
		t.Fatalf("seed repo file: %v", err)
	}
	gitInit(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir: repo,
		Image:   image,
		// Write ~4 MiB in one shot, comfortably past the 1 MiB ceiling.
		Cmd: []string{"sh", "-c", "dd if=/dev/zero of=filler.bin bs=1M count=4 2>/dev/null"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true (4 MiB written past a 1 MiB ceiling under a non-root-USER image); ExitCode=%d", res.ExitCode)
	}
}

// TestIntegrationNonRootUserRWMountReadWriteAfterRun — bugbot-p8y4 fix-round
// regression (oracle-review B1) — proves a bugbot-owned (Shared=false)
// writable mount, exactly the shape the dependency-prefetch cache uses
// (deps.go), is still readable AND writable by the HOST user after a
// non-root-USER run. The rejected ":U" design chowned this mount too (even
// though it is never the workspace), breaking the prefetch sentinel's
// host-side read/write and making the cache directory itself unreclaimable.
func TestIntegrationNonRootUserRWMountReadWriteAfterRun(t *testing.T) {
	s := newTestCLI(t)
	image := buildEntrypointUserImage(t, s)

	cacheDir := t.TempDir()
	sentinel := filepath.Join(cacheDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("pre-existing\n"), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{
		RepoDir: t.TempDir(),
		Image:   image,
		Cmd:     []string{"sh", "-c", "cat /cache/sentinel && echo written-by-container >> /cache/sentinel"},
		RWMounts: []ROMount{
			{HostPath: cacheDir, ContainerPath: "/cache"}, // Shared=false, matches deps.go's own cache mounts
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "pre-existing") {
		t.Errorf("container could not read the pre-seeded sentinel; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// HOST-side read/write after the run: the mount's ownership must be
	// untouched (never chowned), unlike the rejected :U design.
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("host-side read of the RW-mounted sentinel after the run: %v", readErr)
	}
	if !strings.Contains(string(got), "written-by-container") {
		t.Errorf("sentinel missing the container's write; got %q", got)
	}
	if writeErr := os.WriteFile(sentinel, []byte("host-write-after-run\n"), 0o644); writeErr != nil {
		t.Fatalf("host-side write of the RW-mounted sentinel after the run: %v (the cache dir must stay host-writable)", writeErr)
	}
	if rmErr := os.RemoveAll(cacheDir); rmErr != nil {
		t.Errorf("host-side removal of the RW-mounted cache dir after the run: %v (the rejected :U design made this undeletable)", rmErr)
	}
}

// TestIntegrationNonRootUserIdentityCaching — bugbot-p8y4 fix-round
// regression — resolveNonRootUser's probe (a real container launch) must
// run at most once per (runtime, image): a second Exec against the same
// non-root-USER image must find the identity already cached rather than
// re-probing.
func TestIntegrationNonRootUserIdentityCaching(t *testing.T) {
	s := newTestCLI(t)
	if s.Runtime() != "podman" {
		t.Skip("identity caching only applies to the podman-gated mechanism")
	}
	image := buildEntrypointUserImage(t, s)
	InvalidateNonRootUserCache(image)

	if _, hit := nonRootUserCache.Load(s.Runtime() + "|" + image); hit {
		t.Fatal("expected a cold cache after InvalidateNonRootUserCache")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Image: image, Cmd: []string{"true"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}

	v, hit := nonRootUserCache.Load(s.Runtime() + "|" + image)
	if !hit {
		t.Fatal("expected the resolved identity to be cached after the first Exec")
	}
	id, ok := v.(containerIdentity)
	if !ok || !id.ok || id.uid != 1500 {
		t.Errorf("cached identity = %+v, want {uid:1500 gid:1500 ok:true}", v)
	}
}
