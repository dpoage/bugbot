package ecosystem

// capabilities.go holds the per-ecosystem capability probe registry. Each
// ProbeEntry declares a cheap probe command (run once per sandbox image) and
// an interpret function that maps the result to a named set of available modes.
//
// The probe data was previously defined inline in sandbox/capabilities.go.
// Moving it here makes sandbox a pure executor (it runs the probes and caches
// the results) while the knowledge of WHAT to probe lives in this registry.
//
// # Import topology
//
// internal/ecosystem does NOT import internal/sandbox. sandbox/capabilities.go
// imports internal/ecosystem to read ProbeEntries, then executes the probes
// using its own Exec machinery. ProbeResult is defined here (not in sandbox)
// so the interpret func signatures are cycle-free.
//
// # Adding a new ecosystem probe
//
// Append a ProbeEntry to ProbeEntries. Name it, write a probe argv that exits 0
// when the capability is present (or emits tokens on stdout for multi-mode
// probes), and write an interpret func. Add the ecosystem name to
// allKnownProbeEcosystems in capabilities_test.go.

import "strings"

// ProbeResult is the minimal sandbox result a capability probe interpret func
// needs. sandbox/capabilities.go fills it from a sandbox.Result value; keeping
// this type in internal/ecosystem breaks the ecosystem↔sandbox import cycle.
type ProbeResult struct {
	ExitCode int
	Stdout   string
}

// ProbeEntry describes a single ecosystem's capability probe.
type ProbeEntry struct {
	// Name is the ecosystem key in sandbox.CapabilitySet (e.g. "go").
	Name string
	// Probe is the argv run inside the sandbox (under network=none) to test
	// capabilities. Best-effort: Exec error or timeout → all-false result.
	Probe []string
	// Interpret maps the probe ProbeResult to a map[mode]available.
	// It is called even on non-zero exit codes so it can decide per-mode.
	// An Exec error means best-effort: return all-false.
	Interpret func(r ProbeResult) map[string]bool
}

// ProbeEntries is the ordered registry of capability probes consumed by
// sandbox/capabilities.go. To add a new ecosystem, append a ProbeEntry here.
var ProbeEntries = []ProbeEntry{
	goCapabilityProbe,
	cppCapabilityProbe,
	rustCapabilityProbe,
	jsCapabilityProbe,
	pythonCapabilityProbe,
	bazelCapabilityProbe,
}

// goCapabilityProbe probes whether the Go sandbox image has cgo + a C
// compiler available (prerequisite for `go test -race`).
var goCapabilityProbe = ProbeEntry{
	Name:  "go",
	Probe: []string{"/bin/sh", "-c", "go env CGO_ENABLED && (which cc || which gcc)"},
	Interpret: func(r ProbeResult) map[string]bool {
		if r.ExitCode != 0 {
			return map[string]bool{"race": false}
		}
		cgoEnabled := strings.TrimSpace(r.Stdout)
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
// (ASan, TSan, UBSan).
var cppCapabilityProbe = ProbeEntry{
	Name: "cpp",
	Probe: []string{"/bin/sh", "-c",
		`CXX=$(command -v c++ || command -v g++ || command -v clang++); [ -n "$CXX" ] || exit 1; d=$(mktemp -d); echo 'int main(){return 0;}' > "$d/p.cpp"; for s in address thread undefined; do "$CXX" -fsanitize=$s -g -x c++ "$d/p.cpp" -o "$d/a" 2>/dev/null && echo $s; done`,
	},
	Interpret: func(r ProbeResult) map[string]bool {
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
// (cargo) and the Miri interpreter.
var rustCapabilityProbe = ProbeEntry{
	Name: "rust",
	Probe: []string{"/bin/sh", "-c",
		`command -v cargo >/dev/null 2>&1 && echo cargo; cargo miri --version >/dev/null 2>&1 && echo miri`,
	},
	Interpret: func(r ProbeResult) map[string]bool {
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

// jsCapabilityProbe probes whether the sandbox image has Node.js and whether
// the version is >= 18 (for built-in `node --test`).
var jsCapabilityProbe = ProbeEntry{
	Name: "js",
	Probe: []string{"/bin/sh", "-c",
		`command -v node >/dev/null 2>&1 && echo node; node -e 'process.exit(parseInt(process.versions.node)>=18?0:1)' >/dev/null 2>&1 && echo node_test`,
	},
	Interpret: func(r ProbeResult) map[string]bool {
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
// interpreter and the pytest test runner (including the pytest-timeout plugin).
var pythonCapabilityProbe = ProbeEntry{
	Name: "python",
	Probe: []string{"/bin/sh", "-c",
		`{ command -v python3 >/dev/null 2>&1 || command -v python >/dev/null 2>&1; } && echo python; { python3 -m pytest --version >/dev/null 2>&1 || python -m pytest --version >/dev/null 2>&1 || command -v pytest >/dev/null 2>&1; } && echo pytest; { python3 -c 'import pytest_timeout' >/dev/null 2>&1 || python -c 'import pytest_timeout' >/dev/null 2>&1; } && echo pytest_timeout`,
	},
	Interpret: func(r ProbeResult) map[string]bool {
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

// bazelCapabilityProbe probes whether the sandbox can run the Bazel build
// driver at all. Unlike the language probes above, bazel is not a language
// ecosystem — it is a build DRIVER an agent reaches for on bazel-built repos
// (the_cloud incident: `sh: line 2: exec: bazel: not found` after the plan
// sailed past the 14g0 gate, bugbot-rj3z). Probing it makes the pre-launch
// plan gate and the agent's capability prompt aware of its absence, and
// automatically un-gates it on sandboxes that genuinely provide it (a
// purpose-built image, or a host install exposed via sandbox.host_toolchains).
// bazelisk counts as presence: it IS the bazel launcher.
var bazelCapabilityProbe = ProbeEntry{
	Name: "bazel",
	Probe: []string{"/bin/sh", "-c",
		`{ command -v bazel >/dev/null 2>&1 || command -v bazelisk >/dev/null 2>&1; } && echo bazel`,
	},
	Interpret: func(r ProbeResult) map[string]bool {
		modes := map[string]bool{"bazel": false}
		for _, line := range strings.Split(r.Stdout, "\n") {
			if strings.TrimSpace(line) == "bazel" {
				modes["bazel"] = true
			}
		}
		return modes
	},
}
