package sandbox

// capabilities.go implements per-ecosystem sandbox capability probing.
//
// The reproducer agent generates test invocations without knowing what the
// sandbox image can actually run; this package fills that gap by probing a
// given image once and caching the result. Probe data (commands + interpret
// functions) is declared in internal/ecosystem/capabilities.go; this file
// executes those probes inside the sandbox and caches the results.
//
// # Ecosystem registry
//
// The probe table is ecoreg.ProbeEntries — an ordered list of ProbeEntry
// values, one per ecosystem. Each entry declares:
//   - Name: identifier used as the ecosystem key in CapabilitySet.
//   - Probe: an argv run once per image under network=none.
//   - Interpret: maps the probe's Result to a map[string]bool of mode names.
//
// To add a new ecosystem probe, append a ProbeEntry to
// internal/ecosystem/capabilities.go. Nothing here needs to change.
//
// # Caching
//
// ProbeCapabilities caches keyed on the image string. A sync.Map is used so
// concurrent scan/daemon goroutines never probe the same image twice. A failed
// probe records the best-effort unavailable result so repeated calls for a
// broken image don't hammer the runtime.
//
// # Adding a new ecosystem
//
// Append a new ProbeEntry in internal/ecosystem/capabilities.go. Name it,
// write a probe command that exits 0 when the capability is present, and write
// an interpret func. Nothing else needs to change here.

import (
	"context"
	"sync"
	"time"

	ecoreg "github.com/dpoage/bugbot/internal/ecosystem"
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
func (cs CapabilitySet) Available(eco, mode string) bool {
	if cs == nil {
		return false
	}
	return cs[eco][mode]
}

// probeTimeout is the per-probe wall-clock ceiling. Probes are cheap (env
// reads, trivial compiles); 30 s is generous.
const probeTimeout = 30 * time.Second

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
		// No sandbox or no repo to probe against — return empty capability set.
		cs := make(CapabilitySet)
		for _, e := range ecoreg.ProbeEntries {
			cs[e.Name] = allFalse(e)
		}
		return cs
	}

	if v, ok := capCache.Load(image); ok {
		return v.(CapabilitySet)
	}
	actual, _ := capCache.LoadOrStore(image, runProbes(ctx, sb, image, repoDir))
	return actual.(CapabilitySet)
}

// runProbes executes all capability probes and assembles the CapabilitySet.
func runProbes(ctx context.Context, sb Sandbox, image, repoDir string) CapabilitySet {
	cs := make(CapabilitySet, len(ecoreg.ProbeEntries))
	for _, e := range ecoreg.ProbeEntries {
		spec := Spec{
			RepoDir: repoDir,
			Cmd:     e.Probe,
			Image:   image,
			Timeout: probeTimeout,
			// Network defaults to "none" in the sandbox backend; no override needed.
		}
		result, err := sb.Exec(ctx, spec)
		if err != nil {
			// Infrastructure failure → all modes unavailable for this ecosystem.
			cs[e.Name] = allFalse(e)
			continue
		}
		// Convert sandbox.Result to ecoreg.ProbeResult (avoids import cycle).
		pr := ecoreg.ProbeResult{
			ExitCode: result.ExitCode,
			Stdout:   result.Stdout,
		}
		modes := e.Interpret(pr)
		if modes == nil {
			modes = allFalse(e)
		}
		cs[e.Name] = modes
	}
	return cs
}

// allFalse returns a modes map with every mode for e set to false.
// Used when a probe fails at the infrastructure level.
func allFalse(e ecoreg.ProbeEntry) map[string]bool {
	// Build the false map by running Interpret on a synthetic zero-exit result
	// so the mode set stays consistent with the ecosystem's declared modes.
	modes := e.Interpret(ecoreg.ProbeResult{ExitCode: 1})
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
