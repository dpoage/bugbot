package ecosystem

// ecosystem.go defines the Ecosystem type and its constants.

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
