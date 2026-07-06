package sandbox

// ecosystem.go is the CANONICAL RE-EXPORT BOUNDARY for the Ecosystem type.
//
// internal/ecosystem is the authoritative definition; the type was moved there
// to break the import cycle: internal/ecosystem needs to use Ecosystem in its
// interp.go and capabilities.go types, while sandbox/capabilities.go imports
// internal/ecosystem for probe data. Moving the type to internal/ecosystem
// makes the DAG acyclic (sandbox → ecosystem, never the reverse).
//
// This file re-exports Ecosystem via a type alias and const aliases so all
// existing callers (repro, funnel, agent, sandbox tests) that reference
// sandbox.Ecosystem / sandbox.EcosystemGo / etc. compile unchanged.
//
// This is a deliberate, permanent public surface — NOT a deprecation shim.
// The sandbox package is the entry point for sandbox-execution callers;
// exposing Ecosystem here keeps its API self-contained. Callers that only need
// the type (e.g. internal/ecosystem itself) import internal/ecosystem directly.

import ecoreg "github.com/dpoage/bugbot/internal/ecosystem"

// Ecosystem identifies the build/test ecosystem of a repository.
// Re-exported from internal/ecosystem; see that package for the canonical definition.
type Ecosystem = ecoreg.Ecosystem

const (
	EcosystemGo      = ecoreg.EcosystemGo
	EcosystemPython  = ecoreg.EcosystemPython
	EcosystemRust    = ecoreg.EcosystemRust
	EcosystemJS      = ecoreg.EcosystemJS
	EcosystemCpp     = ecoreg.EcosystemCpp
	EcosystemBazel   = ecoreg.EcosystemBazel
	EcosystemUnknown = ecoreg.EcosystemUnknown
)
