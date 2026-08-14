//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

// nativeSeccompAuditArch is the AUDIT_ARCH_* value the kernel reports in
// seccomp_data.arch for a 64-bit x86 process — the one arch buildSeccompProgram
// evaluates the deny-list under; every other arch value (compat 32-bit i386,
// or a spoofed value) is denied with ENOSYS by default, or unfiltered
// under the sandbox.allow_nonnative_arch opt-out — see
// buildSeccompProgram's doc.
var nativeSeccompAuditArch = uint32(unix.AUDIT_ARCH_X86_64)

// bwrapDenySyscalls is the amd64 syscall deny-list (bugbot-6dph --design
// decision 5): syscall numbers come from golang.org/x/sys/unix's generated
// amd64 SYS_* table, never hand-typed. See seccomp_linux.go's package doc
// comment on buildSeccompProgram for the action (ERRNO(ENOSYS)) and the full
// provenance/exclusion rationale (why unshare/setns/clone and ptrace are
// deliberately absent).
var bwrapDenySyscalls = []denySyscall{
	// Mount-namespace / filesystem-boundary manipulation. bwrap's own user
	// namespace grants the sandboxed process CAP_SYS_ADMIN etc. WITHIN that
	// namespace, so these are otherwise reachable — this is the actual
	// container-escape-relevant half of the deny-list.
	{"mount", uint32(unix.SYS_MOUNT)},
	{"umount2", uint32(unix.SYS_UMOUNT2)},
	{"pivot_root", uint32(unix.SYS_PIVOT_ROOT)},
	{"chroot", uint32(unix.SYS_CHROOT)},
	{"open_by_handle_at", uint32(unix.SYS_OPEN_BY_HANDLE_AT)},
	{"name_to_handle_at", uint32(unix.SYS_NAME_TO_HANDLE_AT)},

	// Kernel module / kexec: arbitrary-code-in-kernel primitives with zero
	// relevance to any offline language-toolchain build.
	{"init_module", uint32(unix.SYS_INIT_MODULE)},
	{"finit_module", uint32(unix.SYS_FINIT_MODULE)},
	{"delete_module", uint32(unix.SYS_DELETE_MODULE)},
	{"kexec_load", uint32(unix.SYS_KEXEC_LOAD)},
	{"kexec_file_load", uint32(unix.SYS_KEXEC_FILE_LOAD)},

	// Recurring kernel-exploit-primitive / privilege-escalation CVE surface:
	// unprivileged BPF verifier bugs, perf_event_open side-channel and
	// local-root CVEs, userfaultfd used to widen exploit race windows, and
	// keyring/process-memory syscalls used for credential/secret scraping.
	{"bpf", uint32(unix.SYS_BPF)},
	{"perf_event_open", uint32(unix.SYS_PERF_EVENT_OPEN)},
	{"userfaultfd", uint32(unix.SYS_USERFAULTFD)},
	{"process_vm_readv", uint32(unix.SYS_PROCESS_VM_READV)},
	{"process_vm_writev", uint32(unix.SYS_PROCESS_VM_WRITEV)},
	{"keyctl", uint32(unix.SYS_KEYCTL)},
	{"add_key", uint32(unix.SYS_ADD_KEY)},
	{"request_key", uint32(unix.SYS_REQUEST_KEY)},

	// Host-affecting admin operations no build tool has any reason to call.
	{"swapon", uint32(unix.SYS_SWAPON)},
	{"swapoff", uint32(unix.SYS_SWAPOFF)},
	{"reboot", uint32(unix.SYS_REBOOT)},
	{"acct", uint32(unix.SYS_ACCT)},
	{"quotactl", uint32(unix.SYS_QUOTACTL)},
	// syslog(2) reads the kernel ring buffer — a KASLR/kernel-pointer info
	// leak vector when accessible.
	{"syslog", uint32(unix.SYS_SYSLOG)},

	// x86-only raw hardware access: I/O port permissions and LDT
	// manipulation, classic local-privesc primitives with no arm64
	// equivalent syscall at all.
	{"iopl", uint32(unix.SYS_IOPL)},
	{"ioperm", uint32(unix.SYS_IOPERM)},
	{"modify_ldt", uint32(unix.SYS_MODIFY_LDT)},
}
