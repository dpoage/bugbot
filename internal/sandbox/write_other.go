//go:build !unix

package sandbox

import "os"

// secureCreateFile is the non-unix fallback: O_NOFOLLOW has no portable
// equivalent outside unix (see write_unix.go). bugbot's supported deployment
// targets are unix (Linux/macOS running Linux containers via podman/docker),
// so this is graceful degradation on an unsupported platform, not a
// sanctioned safety mode — secureJoinForWrite's parent-directory walk still
// refuses a symlinked intermediate component regardless of platform.
func secureCreateFile(dst string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}
