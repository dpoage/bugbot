//go:build !linux

package sandbox

import (
	"errors"
	"os"
)

// bwrapSeccompArchSupported is always false on a non-Linux GOOS — bwrap
// itself is Linux-only (DetectBwrap's runtime.GOOS check always fails
// first), so this file exists only to keep the package building on
// macOS/other dev and CI hosts. See seccomp_linux.go for the real
// implementation.
func bwrapSeccompArchSupported() bool { return false }

// newBwrapSeccompFile is unreachable in practice (DetectBwrap refuses the
// backend on non-Linux before Bwrap.Exec could ever call this), but must
// exist so Bwrap.Exec — which has no build tag of its own — still compiles
// here.
func newBwrapSeccompFile(allowNonNativeArch bool) (*os.File, error) {
	return nil, errors.New("sandbox: bwrap seccomp filter is Linux-only")
}

// DescribeBwrapSeccompPosture is a no-op stub on non-Linux; see
// seccomp_linux.go for the real implementation. Unreachable in practice for
// the same reason as newBwrapSeccompFile above.
func DescribeBwrapSeccompPosture() (archLabel string, deniedCount int) {
	return "", 0
}
