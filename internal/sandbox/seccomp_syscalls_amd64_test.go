//go:build linux && amd64

package sandbox

import "testing"

// TestBwrapDenySyscallsAmd64Membership pins the deny-list BY NAME (oracle
// finding B3): TestBuildSeccompProgramShape only asserts a synthetic
// 2-entry list's SHAPE, and TestBwrapDenySyscallsNoDuplicateNumbers/
// TestBwrapDenySyscallsExcludesUserns only assert absence, so nothing
// previously caught a dropped entry — deleting {"bpf", ...} left the full
// unit+integration suite green. For a control whose entire value IS its
// membership, a rebase drop or a "compat trim" must fail a test.
func TestBwrapDenySyscallsAmd64Membership(t *testing.T) {
	want := []string{
		"mount", "umount2", "pivot_root", "chroot", "open_by_handle_at", "name_to_handle_at",
		"init_module", "finit_module", "delete_module", "kexec_load", "kexec_file_load",
		"bpf", "perf_event_open", "userfaultfd", "process_vm_readv", "process_vm_writev",
		"keyctl", "add_key", "request_key",
		"swapon", "swapoff", "reboot", "acct", "quotactl", "syslog",
		"iopl", "ioperm", "modify_ldt",
	}
	got := make(map[string]bool, len(bwrapDenySyscalls))
	for _, sc := range bwrapDenySyscalls {
		got[sc.name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("bwrapDenySyscalls (amd64) is missing %q", name)
		}
	}
	if len(bwrapDenySyscalls) != len(want) {
		t.Errorf("bwrapDenySyscalls (amd64) has %d entries, want exactly %d (%v) — an unexpected extra entry needs deliberate review, not silent growth",
			len(bwrapDenySyscalls), len(want), want)
	}
}
