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
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: x32SyscallBit, SkipTrue: 0, SkipFalse: 1},
		bpf.RetConstant{Val: unix.SECCOMP_RET_KILL_PROCESS},
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
// input this pure function must handle) still produces the arch gate plus
// the x32 guard plus a bare ALLOW, never an empty/invalid program.
func TestBuildSeccompProgramEmptyDenyStillGatesArch(t *testing.T) {
	program, err := buildSeccompProgram(unix.AUDIT_ARCH_AARCH64, nil)
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}
	raw := decodeSeccompProgram(t, program)
	if len(raw) != 7 {
		t.Fatalf("got %d raw instructions, want 7 (arch-load, arch-jump, kill, nr-load, x32-jump, kill, allow)", len(raw))
	}
}

// seccompDataPacket builds a synthetic "packet" byte buffer carrying nr at
// offset 0 and arch at offset 4 (matching seccomp_data's real field
// layout, see seccomp_linux.go's doc), for bpf.VM.Run.
//
// BIG-ENDIAN, deliberately NOT matching the real kernel's native (little-
// endian on amd64/arm64) in-memory layout of struct seccomp_data:
// golang.org/x/net/bpf's VM is built for classic packet filtering, where
// LoadAbsolute always reads network byte order (loadCommon uses
// binary.BigEndian — verified directly against the library source, not
// assumed). This is purely a property of using this VM as a test
// oracle for the FILTER PROGRAM's instruction logic (branches/compares),
// which is byte-order-agnostic; it has no bearing on
// buildSeccompProgram's own little-endian sock_filter ENCODING (a
// completely separate concern, pinned by TestBuildSeccompProgramShape's
// byte-level round trip) or on the real kernel's execution, which this
// package's live bwrap_integration_test.go tests separately end to end.
func seccompDataPacket(nr, arch uint32) []byte {
	pkt := make([]byte, 64)
	binary.BigEndian.PutUint32(pkt[0:4], nr)
	binary.BigEndian.PutUint32(pkt[4:8], arch)
	return pkt
}

// runSeccompProgram decodes program (buildSeccompProgram's output) and
// executes it against a synthetic seccomp_data packet via the REAL
// golang.org/x/net/bpf virtual machine — not a structural instruction
// comparison — returning the raw action value (e.g.
// unix.SECCOMP_RET_KILL_PROCESS, unix.SECCOMP_RET_ALLOW, or
// unix.SECCOMP_RET_ERRNO|errno). bpf.Disassemble's higher-level rendering
// of a jump (JumpEqual vs. JumpNotEqual with inverted skip fields, see
// TestBuildSeccompProgramShape's doc) is irrelevant here: the VM executes
// the decoded form directly, so both renderings behave identically —
// exactly the property that makes this decode+execute round trip a valid
// behavioral test despite that ambiguity.
func runSeccompProgram(t *testing.T, program []byte, nr, arch uint32) uint32 {
	t.Helper()
	raw := decodeSeccompProgram(t, program)
	insts, allDecoded := bpf.Disassemble(raw)
	if !allDecoded {
		t.Fatalf("bpf.Disassemble left unrecognized raw instructions: %#v", insts)
	}
	vm, err := bpf.NewVM(insts)
	if err != nil {
		t.Fatalf("bpf.NewVM: %v", err)
	}
	ret, err := vm.Run(seccompDataPacket(nr, arch))
	if err != nil {
		t.Fatalf("vm.Run: %v", err)
	}
	return uint32(ret)
}

// TestBuildSeccompProgramX32BypassClosed is oracle finding B1's required
// regression: an x32-encoded (__X32_SYSCALL_BIT set) syscall number sharing
// AUDIT_ARCH_X86_64 with native 64-bit must NEVER reach RET_ALLOW, even
// when the underlying (bit-clear) number is not itself in the deny list —
// the whole point of the guard is that it does not depend on deny-list
// membership at all. Executed via the real BPF VM (runSeccompProgram), not
// a structural assertion, so this fails if the guard's PLACEMENT (not just
// its presence) is wrong — e.g. moved after the deny loop, where it would
// no longer preempt a false ALLOW.
func TestBuildSeccompProgramX32BypassClosed(t *testing.T) {
	deny := []denySyscall{{"fake_a", 111}, {"fake_b", 222}}
	program, err := buildSeccompProgram(unix.AUDIT_ARCH_X86_64, deny)
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}

	cases := []struct {
		name string
		nr   uint32
	}{
		{"x32-encoded denied nr (111 | bit30)", 111 | x32SyscallBit},
		{"x32-encoded arbitrary nr NOT in deny list", 9999 | x32SyscallBit},
		{"x32-encoded nr 0", 0 | x32SyscallBit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runSeccompProgram(t, program, c.nr, uint32(unix.AUDIT_ARCH_X86_64))
			if got == unix.SECCOMP_RET_ALLOW {
				t.Fatalf("x32 nr %#x reached RET_ALLOW — the x32 bypass is NOT closed", c.nr)
			}
			if got != unix.SECCOMP_RET_KILL_PROCESS {
				t.Errorf("x32 nr %#x returned %#x, want RET_KILL_PROCESS (%#x)", c.nr, got, uint32(unix.SECCOMP_RET_KILL_PROCESS))
			}
		})
	}
}

// TestBuildSeccompProgramVMBehavior rounds out the VM-executed behavioral
// coverage for the non-x32 paths: a native, non-x32-bit denied syscall
// gets ERRNO; a native, non-x32-bit, non-denied syscall gets ALLOW; any
// syscall under a non-native arch gets killed.
func TestBuildSeccompProgramVMBehavior(t *testing.T) {
	deny := []denySyscall{{"fake_a", 111}, {"fake_b", 222}}
	program, err := buildSeccompProgram(unix.AUDIT_ARCH_X86_64, deny)
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}

	wantErrno := uint32(unix.SECCOMP_RET_ERRNO) | seccompDenyErrno
	cases := []struct {
		name     string
		nr, arch uint32
		want     uint32
	}{
		{"native denied syscall -> ERRNO", 111, uint32(unix.AUDIT_ARCH_X86_64), wantErrno},
		{"native non-denied syscall -> ALLOW", 42, uint32(unix.AUDIT_ARCH_X86_64), unix.SECCOMP_RET_ALLOW},
		{"compat i386 arch, native-looking nr -> KILL", 111, uint32(unix.AUDIT_ARCH_I386), unix.SECCOMP_RET_KILL_PROCESS},
		{"spoofed/unknown arch -> KILL", 111, 0xdeadbeef, unix.SECCOMP_RET_KILL_PROCESS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runSeccompProgram(t, program, c.nr, c.arch)
			if got != c.want {
				t.Errorf("got %#x, want %#x", got, c.want)
			}
		})
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
