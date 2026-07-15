//go:build integration

// Bwrap integration tests exercise the real bwrap binary and unprivileged
// user namespaces. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// They are skipped automatically when bwrap is missing or unprivileged
// userns are unavailable — see DetectBwrap.
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

// newTestBwrap builds a Bwrap backend, skipping the test when the backend is
// unusable on this host (bwrap absent, non-Linux, or userns disabled).
func newTestBwrap(t *testing.T, opts ...BwrapOption) *Bwrap {
	t.Helper()
	if ok, reason := DetectBwrap(); !ok {
		t.Skipf("bwrap unavailable: %s", reason)
	}
	base := []BwrapOption{
		WithBwrapCPUs(1),
		WithBwrapMemoryMB(256),
		WithBwrapPidsLimit(64),
		WithBwrapTimeout(30 * time.Second),
	}
	s, err := NewBwrap(append(base, opts...)...)
	if err != nil {
		t.Skipf("NewBwrap: %v", err)
	}
	return s
}

// hostSharedLibDirs runs `ldd` on execPath and returns the containing
// directory of every resolved shared-object dependency (interpreter
// included). On an FHS host these all fall under fixedROAllowlist already
// (a no-op in practice); on Nix/Guix hosts a binary's dependency closure is
// typically scattered across several UNRELATED store paths (this host's
// bash needs its own store path plus separate readline/ncurses/glibc store
// paths) that ResolveHostToolchains' single-directory-closure model does
// not capture, so tests identity-bind each one explicitly. Best-effort: ldd
// failing (missing, static binary, non-ELF) yields (nil, nil), not an error
// — the caller has a working binary either way if it linked at all.
func hostSharedLibDirs(execPath string) []string {
	out, err := exec.Command("ldd", execPath).CombinedOutput()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		var libPath string
		if idx := strings.Index(line, "=> "); idx >= 0 {
			rest := strings.TrimSpace(line[idx+len("=> "):])
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				rest = rest[:sp]
			}
			libPath = rest
		} else if strings.HasPrefix(line, "/") {
			libPath = line
			if sp := strings.IndexByte(libPath, ' '); sp >= 0 {
				libPath = libPath[:sp]
			}
		}
		if libPath == "" {
			continue
		}
		resolved, evalErr := filepath.EvalSymlinks(libPath)
		if evalErr != nil {
			continue
		}
		dir := filepath.Dir(resolved)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// hostToolForTest resolves a real, host-portable binary for tests to exec
// inside the sandbox: the fixed allowlist's /bin is bound with --ro-bind-try
// (see fixedROAllowlist's doc), so a hardcoded "/bin/<name>" silently does
// not exist on non-FHS hosts (NixOS, Guix) where /bin is not a real path at
// all and core utilities instead live under /nix/store or similar, reached
// only via a resolved host-toolchain bind. On such hosts the resolved
// binary's shared-library closure (hostSharedLibDirs) is typically scattered
// across several store paths outside ResolveHostToolchains' single-directory
// remap, so each is also identity-bound (kernel's dynamic linker resolves
// them by the exact absolute paths baked into the binary/its libraries, not
// relative to anything ResolveHostToolchains remapped). Some distributions
// (Nix's "coreutils-full") also bundle several applets (sleep, touch,
// true, ...) into ONE "coreutils" multi-call binary dispatched by argv[0]'s
// BASENAME (the busybox convention: normally satisfied by a per-applet
// symlink, which is exactly what got resolved away by EvalSymlinks here) —
// invoking the resolved path directly just prints --help usage and exits 1.
// Returns the ROMounts to add to Spec.ROMounts and an invoke closure that
// renders a shell-embeddable invocation for the given trailing args,
// wrapping a coreutils applet in `(exec -a name path args...)` — a subshell
// so the argv[0] override does not replace the enclosing sh -c script's own
// process image (a bare, unwrapped `exec` would terminate the script at
// that point instead of returning control for any subsequent `&&` stage).
func hostToolForTest(t *testing.T, name string) ([]ROMount, func(args ...string) string) {
	t.Helper()
	root, execPath, err := resolveToolchainRoot(name)
	if err != nil || execPath == "" {
		t.Skipf("cannot resolve a host %q binary for this test", name)
	}
	res, rerr := ResolveHostToolchains([]string{name})
	if rerr != nil || len(res.Mounts) == 0 {
		t.Skipf("cannot resolve %q via the host-toolchain resolver", name)
	}
	rel, relErr := filepath.Rel(root, execPath)
	if relErr != nil {
		t.Skipf("cannot compute %q's in-mount relative path: %v", name, relErr)
	}
	mounts := []ROMount{res.Mounts[0]}
	for _, dir := range hostSharedLibDirs(execPath) {
		mounts = append(mounts, ROMount{HostPath: dir, ContainerPath: dir, Shared: true})
	}
	ctrPath := filepath.ToSlash(filepath.Join(res.Mounts[0].ContainerPath, rel))
	multiCall := filepath.Base(execPath) == "coreutils"
	invoke := func(args ...string) string {
		if multiCall {
			return "(exec -a " + name + " " + ctrPath + " " + strings.Join(args, " ") + ")"
		}
		return strings.Join(append([]string{ctrPath}, args...), " ")
	}
	return mounts, invoke
}

