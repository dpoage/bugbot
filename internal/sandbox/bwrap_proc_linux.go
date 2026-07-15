//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// setBwrapProcAttr places the launched bwrap process in its own process
// group so killBwrapProcessGroup can signal it and every descendant it
// spawned — bwrap itself plus whatever it exec'd inside the sandbox — with a
// single syscall, mirroring the container backend's "docker rm -f" reaching
// every process inside the container.
func setBwrapProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killBwrapProcessGroup sends SIGKILL to the process group of a timed-out or
// cancelled run. bwrap's own --die-with-parent flag already guarantees its
// children die if bwrap itself is killed; this signals the whole group
// directly so a wedged bwrap that is slow to notice its parent's demise (or
// a caller-cancellation, which --die-with-parent does not cover at all,
// since bugbot itself keeps running) is still torn down promptly.
func killBwrapProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
