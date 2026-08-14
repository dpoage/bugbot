//go:build linux && !amd64 && !arm64

package sandbox

// nativeSeccompAuditArch is zero on any Linux GOARCH this package has not
// been taught an AUDIT_ARCH_* mapping and syscall-number table for.
// bwrapSeccompArchSupported (seccomp_linux.go) reports false in that case
// and DetectBwrap refuses the bwrap backend entirely rather than installing
// a filter that silently doesn't apply (bugbot-6dph acceptance criterion
// 3) — this keeps the package COMPILING on every Linux GOARCH while only
// amd64 and arm64 (seccomp_syscalls_amd64.go / seccomp_syscalls_arm64.go)
// actually run bwrap.
var nativeSeccompAuditArch = uint32(0)

// bwrapDenySyscalls is empty: with no native arch mapping, buildSeccompProgram
// refuses before this would ever matter.
var bwrapDenySyscalls []denySyscall
