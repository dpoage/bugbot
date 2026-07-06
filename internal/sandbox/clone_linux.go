//go:build linux

package sandbox

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// reflinkOrCopy copies src to dst, preferring a copy-on-write reflink
// (FICLONE) so cloning a pristine workspace (wsCache) is near-instant and
// disk-cheap on filesystems that support it (btrfs, XFS with reflink=1,
// overlayfs layered on one of those). Any ioctl failure — ENOTSUP/EOPNOTSUPP
// (filesystem doesn't support reflink), EXDEV (cross-device, e.g. pristine
// and workspace on different tmp mounts), or anything else — falls back to a
// full byte copy so unsupported filesystems (ext4, tmpfs, most CI runners)
// still work correctly, just without the speedup.
func reflinkOrCopy(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if ierr := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); ierr == nil {
		return nil
	}

	// Reflink unsupported or failed partway: reset dst and fall back to a
	// full copy from the start of src.
	if _, serr := in.Seek(0, io.SeekStart); serr != nil {
		return serr
	}
	if terr := out.Truncate(0); terr != nil {
		return terr
	}
	if _, serr := out.Seek(0, io.SeekStart); serr != nil {
		return serr
	}
	if _, werr := io.Copy(out, in); werr != nil {
		return werr
	}
	return nil
}
