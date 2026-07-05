package sandbox

// capabilities.go implements per-ecosystem sandbox capability probing.
//
// The reproducer agent generates test invocations without knowing what the
// sandbox image can actually run; this package fills that gap by probing a
// given image once and caching the result. Each ecosystem declares a cheap
// probe command executed inside a network-none sandbox; the result is a
// CapabilitySet mapping ecosystem → named mode → available.
//
// # Ecosystem registry
//
// capabilityEcosystems is the ordered table of ecosystems. Each entry
// declares:
//   - name: identifier used as the ecosystem key in CapabilitySet.
//   - probe: an argv run once per image under network=none. The probe is
//     best-effort: an infrastructure error (Exec error OR exit code 1) means
//     the mode is marked unavailable; it never blocks or errors the caller.
//   - interpret: maps the probe's Result to a map[string]bool of mode names.
//     Interpretation is pure parsing — the probe exits 0/non-zero to indicate
//     presence/absence, and stdout may carry additional detail.
//
// # Caching
//
// ProbeCapabilities caches keyed on the image string. A sync.Map is used so
// concurrent scan/daemon goroutines never probe the same image twice. A failed
// probe records the best-effort unavailable result in the cache so repeated
// calls for a broken image don't hammer the runtime.
//
// # Adding a new ecosystem
//
// Append a new entry to capabilityEcosystems. Name it, write a probe command
// that exits 0 when the capability is present, and write an interpret func
// that parses the result into named modes. Nothing else needs to change.

import (
	"context"
	"strings"
	"sync"
	"time"
)

// CapabilitySet maps ecosystem name → mode name → available. It is returned
// by ProbeCapabilities and carried through repro.Options into the reproducer
// so the prompt enumerates only what the image can run.
//
// A missing key is equivalent to false (unavailable). CapabilitySet is
// read-only after construction; it is safe to share across goroutines.
type CapabilitySet map[string]map[string]bool

// Available reports whether the named mode is available in the given
// ecosystem. A missing ecosystem or mode returns false (unavailable).
func (cs CapabilitySet) Available(ecosystem, mode string) bool {
	if cs == nil {
		return false
	}
	return cs[ecosystem][mode]
}

// probeEntry describes a single ecosystem's capability probe.
type probeEntry struct {
	// name is the ecosystem key in CapabilitySet (e.g. "go").
	name string
	// probe is the argv run inside the sandbox to test capabilities.
	probe []string
	// interpret maps the sandbox Result to a map[mode]available.
	// It is called even on non-zero exit codes so it can decide per-mode.
	// Err from Exec means best-effort: return all-false.
	interpret func(r Result) map[string]bool
}

// probeTimeout is the per-probe wall-clock ceiling. Probes are cheap (env
// reads, trivial compiles); 30 s is generous.
const probeTimeout = 30 * time.Second

// capabilityEcosystems is the ordered registry of capability probes. To add a
// new ecosystem, append a new probeEntry here.
var capabilityEcosystems = []probeEntry{
	goCapabilityProbe,
	cppCapabilityProbe,
	rustCapabilityProbe,
	jsCapabilityProbe,
	pythonCapabilityProbe,
}

// goCapabilityProbe probes whether the Go sandbox image has cgo + a C
// compiler available (prerequisite for `go test -race`). The probe runs:
//
//	go env CGO_ENABLED
//
// and then checks whether `cc` or `gcc` resolves on PATH with `which cc ||
// which gcc`. CGO_ENABLED=1 alone is not sufficient — the Go toolchain
// re-disables cgo when no C compiler is found. We run two commands via sh so
// a single probe Exec covers both.
//
// Probe command: sh -c 'go env CGO_ENABLED && (which cc || which gcc)'
// Exit 0 → race available; any non-zero exit (including Exec err) → unavailable.
var goCapabilityProbe = probeEntry{
	name:  "go",
	probe: []string{"/bin/sh", "-c", "go env CGO_ENABLED && (which cc || which gcc)"},
	interpret: func(r Result) map[string]bool {
		// Exit 0 means CGO_ENABLED printed successfully AND a C compiler was
		// found. Any other outcome means race is unavailable.
		if r.ExitCode != 0 {
			return map[string]bool{"race": false}
		}
		// Double-check: go env CGO_ENABLED should print "1".
		cgoEnabled := strings.TrimSpace(r.Stdout)
		// Stdout may contain multiple lines if go env output is verbose; look
		// for "1" as the first non-empty line.
		for _, line := range strings.Split(cgoEnabled, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			return map[string]bool{"race": line == "1"}
		}
		return map[string]bool{"race": false}
	},
}

