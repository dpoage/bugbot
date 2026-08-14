//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// nativeSeccompAuditArch is the AUDIT_ARCH_* value the kernel reports in
// seccomp_data.arch for a 64-bit ARM process — see the amd64 file's doc
// comment for the full contract this mirrors.
var nativeSeccompAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)

// bwrapDenySyscalls is the arm64 syscall deny-list: identical to amd64's
// (seccomp_syscalls_amd64.go) MINUS iopl/ioperm/modify_ldt, which have no
// arm64 equivalent syscall at all (no legacy I/O-port space or LDT concept
// on this architecture — there is nothing to deny because there is nothing
// to call). See seccomp_linux.go's buildSeccompProgram doc for the
// action/provenance/exclusion rationale shared by both arch tables.
var bwrapDenySyscalls = []denySyscall{
	{"mount", uint32(unix.SYS_MOUNT)},
	{"umount2", uint32(unix.SYS_UMOUNT2)},
	{"pivot_root", uint32(unix.SYS_PIVOT_ROOT)},
	{"chroot", uint32(unix.SYS_CHROOT)},
	{"open_by_handle_at", uint32(unix.SYS_OPEN_BY_HANDLE_AT)},
	{"name_to_handle_at", uint32(unix.SYS_NAME_TO_HANDLE_AT)},

	{"init_module", uint32(unix.SYS_INIT_MODULE)},
	{"finit_module", uint32(unix.SYS_FINIT_MODULE)},
	{"delete_module", uint32(unix.SYS_DELETE_MODULE)},
	{"kexec_load", uint32(unix.SYS_KEXEC_LOAD)},
	{"kexec_file_load", uint32(unix.SYS_KEXEC_FILE_LOAD)},

	{"bpf", uint32(unix.SYS_BPF)},
	{"perf_event_open", uint32(unix.SYS_PERF_EVENT_OPEN)},
	{"userfaultfd", uint32(unix.SYS_USERFAULTFD)},
	{"process_vm_readv", uint32(unix.SYS_PROCESS_VM_READV)},
	{"process_vm_writev", uint32(unix.SYS_PROCESS_VM_WRITEV)},
	{"keyctl", uint32(unix.SYS_KEYCTL)},
	{"add_key", uint32(unix.SYS_ADD_KEY)},
	{"request_key", uint32(unix.SYS_REQUEST_KEY)},

	{"swapon", uint32(unix.SYS_SWAPON)},
	{"swapoff", uint32(unix.SYS_SWAPOFF)},
	{"reboot", uint32(unix.SYS_REBOOT)},
	{"acct", uint32(unix.SYS_ACCT)},
	{"quotactl", uint32(unix.SYS_QUOTACTL)},
	{"syslog", uint32(unix.SYS_SYSLOG)},
}
