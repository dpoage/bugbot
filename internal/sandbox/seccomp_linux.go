//go:build linux

package sandbox

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// seccomp_data field offsets (linux/seccomp.h):
//
//	struct seccomp_data {
//	        int nr;                       // offset 0, size 4
//	        __u32 arch;                   // offset 4, size 4
//	        __u64 instruction_pointer;    // offset 8
//	        __u64 args[6];                // offset 16
//	};
const (
	seccompDataNrOffset   = 0
	seccompDataArchOffset = 4
)

// x32SyscallBit is __X32_SYSCALL_BIT (asm/unistd.h): set on every x32 ABI
// syscall number (asm/unistd_x32.h renders each x32 nr as this bit plus the
// underlying 32-bit number). The x32 ABI shares AUDIT_ARCH_X86_64 with
// native 64-bit, so buildSeccompProgram's arch gate alone does not catch
// it — see the dedicated guard there (oracle finding B1, bugbot-6dph fix
// round 1).
const x32SyscallBit = 0x40000000

// seccompDenyErrno is the errno every denied syscall reports back to the
// sandboxed process (bugbot-6dph --design decision 4): ENOSYS, not EPERM, so
// a toolchain doing "does this kernel support X" feature-detection on a rare
// syscall (errno == ENOSYS) degrades gracefully down its fallback path
// instead of reading the failure as an ACL/permission problem. This matches
// systemd's SystemCallErrorNumber= default and current Docker/containerd
// practice for their own syscall denylists.
var seccompDenyErrno = uint32(unix.ENOSYS)

// denySyscall names one syscall a bwrap seccomp filter blocks, carrying its
// resolved native-architecture number so a future reader/reviewer never has
// to re-derive a bare integer from a kernel header. nr comes from
// golang.org/x/sys/unix's generated per-GOARCH SYS_* constants (see
// seccomp_syscalls_amd64.go / seccomp_syscalls_arm64.go) — never hand-typed.
type denySyscall struct {
	name string
	nr   uint32
}

// bwrapSeccompArchSupported reports whether this GOARCH has a known native
// AUDIT_ARCH_* mapping (see seccomp_syscalls_{amd64,arm64,unsupported}.go).
// DetectBwrap calls this so an unsupported architecture refuses the whole
// bwrap backend rather than silently installing no filter (bugbot-6dph
// acceptance criterion 3: "if you cannot cover an arch, refuse/degrade
// loudly").
func bwrapSeccompArchSupported() bool {
	return nativeSeccompAuditArch != 0
}

// DescribeBwrapSeccompPosture reports the effective bwrap syscall-filter
// posture for doctor's advisory reporting (bugbot-6dph acceptance criterion
// 6): this host's GOARCH (the AUDIT_ARCH_* this filter evaluates the
// deny-list under) and how many syscalls that deny-list blocks. There is no
// "installed?" boolean to report: every bwrap run installs this filter
// unconditionally, since DetectBwrap already refuses the whole backend on
// an architecture bwrapSeccompArchSupported doesn't cover.
func DescribeBwrapSeccompPosture() (archLabel string, deniedCount int) {
	return runtime.GOARCH, len(bwrapDenySyscalls)
}

