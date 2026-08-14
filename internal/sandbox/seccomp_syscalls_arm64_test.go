//go:build linux && arm64

package sandbox

import "testing"

// TestBwrapDenySyscallsArm64Membership is TestBwrapDenySyscallsAmd64Membership's
// arm64 counterpart (oracle finding B3): the 25 entries common to both
// arches, MINUS the 3 amd64-only x86 I/O-port/LDT syscalls that have no
// arm64 equivalent. See seccomp_syscalls_arm64.go's doc.
func TestBwrapDenySyscallsArm64Membership(t *testing.T) {
	want := []string{
		"mount", "umount2", "pivot_root", "chroot", "open_by_handle_at", "name_to_handle_at",
		"init_module", "finit_module", "delete_module", "kexec_load", "kexec_file_load",
		"bpf", "perf_event_open", "userfaultfd", "process_vm_readv", "process_vm_writev",
		"keyctl", "add_key", "request_key",
		"swapon", "swapoff", "reboot", "acct", "quotactl", "syslog",
	}
	got := make(map[string]bool, len(bwrapDenySyscalls))
	for _, sc := range bwrapDenySyscalls {
		got[sc.name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("bwrapDenySyscalls (arm64) is missing %q", name)
		}
	}
	if len(bwrapDenySyscalls) != len(want) {
		t.Errorf("bwrapDenySyscalls (arm64) has %d entries, want exactly %d (%v)",
			len(bwrapDenySyscalls), len(want), want)
	}
	for _, x86Only := range []string{"iopl", "ioperm", "modify_ldt"} {
		if got[x86Only] {
			t.Errorf("bwrapDenySyscalls (arm64) unexpectedly contains x86-only %q", x86Only)
		}
	}
}
