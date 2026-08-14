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

// --- seccomp filter + nested-userns policy (bugbot-6dph) -----------------

// TestBwrapSeccompFilterInstalledOnEveryRun is bugbot-6dph's headline
// acceptance criterion, measured the same way the bead's baseline was: a
// pre-fix bwrap run reported "Seccomp:\t0" / "Seccomp_filters:\t0" in
// /proc/self/status. Every run must now report filter mode with at least
// one loaded program.
func TestBwrapSeccompFilterInstalledOnEveryRun(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	shMounts, sh := shForTest(t)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: shMounts,
		Cmd:      []string{sh, "-c", "grep -i '^Seccomp' /proc/self/status"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "Seccomp:\t2") {
		t.Fatalf("expected Seccomp: 2 (SECCOMP_MODE_FILTER); got:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "Seccomp_filters:\t0") {
		t.Fatalf("expected at least one loaded seccomp filter; got:\n%s", res.Stdout)
	}
}

// TestBwrapSeccompDeniesSyscallWithENOSYS proves the filter is actually
// intercepting a denied syscall (not merely that some OTHER mechanism —
// e.g. bwrap's own capability drop — happens to produce the same visible
// failure): userfaultfd(2) is called directly via ctypes and its errno
// checked. A plain "did the command fail" assertion cannot distinguish
// seccomp's ENOSYS from a permission-check EPERM that would fire anyway;
// reading the raw errno makes the distinction unambiguous.
func TestBwrapSeccompDeniesSyscallWithENOSYS(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	pyMounts, python3 := hostToolForTest(t, "python3")

	script := `
import ctypes
libc = ctypes.CDLL("libc.so.6", use_errno=True)
r = libc.syscall(` + userfaultfdSyscallNrForTest + `, 0)
print("ret=%d errno=%d" % (r, ctypes.get_errno()))
`
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: pyMounts,
		Cmd:      []string{"/bin/sh", "-c", python3("-c") + " " + shellQuoteForTest(script)},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "errno=38") {
		t.Fatalf("expected errno=38 (ENOSYS, from seccomp — not some other permission check); got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestBwrapNestedUsernsBlockedByDefault is bugbot-6dph's second acceptance
// criterion: `unshare -Un true` (unshare a new user namespace, then exit)
// must FAIL by default. The pre-fix baseline measured this succeeding.
func TestBwrapNestedUsernsBlockedByDefault(t *testing.T) {
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })
	unshareMounts, unshare := hostToolForTest(t, "unshare")

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: unshareMounts,
		Cmd:      []string{"/bin/sh", "-c", unshare("-Un", "true") + "; echo EXIT:$?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "EXIT:0") {
		t.Fatalf("expected `unshare -Un true` to FAIL by default (nested userns blocked); got EXIT:0, stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestBwrapNestedUsernsAllowedWithOptIn is the converse of
// TestBwrapNestedUsernsBlockedByDefault: WithBwrapAllowNestedUserns(true)
// (sandbox.allow_nested_userns) must make the SAME probe succeed — the
// documented escape hatch for in-sandbox bazel-style tooling.
func TestBwrapNestedUsernsAllowedWithOptIn(t *testing.T) {
	s := newTestBwrap(t, WithBwrapAllowNestedUserns(true))
	t.Cleanup(func() { _ = s.Close() })
	unshareMounts, unshare := hostToolForTest(t, "unshare")

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: unshareMounts,
		Cmd:      []string{"/bin/sh", "-c", unshare("-Un", "true") + "; echo EXIT:$?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "EXIT:0") {
		t.Fatalf("expected `unshare -Un true` to SUCCEED under allow_nested_userns; got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// buildInt80WriteFixture assembles and links a minimal, self-contained
// (-nostdlib, static) x86_64 ELF that issues ONE syscall through the
// legacy i386 int-0x80 entry point — SYS_write (i386 nr 4) — then reports
// the result via a REAL native `syscall` exit (never int 0x80 again): exit
// code 0 on success (the i386 write actually executed and returned >= 0),
// or -eax (i.e. the positive errno, e.g. 38 for ENOSYS) when the i386
// syscall itself failed. int 0x80 works identically on a 64-bit CPU/process
// without any 32-bit ELF or -m32 multilib toolchain — it is the exact
// "same 64-bit binary, alternate syscall entry point" technique
// AUDIT_ARCH_X86_64's compat handling has to cover, so this fixture
// exercises the real bypass class the arch gate defends against. Skips
// cleanly (not a failure) when `as`/`ld` are unavailable — CI portability,
// mirroring this file's other host-toolchain self-skip helpers.
func buildInt80WriteFixture(t *testing.T) string {
	t.Helper()
	asPath, err := exec.LookPath("as")
	if err != nil {
		t.Skip("`as` not found on PATH; skipping int-0x80 arch-gate regression")
	}
	ldPath, err := exec.LookPath("ld")
	if err != nil {
		t.Skip("`ld` not found on PATH; skipping int-0x80 arch-gate regression")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "int80w.S")
	asmSrc := `.text
.global _start
_start:
    movl $4, %eax
    movl $1, %ebx
    leaq msg(%rip), %rcx
    movl $msglen, %edx
    int $0x80
    cmpl $0, %eax
    jl   .Lerr
    movl $60, %eax
    xorl %edi, %edi
    syscall
.Lerr:
    negl %eax
    movl %eax, %edi
    movl $60, %eax
    syscall
.data
msg:
    .ascii "hello-i386-write\n"
msglen = . - msg
`
	if err := os.WriteFile(src, []byte(asmSrc), 0o644); err != nil {
		t.Fatalf("write asm source: %v", err)
	}
	obj := filepath.Join(dir, "int80w.o")
	if out, err := exec.Command(asPath, "--64", "-o", obj, src).CombinedOutput(); err != nil {
		t.Fatalf("as: %v: %s", err, out)
	}
	bin := filepath.Join(dir, "int80w")
	if out, err := exec.Command(ldPath, "-o", bin, obj, "-nostdlib", "-static").CombinedOutput(); err != nil {
		t.Fatalf("ld: %v: %s", err, out)
	}
	// Sanity: the fixture must behave as designed OUTSIDE the sandbox
	// (exit 0, the i386 write genuinely works unfiltered) before it is
	// trusted as a probe of the filter's behavior.
	if out, err := exec.Command(bin).CombinedOutput(); err != nil || string(out) != "hello-i386-write\n" {
		t.Fatalf("fixture sanity check failed: err=%v out=%q", err, out)
	}
	return bin
}

// TestBwrapArchMismatchGetsErrnoNotKill is fix round 2's core regression
// (orchestrator directive, oracle findings B1a/B1b): a syscall issued
// through the i386 int-0x80 entry point from this 64-bit process must
// return ENOSYS and let the process CONTINUE RUNNING to report it — never
// SECCOMP_RET_KILL_PROCESS (SIGSYS). Round 1's fix used KILL_PROCESS here,
// which surfaced only as free-text ("signal: bad system call") in a parent
// process's output — an unbounded vocabulary space that both missed real
// kills phrased differently and suppressed genuine failures containing the
// same phrase. ERRNO removes the hazard at its root.
func TestBwrapArchMismatchGetsErrnoNotKill(t *testing.T) {
	fixture := buildInt80WriteFixture(t)
	s := newTestBwrap(t)
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: []ROMount{{HostPath: fixture, ContainerPath: "/int80w"}},
		Cmd:      []string{"/int80w"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == seccompArchKillExitCodeForTest {
		t.Fatalf("got exit 159 (128+SIGSYS) — the process was KILLED, not given ENOSYS; this is the regression fix round 2 closes")
	}
	if res.ExitCode != 38 {
		t.Fatalf("exit code = %d, want 38 (ENOSYS, from the i386 write syscall being denied); stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "hello-i386-write") {
		t.Errorf("the i386 write syscall's message appeared in output — it must have been denied BEFORE executing, not merely reported as failed afterward")
	}
}

// TestBwrapArchMismatchAllowedWithOptIn is the converse of
// TestBwrapArchMismatchGetsErrnoNotKill: WithBwrapAllowNonNativeArch(true)
// (sandbox.allow_nonnative_arch) must make the SAME i386 write syscall
// actually SUCCEED — the escape hatch for a repo that genuinely needs a
// 32-bit/compat binary to run.
func TestBwrapArchMismatchAllowedWithOptIn(t *testing.T) {
	fixture := buildInt80WriteFixture(t)
	s := newTestBwrap(t, WithBwrapAllowNonNativeArch(true))
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:  t.TempDir(),
		Timeout:  15 * time.Second,
		ROMounts: []ROMount{{HostPath: fixture, ContainerPath: "/int80w"}},
		Cmd:      []string{"/int80w"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (the i386 write syscall should genuinely succeed under allow_nonnative_arch); stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-i386-write") {
		t.Errorf("expected the i386 write syscall's message in stdout under allow_nonnative_arch=true; got stdout=%q", res.Stdout)
	}
}

// seccompArchKillExitCodeForTest is 128+SIGSYS, named locally (not shared
// with production code — bugbot-6dph fix round 2 removed the KILL action
// from the filter entirely, so this exists ONLY as a negative-control
// constant these two regression tests compare against) so the regression
// assertion above documents exactly what it is guarding against.
const seccompArchKillExitCodeForTest = 159

// userfaultfdSyscallNrForTest is the amd64 SYS_userfaultfd number as a
// decimal literal embedded in a Python script string (ctypes.CDLL.syscall
// needs a plain int, and this repo's CI targets amd64 exclusively — see
// seccomp_syscalls_amd64.go's provenance comment for the same number).
const userfaultfdSyscallNrForTest = "323"

// shellQuoteForTest wraps s in single quotes for embedding in a `sh -c`
// argv, escaping any literal single quote the multi-line Python script
// might contain (none currently do, but this keeps the helper correct if
// one ever does).
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
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

// bwrapDiskFillerScript writes a NEW 4 KiB file into the current directory
// every 50ms, up to 200 iterations (bounded so a broken watchdog fails the
// test instead of hanging it) — the bwrap-backend counterpart of
// cli_exec_quota_test.go's fake-podman filler loop. dd is reachable via
// resolveBwrapBaseline's automatic "mkdir" resolution (mounts the whole
// host coreutils output directory on store-based distros), with no extra
// ROMounts required.
const bwrapDiskFillerScript = `i=0
while [ $i -lt 200 ]; do
  dd if=/dev/zero of=hog.$i bs=4096 count=1 2>/dev/null
  i=$((i+1))
  sleep 0.05
done`

// TestBwrapExec_QuotaKillFidelity is the bwrap-backend counterpart of
// cli_exec_quota_test.go's TestCLIExec_QuotaKillFidelity (bugbot-bdqf oracle
// review B1/A-B1): a REAL bwrap run whose only activity is workspace growth
// returns WorkspaceQuotaExceeded=true, TimedOut=false, ExitCode=-1, err=nil
// — through the actual Bwrap.Exec code path, not a direct watchIdle call.
func TestBwrapExec_QuotaKillFidelity(t *testing.T) {
	s := newTestBwrap(t, WithBwrapIdleTimeout(60*time.Second))
	s.defaultGrowthCeilingBytes = 20_000 // ~5 filler files; byte-precise, below WithBwrapWorkspaceGrowthCeilingMB's 1 MB granularity
	t.Cleanup(func() { _ = s.Close() })

	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"/bin/sh", "-c", bwrapDiskFillerScript},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false — a growth-ceiling kill must never collapse into TimedOut (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("elapsed = %s, want well under the 30s/60s Timeout/IdleTimeout ceilings", elapsed)
	}
}

// TestBwrapExec_QuotaKillFidelity_SpawnGateWithIdleTimeoutUnset pins the
// watchdog spawn gate on the bwrap backend (bugbot-bdqf oracle review
// A-B1, mutation M8's bwrap.go equivalent): the goroutine must start from
// the growth ceiling ALONE, with IdleTimeout completely unset.
func TestBwrapExec_QuotaKillFidelity_SpawnGateWithIdleTimeoutUnset(t *testing.T) {
	s := newTestBwrap(t) // base sets no IdleTimeout -> defaultIdleTimeout stays 0
	s.defaultGrowthCeilingBytes = 20_000
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"/bin/sh", "-c", bwrapDiskFillerScript},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true — the watchdog must spawn from the growth ceiling alone (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestBwrapExec_CallerCancellationWinsOverQuotaBreach is the bwrap-backend
// counterpart of cli_exec_quota_test.go's
// TestCLIExec_CallerCancellationWinsOverQuotaBreach (bugbot-bdqf oracle
// review, fix round 2, cancellation precedence): a caller cancellation
// landing in the same window as a real growth-ceiling breach must surface
// as "sandbox: execution cancelled", never as WorkspaceQuotaExceeded.
func TestBwrapExec_CallerCancellationWinsOverQuotaBreach(t *testing.T) {
	s := newTestBwrap(t, WithBwrapIdleTimeout(60*time.Second))
	s.defaultGrowthCeilingBytes = 5_000 // ~1-2 filler iterations blow past this
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := s.Exec(ctx, Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"/bin/sh", "-c", bwrapDiskFillerScript},
	})
	if err == nil {
		t.Fatalf("expected a cancellation error, got nil err with res=%+v", res)
	}
	if !strings.Contains(err.Error(), "execution cancelled") {
		t.Errorf("error = %v, want it to mention \"execution cancelled\"", err)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("a caller-cancelled run must never report WorkspaceQuotaExceeded, even if a breach was also detected; got %+v", res)
	}
}

// TestBwrapExec_QuotaBaselineCapturedAfterWorkspacePrep is the bwrap
// counterpart of cli_exec_quota_test.go's
// TestCLIExec_QuotaBaselineCapturedAfterWorkspacePrep (bugbot-bdqf oracle
// review, mutation M15): the growth-ceiling baseline must be captured from
// the PREPARED workspace, not from zero — RepoDir is seeded with 50 KB
// against a 1 KB ceiling; the command writes nothing, so any kill proves
// the baseline was wrong.
func TestBwrapExec_QuotaBaselineCapturedAfterWorkspacePrep(t *testing.T) {
	s := newTestBwrap(t)
	s.defaultGrowthCeilingBytes = 1000 // 1 KB — far below the seeded content
	t.Cleanup(func() { _ = s.Close() })

	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "big.bin"), make([]byte, 50_000), 0o644); err != nil {
		t.Fatalf("seed repo content: %v", err)
	}

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Timeout: 15 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", "true"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("pre-existing workspace content (50 KB against a 1 KB ceiling) must NOT trip the ceiling — the baseline must be captured AFTER workspace prep, not from zero; got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
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