// cppCapabilityProbe probes whether the sandbox image supports C++ sanitizers
// (ASan, TSan, UBSan). The probe finds a C++ compiler then attempts to compile
// a trivial program with each sanitizer flag, echoing a token per available
// mode.
//
// Probe command (sh -c): finds c++/g++/clang++, writes a trivial program to a
// temp dir, then loops over address/thread/undefined flags trying to compile;
// each successful compile emits its token on stdout.
//
// interpret is called even on non-zero exit so partial availability is
// captured — e.g. ASan available but TSan absent. The FULL key set
// {asan, tsan, ubsan} is always returned so allFalse() works correctly.
var cppCapabilityProbe = probeEntry{
	name: "cpp",
	probe: []string{"/bin/sh", "-c",
		`CXX=$(command -v c++ || command -v g++ || command -v clang++); [ -n "$CXX" ] || exit 1; d=$(mktemp -d); echo 'int main(){return 0;}' > "$d/p.cpp"; for s in address thread undefined; do "$CXX" -fsanitize=$s -g -x c++ "$d/p.cpp" -o "$d/a" 2>/dev/null && echo $s; done`,
	},
	interpret: func(r Result) map[string]bool {
		// Always return the full key set so allFalse() enumerates all modes.
		// Parse stdout tokens: "address"→asan, "thread"→tsan, "undefined"→ubsan.
		// interpret is called even on non-zero exit — parse whatever stdout contains.
		modes := map[string]bool{
			"asan":  false,
			"tsan":  false,
			"ubsan": false,
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			switch strings.TrimSpace(line) {
			case "address":
				modes["asan"] = true
			case "thread":
				modes["tsan"] = true
			case "undefined":
				modes["ubsan"] = true
			}
		}
		return modes
	},
}

// rustCapabilityProbe probes whether the sandbox image has the Rust toolchain
// (cargo) and the Miri interpreter (used to expose undefined behavior and data
// races in safe Rust code). The probe finds `cargo` on PATH then attempts
// `cargo miri --version`, echoing a token per available mode.
//
// Probe command (sh -c): checks `command -v cargo` and `cargo miri --version`,
// emitting "cargo" and "miri" tokens on stdout respectively. Both commands
// are silenced; only the echo tokens appear.
//
// interpret is called even on non-zero exit so partial availability is
// captured — e.g. cargo present but miri absent. The FULL key set
// {cargo, miri} is always returned so allFalse() works correctly.
var rustCapabilityProbe = probeEntry{
	name: "rust",
	probe: []string{"/bin/sh", "-c",
		`command -v cargo >/dev/null 2>&1 && echo cargo; cargo miri --version >/dev/null 2>&1 && echo miri`,
	},
	interpret: func(r Result) map[string]bool {
		// Always return the full key set so allFalse() enumerates all modes.
		// Parse stdout tokens: "cargo"→cargo, "miri"→miri. interpret is called
		// even on non-zero exit — parse whatever stdout contains.
		modes := map[string]bool{
			"cargo": false,
			"miri":  false,
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			switch strings.TrimSpace(line) {
			case "cargo":
				modes["cargo"] = true
			case "miri":
				modes["miri"] = true
			}
		}
		return modes
	},
}

// jsCapabilityProbe probes whether the sandbox image has the Node.js runtime
// (mode "node") and a recent enough version to expose the built-in test runner
// `node --test` (mode "node_test", requires node >= 18). The probe finds
// `node` on PATH then runs an inline `-e` script that exits 0 only when the
// parsed major version is 18 or newer, echoing a token per available mode.
//
// Probe command (sh -c): checks `command -v node` and runs
// `node -e 'process.exit(...)'`, emitting "node" and "node_test" tokens on
// stdout respectively. Both commands are silenced; only the echo tokens
// appear.
//
// interpret is called even on non-zero exit so partial availability is
// captured — e.g. node present but older than 18. The FULL key set
// {node, node_test} is always returned so allFalse() works correctly.
var jsCapabilityProbe = probeEntry{
	name: "js",
	probe: []string{"/bin/sh", "-c",
		`command -v node >/dev/null 2>&1 && echo node; node -e 'process.exit(parseInt(process.versions.node)>=18?0:1)' >/dev/null 2>&1 && echo node_test`,
	},
	interpret: func(r Result) map[string]bool {
		// Always return the full key set so allFalse() enumerates all modes.
		// Parse stdout tokens: "node"→node, "node_test"→node_test. interpret is
		// called even on non-zero exit — parse whatever stdout contains.
		modes := map[string]bool{
			"node":      false,
			"node_test": false,
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			switch strings.TrimSpace(line) {
			case "node":
				modes["node"] = true
			case "node_test":
				modes["node_test"] = true
			}
		}
		return modes
	},
}

