//go:build !linux

package sandbox

import "os/exec"

// setBwrapProcAttr is a no-op on non-Linux: bwrap itself is Linux-only
// (DetectBwrap always fails first on other platforms), so this file exists
// only to keep the package building on macOS/other dev and CI hosts. See
// bwrap_proc_linux.go for the real implementation.
func setBwrapProcAttr(*exec.Cmd) {}

// killBwrapProcessGroup is a no-op on non-Linux; see setBwrapProcAttr.
func killBwrapProcessGroup(*exec.Cmd) {}
