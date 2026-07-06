package ecosystem

// ecosystem.go defines the Ecosystem type and its constants. This type was
// previously in internal/sandbox/ecosystem.go; moving it here breaks the
// import cycle that would arise from internal/ecosystem importing sandbox for
// the type, while sandbox also imports ecosystem for probe data.
//
// internal/sandbox/ecosystem.go now re-exports Ecosystem as a type alias so
// all existing callers (repro, funnel, agent, ...) that reference sandbox.Ecosystem
// continue to compile without modification.

// Ecosystem identifies the build/test ecosystem of a repository. It is used
// across packages to classify sandbox results without bare string comparisons.
//
// Naming convention: lowercase, single word.
type Ecosystem = string

const (
	// EcosystemGo is the Go ecosystem (go test, go build).
	EcosystemGo Ecosystem = "go"
	// EcosystemPython is the Python ecosystem (pytest, python -m pytest).
	EcosystemPython Ecosystem = "python"
	// EcosystemRust is the Rust/Cargo ecosystem (cargo test, cargo build).
	EcosystemRust Ecosystem = "rust"
	// EcosystemJS is the JavaScript/npm ecosystem (npm test, jest, vitest).
	EcosystemJS Ecosystem = "js"
	// EcosystemCpp is the C/C++ ecosystem (ctest, cmake, make).
	EcosystemCpp Ecosystem = "cpp"
	// EcosystemBazel is the Bazel build/test ecosystem.
	EcosystemBazel Ecosystem = "bazel"
	// EcosystemUnknown is the fallback for unrecognized launchers.
	EcosystemUnknown Ecosystem = "unknown"
)
