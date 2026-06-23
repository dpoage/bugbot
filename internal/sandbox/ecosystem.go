package sandbox

// Ecosystem identifies the build/test ecosystem of a repository. It is used
// across the sandbox and repro packages to classify sandbox results without
// bare string comparisons.
//
// Naming is aligned with sandbox.DepStrategy and ingest.Language conventions:
// lowercase, single word, exported constants.
type Ecosystem string

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
	// Bazel reproduction IS supported when the sandbox image carries bazel plus
	// vendored deps and a warm cache (offline). repro.interpret() classifies a
	// bazel run by its exit code: 3 (build OK, tests ran, >=1 FAILED) demonstrates.
	EcosystemBazel Ecosystem = "bazel"
	// EcosystemUnknown is the fallback for unrecognized launchers. It still
	// requires positive ran-evidence; a bare non-zero exit never demonstrates.
	EcosystemUnknown Ecosystem = "unknown"
)