// buildSeccompProgram assembles the bwrap seccomp filter described in
// bugbot-6dph's --design: a syscall issued under any architecture other
// than nativeArch (the compat 32-bit personality, or a spoofed/
// unrecognized arch value) is denied without consulting deny at all,
// UNLESS allowNonNativeArch opts out of that gate entirely (--design
// decision, fix round 2: sandbox.allow_nonnative_arch — an escape hatch
// for a repo that genuinely needs a 32-bit/compat binary to run, mirroring
// allow_nested_userns's opt-out shape); a syscall issued under nativeArch
// AND without the x32 ABI bit set (see x32SyscallBit below) is checked
// against deny and allowed otherwise; an x32-bit-set syscall under
// nativeArch is ALSO gated by allowNonNativeArch, for the same reason.
//
// ONE action for every denial: RET_ERRNO(ENOSYS) — not
// RET_KILL_PROCESS (fix round 2, oracle findings B1a/B1b, orchestrator
// directive). A kill terminates the denied process outright, which
// bugbot-repro's classifiers cannot distinguish from a genuine crash
// (SIGSYS surfaces only as free-text — "signal: bad system call" — in a
// parent process's own reported output, an unbounded and unreliable
// vocabulary space to match against, see fix round 1's now-reverted
// internal/repro/seccomp_kill.go for the failed attempt), so it both
// missed real kills phrased differently AND suppressed genuine failures
// that happened to contain that same phrase (including this repo's own
// source, before the revert). ERRNO removes the hazard at its root: no
// syscall under a non-native arch or with the x32 bit ever EXECUTES
// either way — the security property is identical — but the denied
// process keeps running and must handle the failure itself, exactly like
// every other syscall in the deny-list already does. This also
// incidentally fixes a real nit: seccomp_data.nr is a signed int, so
// nr == -1 (an invalid-syscall-number probe some libc feature-detection
// code issues deliberately) has EVERY bit set including bit 30 and was
// being caught by the x32 guard; under KILL that silently changed its
// observed behavior from the kernel's own "unimplemented syscall number"
// ENOSYS to a SIGSYS kill, whereas under ERRNO the observed result
// (ENOSYS) is unchanged either way.
//
// Evaluated and rejected: surfacing WTERMSIG==SIGSYS as a sandbox.Result
// fact instead (so repro's classifiers could veto on a hard signal rather
// than a kill's absence of one). bwrap is pid 1 of its sandbox and reaps
// orphans, but reports only its OWN direct child's exit status — the
// common real case is a 32-bit tool as a child of a TEST RUNNER (go test,
// pytest, ...), never orphaned, so its signal death is invisible at the
// sandbox-Exec layer without either patching bwrap itself or a
// SECCOMP_USER_NOTIF listener, which --add-seccomp-fd cannot install
// (that requires the separate, unrelated SECCOMP_RET_USER_NOTIF action
// plus a notification fd bwrap has no flag to plumb through). Recorded
// here so a future maintainer does not re-attempt it without first
// solving that plumbing gap.
//
// Pure function of its inputs, so the security-relevant program shape is
// exercised by unit tests without a Linux kernel, bwrap, or any syscall —
// mirrors buildBwrapArgs' purity contract.
func buildSeccompProgram(nativeArch uint32, deny []denySyscall, allowNonNativeArch bool) ([]byte, error) {
	if nativeArch == 0 {
		return nil, fmt.Errorf("sandbox: no native AUDIT_ARCH_* mapping available; cannot build a seccomp filter")
	}

	nonNativeAction := uint32(unix.SECCOMP_RET_ERRNO) | seccompDenyErrno
	if allowNonNativeArch {
		nonNativeAction = unix.SECCOMP_RET_ALLOW
	}

	insts := make([]bpf.Instruction, 0, 6+2*len(deny))
	insts = append(insts,
		// A = seccomp_data.arch
		bpf.LoadAbsolute{Off: seccompDataArchOffset, Size: 4},
		// arch == nativeArch? skip the non-native return below and fall
		// into the nr-based deny-list; anything else (compat 32-bit, or a
		// spoofed value) falls straight through to nonNativeAction.
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: nativeArch, SkipTrue: 1},
		bpf.RetConstant{Val: nonNativeAction},
		// A = seccomp_data.nr
		bpf.LoadAbsolute{Off: seccompDataNrOffset, Size: 4},
		// x32 ABI guard (oracle finding B1, bugbot-6dph fix round 1): the
		// x32 ABI (64-bit registers, 32-bit-numbered syscalls) shares
		// AUDIT_ARCH_X86_64 with native 64-bit, so the arch check above
		// does not distinguish it. An x32 syscall's nr has
		// __X32_SYSCALL_BIT (bit 30, 0x40000000) set and so never equals
		// any (bit-clear) deny-list number below — unguarded, EVERY x32
		// syscall (mount, bpf, kexec_load, ...) would silently fall
		// through to the final RET_ALLOW. seccomp(2) is explicit that a
		// policy must either recognize both bit-set and bit-clear numbers
		// or reject the entire bit-set range; we take the latter (same
		// nonNativeAction as the arch gate, so allowNonNativeArch governs
		// both consistently) rather than doubling the deny-list under a
		// second, separately hand-maintained x32 numbering. A does NOT
		// need reloading afterward: JumpBitsSet is non-destructive, so the
		// deny loop below still compares the same nr this instruction
		// tested.
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: x32SyscallBit, SkipTrue: 0, SkipFalse: 1},
		bpf.RetConstant{Val: nonNativeAction},
	)
	for _, sc := range deny {
		insts = append(insts,
			// nr == sc.nr? fall through to the ERRNO return immediately
			// below (SkipTrue: 0); otherwise skip over it (SkipFalse: 1) to
			// reach the next check.
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: sc.nr, SkipTrue: 0, SkipFalse: 1},
			bpf.RetConstant{Val: unix.SECCOMP_RET_ERRNO | seccompDenyErrno},
		)
	}
	insts = append(insts, bpf.RetConstant{Val: unix.SECCOMP_RET_ALLOW})

	raw, err := bpf.Assemble(insts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: assemble seccomp filter: %w", err)
	}

	// raw is []bpf.RawInstruction{Op uint16; Jt, Jf uint8; K uint32} — the
	// exact wire layout of the kernel's struct sock_filter (8 bytes each, no
	// padding: K is already 4-byte-aligned at offset 4). Encoded in
	// LITTLE-ENDIAN: the filter is always built and consumed on the same
	// host that runs it, and every GOARCH this package supports (amd64,
	// arm64) is little-endian as Go targets them, so this is the host's
	// native order, not an arbitrary wire-protocol choice.
	buf := make([]byte, 0, len(raw)*8)
	w := bufferWriter{buf: &buf}
	if err := binary.Write(&w, binary.LittleEndian, raw); err != nil {
		return nil, fmt.Errorf("sandbox: encode seccomp filter: %w", err)
	}
	return buf, nil
}