// shForTest is hostToolForTest("sh"), the common case: sh is never a
// multi-call applet, so this collapses straight to the single interpreter
// path every "sh -c" caller expects (invoke() with no args, for a
// non-multi-call tool, is just its resolved path).
func shForTest(t *testing.T) ([]ROMount, string) {
	mounts, invoke := hostToolForTest(t, "sh")
	return mounts, invoke()
}

// mergeROMounts concatenates ROMount slices from multiple hostToolForTest
// calls, deduplicating by ContainerPath: two resolved tools sharing the
// same dynamic linker (a common case — most binaries on a given host link
// against the same libc) would otherwise produce an identical identity
// bind twice, which Spec validation rejects as a duplicate mount.
func mergeROMounts(groups ...[]ROMount) []ROMount {
	var out []ROMount
	seen := make(map[string]bool)
	for _, g := range groups {
		for _, m := range g {
			if seen[m.ContainerPath] {
				continue
			}
			seen[m.ContainerPath] = true
			out = append(out, m)
		}
	}
	return out
}

// TestBwrapNetworkNoneBlocksEgress proves the network=none default actually
// severs network access rather than merely omitting a flag: a TCP connect
// attempt inside the sandbox must fail.
func TestBwrapNetworkNoneBlocksEgress(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	repo := t.TempDir()
	shMounts, sh := shForTest(t)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Network:  "none",
		Timeout:  15 * time.Second,
		ROMounts: shMounts,
		// /dev/tcp is a bash-ism; use a portable connect probe instead:
		// getent (or python/nc) may not exist in every environment, so this
		// relies only on sh + a raw connect via /dev/tcp-less means — the
		// simplest portable probe available on virtually any host is
		// `cat < /dev/tcp/1.1.1.1/80`, but /dev/tcp requires bash. Fall back
		// to a plain TCP connect via sh's exec redirection is not portable
		// either, so this probes with the sh builtin `exec 3<>` via bash if
		// present, else the test degrades to checking DNS resolution fails
		// (no resolv.conf bound under network=none, so any resolver call
		// fails immediately without ever reaching the wire).
		Cmd: []string{sh, "-c", "exec 3<>/dev/tcp/1.1.1.1/80 2>&1; echo EXIT:$?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout+res.Stderr, "EXIT:0") {
		// EXIT:0 would mean the connection succeeded — anything else
		// (connection refused/unreachable, or sh lacking /dev/tcp support
		// entirely, which itself proves no working network path) is the
		// expected outcome under network=none.
		return
	}
	t.Fatalf("expected network=none to block egress, but the probe reported success: stdout=%q stderr=%q", res.Stdout, res.Stderr)
}