// pythonCapabilityProbe probes whether the sandbox image has a Python
// interpreter (mode "python", accepts `python3` or `python` on PATH) and the
// pytest test runner (mode "pytest", either via `python3 -m pytest`,
// `python -m pytest`, or `pytest` on PATH). The probe echoes a token per
// available mode; missing tools simply contribute no token.
//
// Probe command (sh -c): a grouped check for the interpreter then a grouped
// check for pytest, emitting "python" and "pytest" tokens on stdout
// respectively. Both checks are silenced; only the echo tokens appear.
//
// interpret is called even on non-zero exit so partial availability is
// captured — e.g. python present but pytest absent. The FULL key set
// {python, pytest, pytest_timeout} is always returned so allFalse() works
// correctly. pytest_timeout reports the pytest-timeout PLUGIN (importable as
// the pytest_timeout module): repro guidance suggests `pytest --timeout` only
// when this probed true, since passing the flag without the plugin fails with
// "unrecognized arguments" and wastes the attempt (bugbot-v9d6).
var pythonCapabilityProbe = probeEntry{
	name: "python",
	probe: []string{"/bin/sh", "-c",
		`{ command -v python3 >/dev/null 2>&1 || command -v python >/dev/null 2>&1; } && echo python; { python3 -m pytest --version >/dev/null 2>&1 || python -m pytest --version >/dev/null 2>&1 || command -v pytest >/dev/null 2>&1; } && echo pytest; { python3 -c 'import pytest_timeout' >/dev/null 2>&1 || python -c 'import pytest_timeout' >/dev/null 2>&1; } && echo pytest_timeout`,
	},
	interpret: func(r Result) map[string]bool {
		// Always return the full key set so allFalse() enumerates all modes.
		// Parse stdout tokens; interpret is called even on non-zero exit —
		// parse whatever stdout contains.
		modes := map[string]bool{
			"python":         false,
			"pytest":         false,
			"pytest_timeout": false,
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			switch strings.TrimSpace(line) {
			case "python":
				modes["python"] = true
			case "pytest":
				modes["pytest"] = true
			case "pytest_timeout":
				modes["pytest_timeout"] = true
			}
		}
		return modes
	},
}

// capCache is the global probe cache keyed by image string.
// Values are CapabilitySet (always non-nil after a probe attempt).
var capCache sync.Map

// ProbeCapabilities probes image once per process and returns a CapabilitySet.
// The probe is best-effort: an Exec error or timeout marks all modes for that
// ecosystem unavailable, but the call never returns an error. The result is
// cached keyed on image so subsequent calls are free.
//
// repoDir is passed so the sandbox spec has a valid RepoDir; it is only used
// for the probe workspace copy (read-only; never written). An empty string
// disables cgo-style probes gracefully (returns all-false caps).
//
// sb must be non-nil. The probe runs under network=none (sandbox default) with
// a short timeout so it cannot stall the caller.
func ProbeCapabilities(ctx context.Context, sb Sandbox, image, repoDir string) CapabilitySet {
	if sb == nil || repoDir == "" {
		return make(CapabilitySet)
	}

	key := image // cache keyed on image name
	if v, ok := capCache.Load(key); ok {
		return v.(CapabilitySet)
	}

	cs := runProbes(ctx, sb, image, repoDir)

	// Store only if another goroutine hasn't beaten us. The losing goroutine
	// discards its result; the winner's value wins — both are equivalent because
	// probes are deterministic for a given image.
	actual, _ := capCache.LoadOrStore(key, cs)
	return actual.(CapabilitySet)
}

// runProbes executes all capability probes and assembles the CapabilitySet.
func runProbes(ctx context.Context, sb Sandbox, image, repoDir string) CapabilitySet {
	cs := make(CapabilitySet, len(capabilityEcosystems))
	for _, e := range capabilityEcosystems {
		spec := Spec{
			RepoDir: repoDir,
			Cmd:     e.probe,
			Image:   image,
			Timeout: probeTimeout,
			// Network defaults to "none" in the sandbox backend; no override needed.
		}
		result, err := sb.Exec(ctx, spec)
		if err != nil {
			// Infrastructure failure → all modes unavailable for this ecosystem.
			cs[e.name] = allFalse(e)
			continue
		}
		modes := e.interpret(result)
		if modes == nil {
			modes = allFalse(e)
		}
		cs[e.name] = modes
	}
	return cs
}

// allFalse returns a modes map with every mode for e set to false.
// Used when a probe fails at the infrastructure level.
func allFalse(e probeEntry) map[string]bool {
	// Build the false map by running interpret on a synthetic zero-exit result
	// so the mode set stays consistent with the ecosystem's declared modes.
	// We pass a Result with ExitCode=1 to guarantee false outcomes.
	modes := e.interpret(Result{ExitCode: 1})
	if modes == nil {
		return map[string]bool{}
	}
	for k := range modes {
		modes[k] = false
	}
	return modes
}

// InvalidateCapabilityCache removes a cached result for image, forcing the next
// ProbeCapabilities call to re-probe. Intended for tests that need a clean slate.
func InvalidateCapabilityCache(image string) {
	capCache.Delete(image)
}
