package repro

// target_exec.go implements the static "executable edge" gate: layer (a) of
// bugbot-qb4r. It runs over the submitted plan BEFORE any sandbox execution
// and answers a narrower question than interpret()'s execution-witness layer
// (b, see witnessDemonstration in interpret.go): does ANY submitted test file
// reach the finding's target file through an executable edge (import,
// require, #include, use, or same-package colocation), as opposed to merely
// opening it as data — reading its source text to grep or lint it?
//
// This is the layer that catches transliterations (a test that executes a
// re-implementation of the buggy logic, never touching the target file at
// all): the runtime alone cannot tell a transliteration from a genuine
// behavioral test — both run, both fail, both print ran-evidence. Only a
// static look at what the test file actually references can tell them apart.
//
// Pure function, no sandbox/store/LLM dependency: internal/repro/bundle.go's
// corpus replay (bugbot-ecm8) calls it directly against saved bundle
// fixtures, and it is exercised here with nothing but literal strings.
import (
	"fmt"
	"path"
	"regexp"
	"strings"

	eco "github.com/dpoage/bugbot/internal/ecosystem"
)

// ClassifyTargetExecution reports whether testFiles (workspace-relative path
// -> file contents, i.e. a Plan.Files map) contains at least one executable
// edge to targetPath (the finding's target file, workspace-relative) for the
// given ecosystem.
//
// Returns ("", "") when at least one test file reaches the target through an
// executable edge — no static objection, the plan may proceed to the
// sandbox. Returns (VerdictReasonTargetNotExecuted, detail) when none does;
// detail is a short human-readable explanation naming the missing edge,
// suitable for verdict.feedback / an agent-facing message.
//
// Ecosystems this function has no edge-detection rule for (bazel, unknown,
// or any ecosystem missing from executableEdgeCheckers) are treated
// permissively — this is a precision-first filter for the observed failure
// mode (an agent pivoting to a DIFFERENT ecosystem's test files because the
// target's own toolchain is unavailable in the sandbox), not a universal
// static-analysis lint. A missing rule never blocks a plan.
func ClassifyTargetExecution(testFiles map[string]string, targetPath string, ecoName eco.Ecosystem) (VerdictReason, string) {
	if targetPath == "" || len(testFiles) == 0 {
		return "", ""
	}
	check, ok := executableEdgeCheckers[ecoName]
	if !ok {
		return "", ""
	}
	for testPath, content := range testFiles {
		if check(testPath, content, targetPath) {
			return "", ""
		}
	}
	base := path.Base(targetPath)
	detail := fmt.Sprintf(
		"none of the submitted test files import/require/include %q (or link against its module/package) as "+
			"executable code; opening it only to read its text (grep or lint checks) is not a behavioral reproduction",
		base)
	switch ecoName {
	case eco.EcosystemGo:
		detail += ". For Go, the simplest executable edge is COLOCATION: put your _test.go in the SAME DIRECTORY " +
			"as the target, declared in the same package — for a `package main` target this is the ONLY option, " +
			"because a main package cannot be imported"
	case eco.EcosystemPython:
		detail += ". For Python, import the target module (e.g. `from <pkg>.<module> import ...`) and CALL the " +
			"buggy function; if the package roots at a subdirectory of the repo, run pytest from that " +
			"subdirectory (relative cd) or set PYTHONPATH to it"
	}
	return VerdictReasonTargetNotExecuted, detail
}

