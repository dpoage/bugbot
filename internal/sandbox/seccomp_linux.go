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

// buildSeccompProgram assembles the two-tier bwrap seccomp filter described
// in bugbot-6dph's --design (decision 4): a syscall issued under any
// architecture other than nativeArch — the compat 32-bit personality, or a
// spoofed/unrecognized arch value — is killed unconditionally, without
// consulting deny at all; a syscall issued under nativeArch AND without the
// x32 ABI bit set (see x32SyscallBit below) is checked against deny
// (ERRNO(ENOSYS) on a match) and allowed otherwise. Denying the ENTIRE
// non-native architecture (rather than replicating deny under a second,
// hand-maintained i386/ARM-EABI syscall-number table) is what satisfies
// "handles the compat/32-bit audit arch, not just x86_64 native": nothing
// can be allowed by omission under an arch VALUE this filter was never
// taught to interpret. The x32 ABI is the one bypass that trick alone does
// NOT close (oracle finding, fix round 1): x32 shares AUDIT_ARCH_X86_64
// with native 64-bit, so it needs its own guard — see the bit-30 check
// below.
//
// Pure function of its inputs, so the security-relevant program shape is
// exercised by unit tests without a Linux kernel, bwrap, or any syscall —
// mirrors buildBwrapArgs' purity contract.
func buildSeccompProgram(nativeArch uint32, deny []denySyscall) ([]byte, error) {
	if nativeArch == 0 {
		return nil, fmt.Errorf("sandbox: no native AUDIT_ARCH_* mapping available; cannot build a seccomp filter")
	}

	insts := make([]bpf.Instruction, 0, 6+2*len(deny))
	insts = append(insts,
		// A = seccomp_data.arch
		bpf.LoadAbsolute{Off: seccompDataArchOffset, Size: 4},
		// arch == nativeArch? skip the kill-and-return below and fall into
		// the nr-based deny-list; anything else (compat 32-bit, or a
		// spoofed value) falls straight through to RET_KILL_PROCESS.
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: nativeArch, SkipTrue: 1},
		bpf.RetConstant{Val: unix.SECCOMP_RET_KILL_PROCESS},
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
		// or reject the entire bit-set range; we take the latter — no
		// legitimate Go/npm/pip/cargo/Maven/Gradle toolchain path issues
		// an x32 syscall — rather than doubling the deny-list under a
		// second, separately hand-maintained x32 numbering. A does NOT
		// need reloading afterward: JumpBitsSet is non-destructive, so the
		// deny loop below still compares the same nr this instruction
		// tested.
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: x32SyscallBit, SkipTrue: 0, SkipFalse: 1},
		bpf.RetConstant{Val: unix.SECCOMP_RET_KILL_PROCESS},
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
// The returned file's offset is reset to 0 before return: exec.Cmd.
// ExtraFiles shares the SAME open file description (not a copy) with the
// child, so if left at end-of-write, bwrap's read would see immediate EOF.
func newBwrapSeccompFile() (*os.File, error) {
	program, err := buildSeccompProgram(nativeSeccompAuditArch, bwrapDenySyscalls)
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