// bufferWriter adapts a *[]byte to io.Writer for binary.Write, avoiding a
// bytes.Buffer allocation for what is always a small (well under 1KB),
// one-shot encode.
type bufferWriter struct{ buf *[]byte }

func (w *bufferWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// newBwrapSeccompFile builds this host's native-architecture seccomp filter
// (buildSeccompProgram + this GOARCH's denySyscalls, see
// seccomp_syscalls_amd64.go / seccomp_syscalls_arm64.go) and delivers it as
// a sealed memfd: an anonymous, in-memory file (memfd_create(2)) sealed
// against further shrink/grow/write (F_ADD_SEALS) once the program is
// written, mirroring Flatpak's own delivery mechanism for exported cBPF
// programs. A memfd (rather than a pipe) gives bwrap normal fstat/lseek
// semantics on the fd it reads from, and the seal is cheap defense-in-depth
// against the content changing under bwrap between our write and its read —
// though the fd is never shared with the sandboxed process itself, only
// bwrap's own (pre-seccomp) setup code.
//
// allowNonNativeArch is sandbox.allow_nonnative_arch (Bwrap.
// allowNonNativeArch), threaded straight to buildSeccompProgram.
//
// The returned file's offset is reset to 0 before return: exec.Cmd.
// ExtraFiles shares the SAME open file description (not a copy) with the
// child, so if left at end-of-write, bwrap's read would see immediate EOF.
func newBwrapSeccompFile(allowNonNativeArch bool) (*os.File, error) {
	program, err := buildSeccompProgram(nativeSeccompAuditArch, bwrapDenySyscalls, allowNonNativeArch)
	if err != nil {
		return nil, err
	}

	fd, err := unix.MemfdCreate("bugbot-seccomp-filter", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("sandbox: memfd_create seccomp filter: %w", err)
	}
	f := os.NewFile(uintptr(fd), "bugbot-seccomp-filter")

	if _, err := f.Write(program); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sandbox: write seccomp filter: %w", err)
	}
	// Seal against future shrink/grow/write so the program bwrap eventually
	// reads can never diverge from what we just assembled. Best-effort is
	// NOT acceptable here (an unsealed, mutable filter fd defeats the point
	// of the seal) — but sealing is unconditionally available on any kernel
	// new enough to have memfd_create with MFD_ALLOW_SEALING at all (both
	// landed together in Linux 3.17), so a failure here means something is
	// genuinely wrong and must not be silently ignored.
	seals := unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sandbox: seal seccomp filter memfd: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sandbox: rewind seccomp filter memfd: %w", err)
	}
	return f, nil
}
