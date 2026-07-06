//go:build !linux

package sandbox

import "io/fs"

// reflinkOrCopy copies src to dst with a full byte copy. Reflink (FICLONE) is
// a Linux-specific ioctl; every other platform always takes the portable
// path here, matching copyFile exactly.
func reflinkOrCopy(src, dst string, perm fs.FileMode) error {
	return copyFile(src, dst, perm)
}
