//go:build linux

package sandbox

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// decodeSeccompProgram reverses buildSeccompProgram's little-endian
// sock_filter encoding back into []bpf.RawInstruction, exercising the FULL
// assemble+encode round trip (not just the pre-encoded instruction slice)
// so a wrong offset/endianness assumption in the encoder would fail this
// test too, not just a hand-inspected instruction list.
func decodeSeccompProgram(t *testing.T, program []byte) []bpf.RawInstruction {
	t.Helper()
	if len(program)%8 != 0 {
		t.Fatalf("program length %d not a multiple of 8 (sock_filter is 8 bytes)", len(program))
	}
	n := len(program) / 8
	raw := make([]bpf.RawInstruction, n)
	for i := range n {
		off := i * 8
		raw[i] = bpf.RawInstruction{
			Op: binary.LittleEndian.Uint16(program[off : off+2]),
			Jt: program[off+2],
			Jf: program[off+3],
			K:  binary.LittleEndian.Uint32(program[off+4 : off+8]),
		}
	}
	return raw
}

func TestBuildSeccompProgramRefusesZeroArch(t *testing.T) {
	if _, err := buildSeccompProgram(0, nil); err == nil {
		t.Fatal("expected an error for nativeArch == 0, got nil")
	}
}

// TestBuildSeccompProgramShape pins the exact instruction sequence
// buildSeccompProgram's doc promises: an arch gate (kill on mismatch) always
// comes first, followed by one JumpIf+RetConstant(ERRNO) pair per denied
// syscall in order, followed by a single trailing RetConstant(ALLOW).
// Decoded via the real x/net/bpf disassembler so a malformed encoding (wrong
// offset, wrong K value, wrong jump target) shows up as a decode mismatch,
// not just a byte-level diff.
func TestBuildSeccompProgramShape(t *testing.T) {
	deny := []denySyscall{{"fake_a", 111}, {"fake_b", 222}}
	program, err := buildSeccompProgram(unix.AUDIT_ARCH_X86_64, deny)
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}

	got := decodeSeccompProgram(t, program)

	// Build the expected raw instructions independently, by assembling the
	// SAME instruction descriptions buildSeccompProgram's doc promises,
	// rather than round-tripping through bpf.Disassemble: cBPF's jt/jf pair
	// has more than one equivalent higher-level rendering (e.g. "JumpEqual
	// skipTrue=0/skipFalse=1" and "JumpNotEqual skipTrue=1/skipFalse=0"
	// decode to the identical raw bytes), so asserting on the DECODED form
	// would be asserting an arbitrary canonicalization choice, not the
	// actual program semantics. Comparing raw Op/Jt/Jf/K fields sidesteps
	// that ambiguity entirely while still validating the little-endian
	// sock_filter encoding end to end.
	wantInsts := []bpf.Instruction{
		bpf.LoadAbsolute{Off: seccompDataArchOffset, Size: 4},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(unix.AUDIT_ARCH_X86_64), SkipTrue: 1},
		bpf.RetConstant{Val: unix.SECCOMP_RET_KILL_PROCESS},
		bpf.LoadAbsolute{Off: seccompDataNrOffset, Size: 4},
	}
	for _, sc := range deny {
		wantInsts = append(wantInsts,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: sc.nr, SkipTrue: 0, SkipFalse: 1},
			bpf.RetConstant{Val: unix.SECCOMP_RET_ERRNO | seccompDenyErrno},
		)
	}
	wantInsts = append(wantInsts, bpf.RetConstant{Val: unix.SECCOMP_RET_ALLOW})

	want, err := bpf.Assemble(wantInsts)
	if err != nil {
		t.Fatalf("assemble expected instructions: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d raw instructions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instruction %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestBuildSeccompProgramEmptyDenyStillGatesArch confirms an empty deny-list
// (never actually used in production — see bwrapDenySyscalls — but a valid
// input this pure function must handle) still produces the arch gate plus a
// bare ALLOW, never an empty/invalid program.
func TestBuildSeccompProgramEmptyDenyStillGatesArch(t *testing.T) {
	program, err := buildSeccompProgram(unix.AUDIT_ARCH_AARCH64, nil)
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}
	raw := decodeSeccompProgram(t, program)
	if len(raw) != 5 {
		t.Fatalf("got %d raw instructions, want 5 (arch-load, arch-jump, kill, nr-load, allow)", len(raw))
	}
}

// TestBwrapDenySyscallsNoDuplicateNumbers guards against a copy-paste error
// in seccomp_syscalls_{amd64,arm64}.go silently aliasing two names onto the
// same number (which would make one entry's ERRNO shadow the other's,
// invisibly narrowing the deny-list without any test noticing a missing
// name).
func TestBwrapDenySyscallsNoDuplicateNumbers(t *testing.T) {
	seen := make(map[uint32]string, len(bwrapDenySyscalls))
	for _, sc := range bwrapDenySyscalls {
		if prev, dup := seen[sc.nr]; dup {
			t.Errorf("syscall number %d used by both %q and %q", sc.nr, prev, sc.name)
		}
		seen[sc.nr] = sc.name
	}
}

// TestBwrapDenySyscallsExcludesUserns pins the bugbot-6dph --design decision
// that unshare/setns/clone/clone3 are deliberately NEVER in the deny-list:
// userns policy belongs exclusively to --disable-userns (buildBwrapArgs), so
// seccomp-blocking these would make sandbox.allow_nested_userns's opt-in
// impossible to actually exercise (a seccomp filter cannot be lifted by the
// sandboxed process once installed).
func TestBwrapDenySyscallsExcludesUserns(t *testing.T) {
	excluded := map[string]bool{"unshare": true, "setns": true, "clone": true, "clone3": true}
	for _, sc := range bwrapDenySyscalls {
		if excluded[sc.name] {
			t.Errorf("bwrapDenySyscalls must not deny %q — nested-userns policy belongs to --disable-userns alone, see buildBwrapArgs", sc.name)
		}
	}
}

func TestBwrapSeccompArchSupportedOnThisBuild(t *testing.T) {
	// This test file is linux-only and this repo's CI/dev targets are
	// amd64/arm64 (see seccomp_syscalls_amd64.go / _arm64.go) — on either,
	// nativeSeccompAuditArch must be nonzero.
	if !bwrapSeccompArchSupported() {
		t.Errorf("bwrapSeccompArchSupported() = false on GOARCH with a syscall table wired up")
	}
}

func TestNewBwrapSeccompFileProducesReadableSealedMemfd(t *testing.T) {
	f, err := newBwrapSeccompFile()
	if err != nil {
		t.Fatalf("newBwrapSeccompFile: %v", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil || n != 8 {
		t.Fatalf("read first instruction: n=%d err=%v (memfd offset must be rewound to 0 before return)", n, err)
	}

	// Sealed against writes: F_SEAL_WRITE must make any further write fail.
	if _, err := f.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Error("write to sealed seccomp memfd unexpectedly succeeded")
	}
}