// targetGateEcosystem returns the ecosystem whose executable-edge rule the
// static gate (ClassifyTargetExecution) should apply, given the PLAN CMD's
// detected ecosystem and the finding's target file. Launcher-based
// ecosystems (bazel) and unrecognized commands carry no edge rule of their
// own — historically that made the gate permissive for them, which was
// harmless while bazel demonstrations could only ever reach witness-only.
// Now that a bazel run whose declared test fails can fully promote
// (witnessDemonstration's declaredFailureWitness branch, bugbot-9fac), the
// gate must still hold the TARGET FILE's own language edge: a bazel-run Go
// repro is required to reach the target through a Go edge (same-package
// colocation or an import of its package) exactly like a `go test` repro.
// Falls back to the cmd ecosystem when the target's extension maps to no
// known ecosystem, preserving the permissive behavior for files we have no
// rule for.
func targetGateEcosystem(cmdEco eco.Ecosystem, targetFile string) eco.Ecosystem {
	if cmdEco != eco.EcosystemBazel && cmdEco != eco.EcosystemUnknown {
		return cmdEco
	}
	if byExt := eco.InferFromExtension(targetFile); byExt != "" {
		return byExt
	}
	return cmdEco
}

// edgeChecker reports whether a single test file (its workspace-relative
// path and contents) reaches targetPath through an executable edge for one
// ecosystem. testPath is used only by ecosystems where same-directory
// colocation is itself an executable edge (Go, Rust, C++ all compile every
// file in a package/target together).
type edgeChecker func(testPath, content, targetPath string) bool

// executableEdgeCheckers is the per-ecosystem registry of edge-detection
// rules, keyed the same way as ecosystem.InterpTable / ecosystem.WitnessTable
// so all three "per-ecosystem behavior" tables stay easy to cross-reference.
var cppSourceExtensions = []string{".cc", ".cpp", ".cxx", ".C", ".c", ".h", ".hpp", ".hh"}

// hasAnySuffix reports whether s ends with any of suffixes.
func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

var executableEdgeCheckers = map[eco.Ecosystem]edgeChecker{
	eco.EcosystemPython: func(_, content, targetPath string) bool {
		return pythonReachesTarget(content, targetPath)
	},
	eco.EcosystemJS: func(_, content, targetPath string) bool {
		return jsReachesTarget(content, targetPath)
	},
	eco.EcosystemGo: func(testPath, content, targetPath string) bool {
		if strings.HasSuffix(testPath, ".go") && path.Dir(testPath) == path.Dir(targetPath) {
			return true
		}
		return goImportReachesTarget(content, targetPath)
	},
	eco.EcosystemRust: func(testPath, content, targetPath string) bool {
		if strings.HasSuffix(testPath, ".rs") && (testPath == targetPath || path.Dir(testPath) == path.Dir(targetPath)) {
			return true
		}
		return basenameReferencedAfter(content, targetPath, []string{"use", "mod"})
	},
	eco.EcosystemCpp: func(testPath, content, targetPath string) bool {
		if hasAnySuffix(testPath, cppSourceExtensions) && path.Dir(testPath) == path.Dir(targetPath) {
			return true
		}
		return cppIncludesTarget(content, targetPath)
	},
}

// pythonModuleCandidates derives the dotted-module names a Python import
// statement could plausibly use to reach targetPath: every path-segment
// suffix of the dotted module path, in both its literal form and with
// hyphens normalized to underscores (Python identifiers cannot contain
// hyphens, but repository directories often do).
func pythonModuleCandidates(targetPath string) []string {
	trimmed := strings.TrimSuffix(targetPath, path.Ext(targetPath))
	parts := strings.Split(trimmed, "/")
	cands := make([]string, 0, len(parts)*2)
	for i := range parts {
		dotted := strings.Join(parts[i:], ".")
		cands = append(cands, dotted)
		if underscored := strings.ReplaceAll(dotted, "-", "_"); underscored != dotted {
			cands = append(cands, underscored)
		}
	}
	return cands
}

