//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// secureCreateFile opens dst for a truncating write, refusing to follow a
// symlink at the leaf itself. This is the second half of applyWriteFiles'
// symlink hardening (see its SECURITY note): secureJoinForWrite already
// refused any symlinked PARENT directory component, but the leaf element
// still needs its own check — a prior untrusted call could have left a plain
// file's name pointing at an arbitrary host path via a symlink instead of a
// directory. O_NOFOLLOW makes the kernel itself fail the open with ELOOP when
// dst already exists as a symlink; it has no effect on creating a brand-new
// regular file (the ordinary case), so legitimate writes are unaffected.
func secureCreateFile(dst string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
}
