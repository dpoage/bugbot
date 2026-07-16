package ecosystem

// infer.go maps a finding's file extension (or a repro plan's command argv)
// to the ecosystem key used by CapabilitySet (see capabilities.go's
// ProbeEntries). This is the single place that answers "what toolchain does
// this finding/command need?" so the claim-time queue gate
// (repro/promote.go's promoteOne) and the pre-launch plan gate
// (repro/repro.go's Attempt) share one inference rule instead of drifting.
//
// Only ecosystems with a base-toolchain-presence probe mode are gated (see
// BaseMode): Go and C/C++ images are assumed toolchain-complete — bugbot
// itself requires a Go toolchain to build, and the cpp probe only measures
// optional sanitizer support, not compiler presence — so findings in those
// languages are never blocked by this gate. Only the ecosystems whose
// absence is the actual production incident (js, python, rust interpreters
// simply missing from a bazel-only image) are gated.

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
// gated ecosystem binary is recognized (e.g. "go", "make", "cmake", or any
// command this function does not know about); such commands are never
// blocked by the gate this feeds.
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
