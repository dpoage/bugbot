package ecosystem

// infer.go maps a finding's file extension (or a repro plan's command argv)
// to the ecosystem key used by CapabilitySet (see capabilities.go's
// ProbeEntries). This is the single place that answers "what toolchain does
// this finding/command need?" so the claim-time queue gate
// (repro/promote.go's promoteOne) and the pre-launch plan gate
// (repro/repro.go's Attempt) share one inference rule instead of drifting.
//
// Only ecosystems with a base-toolchain-presence probe mode are gated (see
// BaseMode): C/C++ images are assumed toolchain-complete — the cpp probe
// only measures optional sanitizer support, not compiler presence — so
// findings in that language are never blocked by this gate. js, python,
// rust, and go are gated: js/python/rust because their absence is the
// original production incident (interpreters missing from a bazel-only
// image); go because a host_toolchains image that omits go (bugbot-bslx)
// burns a full reproducer attempt on exit 127 "go: not found" otherwise.
// Go's gate has an asymmetric degradation rule the others don't (see
// repro.goAvailable): a CapabilitySet with no "go" entry at all — i.e. the
// probe never ran for this image — stays ungated, matching Go's pre-bslx
// behavior; only a probe that DID run and explicitly found no go binary
// blocks.

import (
	"path/filepath"
	"strings"
)

// extEcosystem maps a lower-cased file extension to the gated ecosystem key.
// Extensions with no gated ecosystem (Go, C/C++, or any language with no
// capability probe) are intentionally absent — InferFromExtension returns ""
// for them, and callers treat "" as ungated (never blocked).
var extEcosystem = map[string]Ecosystem{
	".py":  EcosystemPython,
	".pyi": EcosystemPython,
	".js":  EcosystemJS,
	".mjs": EcosystemJS,
	".cjs": EcosystemJS,
	".jsx": EcosystemJS,
	".ts":  EcosystemJS,
	".tsx": EcosystemJS,
	".mts": EcosystemJS,
	".cts": EcosystemJS,
	".rs":  EcosystemRust,
	".go":  EcosystemGo,
}

// baseMode is the CapabilitySet mode name representing "the ecosystem's
// toolchain is present at all", as opposed to an optional feature mode (Go's
// "race", C++'s "asan"/"tsan"/"ubsan"). Only ecosystems listed here are
// gated by InferFromExtension/InferFromCmd.
var baseMode = map[Ecosystem]string{
	EcosystemJS:     "node",
	EcosystemPython: "python",
	EcosystemRust:   "cargo",
	EcosystemBazel:  "bazel",
	EcosystemGo:     "present",
}

// InferFromExtension infers the gated ecosystem for a finding's file path
// from its extension, or "" if the file's language is not gated (see file
// doc). JavaScript and TypeScript both map to EcosystemJS: both run on node.
func InferFromExtension(file string) Ecosystem {
	ext := strings.ToLower(filepath.Ext(file))
	return extEcosystem[ext]
}

// cmdEcosystem maps a well-known ecosystem binary's base name to its gated
// ecosystem key, for InferFromCmd.
var cmdEcosystem = map[string]Ecosystem{
	"node":   EcosystemJS,
	"npm":    EcosystemJS,
	"npx":    EcosystemJS,
	"yarn":   EcosystemJS,
	"pnpm":   EcosystemJS,
	"jest":   EcosystemJS,
	"vitest": EcosystemJS,

	"python":  EcosystemPython,
	"python3": EcosystemPython,
	"pytest":  EcosystemPython,

	"cargo": EcosystemRust,

	// go's base mode ("present") backs the bugbot-bslx gate: repro.goAvailable
	// applies the asymmetric degradation rule (see file doc) on top of this
	// mapping, so a plan launching `go test`/`go build` is only rejected when
	// the probe explicitly found no go binary, never when it never ran.
	"go": EcosystemGo,

	// bazel is a build DRIVER, not a language ecosystem: it is gated so a
	// plan reaching for `bazel test` on a bazel-built repo is rejected
	// pre-launch when the sandbox has no bazel (bugbot-rj3z), instead of
	// burning a sandbox run into exit-127 environment_error. bazelisk is
	// the bazel launcher and counts as the same requirement.
	"bazel":    EcosystemBazel,
	"bazelisk": EcosystemBazel,
}

// InferFromCmd infers the gated ecosystem a repro plan's command argv
// requires. See InferToolFromCmd; this drops the matched binary name for
// callers that only need the ecosystem.
func InferFromCmd(cmd []string) Ecosystem {
	eco, _ := InferToolFromCmd(cmd)
	return eco
}

// InferToolFromCmd infers the gated ecosystem a repro plan's command argv
// requires AND the recognized binary base name that matched, by inspecting
// the argv for a recognized ecosystem binary. A `bash -c "..."`/`sh -c
// "..."` wrapper — the pattern reproSandboxGuidance instructs the agent to
// use for multi-step commands — is unwrapped one level first so a binary
// named inside the quoted script is still found. Returns ("", "") when no
// gated ecosystem binary is recognized (e.g. "make", "cmake", or any command
// this function does not know about); such commands are never blocked by
// the gate this feeds.
//
// The matched name matters for launcher families whose members are NOT
// interchangeable argv (bugbot-4z7m): `bazel` and `bazelisk` both infer
// EcosystemBazel, but a sandbox where only bazelisk resolves must reject a
// `bazel` argv — availability is probed and gated per binary NAME there.
func InferToolFromCmd(cmd []string) (Ecosystem, string) {
	// normalizeArgv (interp.go) is the shared shell/wrapper/absolute-path
	// normalization pipeline also used by DetectEcosystem — bugbot-ds90:
	// this used to re-implement a bespoke one-level "bash -c" unwrap here,
	// missing compound shell strings, "-lc"/"-ec" flag clusters, and benign
	// wrappers (env, exec, timeout, nice) that DetectEcosystem already knew
	// how to peel.
	argv := normalizeArgv(cmd)
	for _, tok := range argv {
		base := filepath.Base(tok)
		if eco, ok := cmdEcosystem[base]; ok {
			return eco, base
		}
	}
	return "", ""
}

// BaseMode returns the CapabilitySet mode name representing eco's base
// toolchain presence, or "" if eco has no gated base mode (see file doc).
func BaseMode(eco Ecosystem) string {
	return baseMode[eco]
}

// ToolchainBinary returns the missing BINARY name an operator should install
// for eco, for user-facing "image lacks X" messages (bugbot-813i). For most
// gated ecosystems BaseMode doubles as the binary name ("node", "python",
// "cargo", "bazel"), but Go's base mode is the probe token "present"
// (bugbot-bslx), not a binary — eco itself ("go") is the binary there. Falls
// back to eco when no base mode is registered. Every user-facing
// blocked-toolchain message MUST use this, never raw BaseMode.
func ToolchainBinary(eco Ecosystem) string {
	if eco == EcosystemGo {
		return string(eco)
	}
	if bin := baseMode[eco]; bin != "" {
		return bin
	}
	return string(eco)
}