// pythonReachesTarget reports whether content contains a Python import
// statement that plausibly resolves to targetPath: `import <module>`,
// `from <module> import ...`, a relative `from .+<module> import`, or the
// dynamic-import builtins. Deliberately does NOT match the target's path
// appearing inside a string literal passed to open()/read_text() — that is
// exactly the "opened as data" pattern this gate exists to reject.
func pythonReachesTarget(content, targetPath string) bool {
	for _, cand := range pythonModuleCandidates(targetPath) {
		q := regexp.QuoteMeta(cand)
		patterns := []string{
			`(?m)^\s*import\s+` + q + `\b`,
			`(?m)^\s*from\s+` + q + `\s+import\b`,
			`(?m)^\s*from\s+\.+` + q + `\s+import\b`,
			`import_module\(\s*['"]` + q + `['"]`,
			`__import__\(\s*['"]` + q + `['"]`,
		}
		for _, p := range patterns {
			if regexp.MustCompile(p).MatchString(content) {
				return true
			}
		}
	}
	// Package-relative "from . import <base>" / "from .. import <base>":
	// a relative import carries no package prefix, only the last segment.
	base := strings.TrimSuffix(path.Base(targetPath), path.Ext(targetPath))
	relPattern := `(?m)^\s*from\s+\.+\s+import\s+.*\b` + regexp.QuoteMeta(base) + `\b`
	return regexp.MustCompile(relPattern).MatchString(content)
}

// jsImportSpecifier extracts the quoted module specifier from an ES `import
// ... from '<spec>'`, a dynamic `import('<spec>')`, or a CommonJS
// `require('<spec>')`.
var jsImportSpecifier = regexp.MustCompile(`(?:import\s[^'"]*from\s*|import\s*\(\s*|require\(\s*)['"]([^'"]+)['"]`)

// jsReachesTarget reports whether content contains an import/require whose
// specifier's basename (extension stripped) matches the target file's
// basename. Matching on the basename rather than the full path tolerates the
// arbitrary relative-path depth ("./x", "../../x") a test file may use
// without needing to resolve it against a real workspace.
func jsReachesTarget(content, targetPath string) bool {
	base := strings.TrimSuffix(path.Base(targetPath), path.Ext(targetPath))
	for _, m := range jsImportSpecifier.FindAllStringSubmatch(content, -1) {
		spec := m[1]
		specBase := strings.TrimSuffix(path.Base(spec), path.Ext(spec))
		if specBase == base {
			return true
		}
	}
	return false
}

// goImportReachesTarget reports whether content contains a Go import whose
// path ends with the target file's directory — the external-test-package
// case (package foo_test importing ".../foo") that same-directory
// colocation does not cover.
func goImportReachesTarget(content, targetPath string) bool {
	dir := path.Dir(targetPath)
	if dir == "." || dir == "" {
		return false
	}
	base := path.Base(dir)
	pattern := `(?m)^\s*(?:\w+\s+)?"[^"]*` + regexp.QuoteMeta(base) + `"\s*$`
	return regexp.MustCompile(pattern).MatchString(content)
}

// basenameReferencedAfter reports whether content has a line starting with
// one of keywords (e.g. Rust's "use"/"mod") that also mentions the target
// file's basename (extension stripped).
func basenameReferencedAfter(content, targetPath string, keywords []string) bool {
	base := strings.TrimSuffix(path.Base(targetPath), path.Ext(targetPath))
	baseQ := regexp.QuoteMeta(base)
	for _, kw := range keywords {
		pattern := `(?m)^\s*` + regexp.QuoteMeta(kw) + `\s+.*\b` + baseQ + `\b`
		if regexp.MustCompile(pattern).MatchString(content) {
			return true
		}
	}
	return false
}

// cppIncludesTarget reports whether content #includes the target's header
// (same basename, .h/.hpp/.hh) or names the target's basename in a build
// directive referencing it as a translation unit.
func cppIncludesTarget(content, targetPath string) bool {
	base := strings.TrimSuffix(path.Base(targetPath), path.Ext(targetPath))
	baseQ := regexp.QuoteMeta(base)
	pattern := `#include\s*["<]` + baseQ + `\.[a-zA-Z+]+[">]`
	return regexp.MustCompile(pattern).MatchString(content)
}