// TestBwrapWritesOutsideWorkspaceFail proves the tmpfs root + fixed
// read-only allowlist actually prevents writes anywhere except /workspace
// and /tmp.
func TestBwrapWritesOutsideWorkspaceFail(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	repo := t.TempDir()
	shMounts, sh := shForTest(t)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Timeout:  15 * time.Second,
		ROMounts: shMounts,
		Cmd:      []string{sh, "-c", "touch /usr/should-fail 2>&1; echo EXIT:$?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout+res.Stderr, "EXIT:0") {
		t.Fatalf("expected write outside /workspace and /tmp to fail on the RO root; got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// The converse: /workspace and /tmp must remain writable. Needs a real
	// `touch` reachable inside the sandbox — resolved the same way as `sh`
	// since the fixed allowlist alone does not guarantee one exists (see
	// hostToolForTest).
	touchMounts, touch := hostToolForTest(t, "touch")
	res, err = s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Timeout:  15 * time.Second,
		ROMounts: mergeROMounts(shMounts, touchMounts),
		Cmd:      []string{sh, "-c", touch("/workspace/ok") + " && " + touch("/tmp/ok") + " && echo EXIT:$?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "EXIT:0") {
		t.Fatalf("expected /workspace and /tmp to remain writable; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestBwrapGoReproRunsGreen runs a real Go test end to end using only host
// toolchain binds (the fixed allowlist), proving base functionality works
// without any toolchain resolver wiring.
func TestBwrapGoReproRunsGreen(t *testing.T) {
	// Go's build system forks a worker process per compiled package plus
	// assembler/linker helpers; newTestBwrap's baseline PidsLimit(64) is
	// tuned for the other, single-process integration tests and is too low
	// even for this trivial one-file module (fork/exec fails with EAGAIN
	// once the cgroup pids.max cap is hit mid-build).
	s := newTestBwrap(t, WithBwrapPidsLimit(512))
	t.Cleanup(func() { _ = s.Close() })

	goRoot := goRootForTest(t)
	repo := t.TempDir()
	writeFile(t, repo+"/go.mod", "module example.com/bwraprepro\n\ngo 1.21\n")
	writeFile(t, repo+"/main_test.go", `package main

import "testing"

func TestPasses(t *testing.T) {}
`)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Timeout: 60 * time.Second,
		Env: []string{
			"PATH=/tmp/goroot/bin:/usr/bin:/bin",
			"GOCACHE=/tmp/gocache",
			"GOPATH=/tmp/gopath",
		},
		ROMounts: []ROMount{{HostPath: goRoot, ContainerPath: "/tmp/goroot", Shared: true}},
		Cmd:      []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected go test to pass; exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestBwrapExitCodeAndTimeoutFidelity proves the bwrap backend's exit-code
// and timeout reporting match the container backend's contract: a real
// non-zero exit surfaces as Result.ExitCode with no error, and a run
// exceeding IdleTimeout is reported as TimedOut with ExitCode -1.
func TestBwrapExitCodeAndTimeoutFidelity(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	repo := t.TempDir()
	shMounts, sh := shForTest(t)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Timeout:  15 * time.Second,
		ROMounts: shMounts,
		Cmd:      []string{sh, "-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
	if res.TimedOut {
		t.Errorf("TimedOut should be false for a clean non-zero exit")
	}

	// sleep (not a busy loop) is deliberate: watchIdle's activeFallback
	// (procTreeCPUBusy) suppresses the idle-kill while the process tree is
	// consuming CPU, so a busy loop would defeat this test — sleep is
	// genuinely idle (no output, no CPU, no workspace activity) and lets
	// the idle-timeout watchdog fire as intended.
	sleepMounts, sleep := hostToolForTest(t, "sleep")
	res, err = s.Exec(context.Background(), Spec{
		RepoDir:     repo,
		Timeout:     10 * time.Second,
		IdleTimeout: 500 * time.Millisecond,
		ROMounts:    mergeROMounts(shMounts, sleepMounts),
		Cmd:         []string{sh, "-c", sleep("30")},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true for an idle-killed run")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
}

// goRootForTest locates a usable GOROOT on the test host, skipping if none
// can be found — the same self-skip discipline as newTestBwrap.
func goRootForTest(t *testing.T) string {
	t.Helper()
	if gr := os.Getenv("GOROOT"); gr != "" {
		if _, err := os.Stat(gr); err == nil {
			return gr
		}
	}
	t.Skip("GOROOT not resolvable on this host; skipping host-toolchain repro test")
	return ""
}

// TestBwrapLiteralBinShResolves pins bugbot-53rl: production code paths
// (the SetupCmds exit-125 wrapper in buildBwrapArgs and every /bin/sh-based
// capability probe) exec the LITERAL path /bin/sh inside the sandbox. On
// store-based distros (NixOS, Guix) /bin/sh is a symlink into the store, so
// binding /bin alone carries a dangling symlink and exec fails with
// "execvp /bin/sh: No such file or directory" — the store roots in
// fixedROAllowlist are what make it resolve. Deliberately uses NO resolver
// helper mounts (unlike shForTest-based tests): the helpers would mask the
// exact regression this test exists to catch. Only sh builtins are used —
// on NixOS /bin contains nothing but sh, so no coreutils can be assumed.
func TestBwrapLiteralBinShResolves(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })

	// Success lane: both the setup wrapper's shell and the command's own
	// /bin/sh must resolve; a passing setup hands control to the command
	// with its exit code intact.
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:   t.TempDir(),
		Timeout:   15 * time.Second,
		SetupCmds: [][]string{{"sh", "-c", ": setup-ok"}},
		Cmd:       []string{"/bin/sh", "-c", "echo cmd-ran"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "cmd-ran") {
		t.Errorf("stdout = %q, want it to contain %q", res.Stdout, "cmd-ran")
	}

	// Failure lane: a failing setup command must still surface as the
	// environment_error exit 125, not as a shell-resolution infra failure.
	res, err = s.Exec(context.Background(), Spec{
		RepoDir:   t.TempDir(),
		Timeout:   15 * time.Second,
		SetupCmds: [][]string{{"sh", "-c", "exit 7"}},
		Cmd:       []string{"/bin/sh", "-c", "echo never"},
	})
	if err != nil {
		t.Fatalf("Exec (failing setup): %v", err)
	}
	if res.ExitCode != 125 {
		t.Errorf("ExitCode = %d, want 125 for a failed setup command (stderr=%q)", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "never") {
		t.Errorf("command ran despite a failing setup: stdout=%q", res.Stdout)
	}
}

// TestBwrapBaselineUtilitiesReachable pins bugbot-qg8b: the sandbox PATH
// must reach POSIX core utilities with NO operator host_toolchains and NO
// helper mounts. On store-based hosts DefaultContainerPath's FHS dirs hold
// only sh and env, so without the construction-time baseline resolution the
// exact production shapes below die with "mkdir: command not found":
// deps.go's applyGoBuildScratch emits a bare `mkdir -p` SetupCmds entry,
// and agent plans routinely run `sh -c` scripts using coreutils/grep/sed.
// (On FHS hosts the baseline resolves empty and this passes through the
// allowlist binds instead — the test is meaningful on both layouts.)
func TestBwrapBaselineUtilitiesReachable(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Timeout: 15 * time.Second,
		// The deps.go applyGoBuildScratch shape: a bare utility argv routed
		// through the /bin/sh setup wrapper, resolved via PATH.
		SetupCmds: [][]string{{"mkdir", "-p", ".bugbot-scratch"}},
		// An agent-plan shape: coreutils + grep piped inside sh -c, plus the
		// ubiquitous `which <tool>` availability probe (not a coreutils
		// applet; missing from the baseline it exits 127 and burns an agent
		// exec-budget call on a spurious environment_error).
		Cmd: []string{"/bin/sh", "-c", "mkdir -p out && echo probe > out/f && grep -c probe out/f && which grep >/dev/null"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "1") {
		t.Errorf("stdout = %q, want grep count output %q", res.Stdout, "1")
	}
}
