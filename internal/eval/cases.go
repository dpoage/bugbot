package eval

import "github.com/dpoage/bugbot/internal/funnel"

// BuiltinCases returns the built-in scripted benchmark suite. Each case pairs a
// real Go fixture with the scripted finder/verifier flow that exercises a
// realistic detection path: finders report the seeded bug (and sometimes a
// decoy), and refuters confirm the real bug while killing the decoy.
//
// The suite deliberately spans the precision/recall surface:
//
//  1. nil-deref-seeded   — one real nil-deref, plus a decoy the refuters kill.
//  2. resource-leak-seeded — an unclosed file on an error path.
//  3. off-by-one-seeded  — a boundary-condition index bug.
//  4. clean-code         — the FALSE-POSITIVE CANARY: no seeded bug. A finder
//     confabulates a candidate; the refuters must kill it, leaving zero
//     findings. Any finding here fails the benchmark gate.
//  5. multi-bug          — two seeded bugs in one file; both must survive.
//  6. suppressed-finding — a real bug whose fingerprint is pre-suppressed;
//     expect zero findings (suppression memory).
//
// The lens names referenced below must match funnel.BuiltinLenses(); the
// scripted finder routes on the lens name embedded in the finder system prompt.
func BuiltinCases() []Case {
	return []Case{
		nilDerefCase(),
		resourceLeakCase(),
		offByOneCase(),
		cleanCodeCase(),
		multiBugCase(),
		suppressedCase(),
	}
}

const (
	lensNil      = "nil-safety/error-handling"
	lensResource = "resource-leaks"
	lensBoundary = "boundary-conditions"
)

// --- 1. nil-deref --------------------------------------------------------

const nilDerefSrc = `package fixture

// Config holds optional settings.
type Config struct {
	Name string
}

// Greeting dereferences cfg without a nil check; a caller passing nil panics.
func Greeting(cfg *Config) string {
	return "hello " + cfg.Name
}
`

func nilDerefCase() Case {
	const (
		realTitle  = "nil deref of cfg in Greeting"
		decoyTitle = "Greeting allocates unnecessarily"
	)
	return NewScriptedCase(
		"nil-deref-seeded",
		FixtureSpec{Files: map[string]string{"greet.go": nilDerefSrc}},
		[]SeededBug{
			{File: "greet.go", Line: 10, LineTolerance: 2, Kind: "nil-deref"},
		},
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensNil, Candidates(
					CandidateJSON{
						File: "greet.go", Line: 10, Title: realTitle,
						Description: "cfg may be nil; dereferenced without a guard",
						Severity:    "high",
						Evidence:    "Greeting returns cfg.Name with no nil check",
						Confidence:  "high",
					},
					CandidateJSON{
						File: "greet.go", Line: 9, Title: decoyTitle,
						Description: "string concat allocates",
						Severity:    "low",
						Evidence:    "string concatenation",
						Confidence:  "high",
					},
				))
			},
			Verifier: func(c *ScriptedClient) {
				c.OnTaskContains(realTitle, NotRefutedJSON)
				c.OnTaskContains(decoyTitle, RefutedJSON)
			},
		},
		funnel.Options{},
		nil,
	)
}

// --- 2. resource leak ----------------------------------------------------

const resourceLeakSrc = `package fixture

import (
	"io"
	"os"
)

// ReadFirst opens path and returns its first n bytes. On the short-read error
// path it returns without closing f: a leaked file descriptor.
func ReadFirst(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	f.Close()
	return buf, nil
}
`

func resourceLeakCase() Case {
	const title = "leaked file on short-read error path in ReadFirst"
	return NewScriptedCase(
		"resource-leak-seeded",
		FixtureSpec{Files: map[string]string{"read.go": resourceLeakSrc}},
		[]SeededBug{
			{File: "read.go", Line: 16, LineTolerance: 3, Kind: "resource-leak"},
		},
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensResource, Candidates(CandidateJSON{
					File: "read.go", Line: 16, Title: title,
					Description: "f is not closed before the ReadFull error return",
					Severity:    "high",
					Evidence:    "early return at the io.ReadFull error check skips f.Close()",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				c.OnTaskContains(title, NotRefutedJSON)
			},
		},
		funnel.Options{},
		nil,
	)
}

// --- 3. off-by-one -------------------------------------------------------

const offByOneSrc = `package fixture

// Last returns the last element of xs. It indexes xs[len(xs)] instead of
// xs[len(xs)-1]: an out-of-range panic on every non-empty slice.
func Last(xs []int) int {
	return xs[len(xs)]
}
`

func offByOneCase() Case {
	const title = "off-by-one index in Last (xs[len(xs)])"
	return NewScriptedCase(
		"off-by-one-seeded",
		FixtureSpec{Files: map[string]string{"last.go": offByOneSrc}},
		[]SeededBug{
			{File: "last.go", Line: 6, LineTolerance: 2, Kind: "off-by-one"},
		},
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensBoundary, Candidates(CandidateJSON{
					File: "last.go", Line: 6, Title: title,
					Description: "indexing xs[len(xs)] is always out of range",
					Severity:    "high",
					Evidence:    "return xs[len(xs)] should be xs[len(xs)-1]",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				c.OnTaskContains(title, NotRefutedJSON)
			},
		},
		funnel.Options{},
		nil,
	)
}

// --- 4. clean code (FP canary) -------------------------------------------

const cleanSrc = `package fixture

// Add returns the sum of a and b. There is no bug here; this file is the
// false-positive canary for the eval suite.
func Add(a, b int) int {
	return a + b
}

// Max returns the larger of a and b.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`

func cleanCodeCase() Case {
	// A finder confabulates a plausible-sounding overflow bug in clean code; the
	// pipeline must kill it. This exercises the precision machinery: the
	// adversarial refuters are the line of defense, and the gate fails if any
	// finding survives.
	const decoy = "Add overflows on large inputs"
	return NewScriptedCase(
		"clean-code",
		FixtureSpec{Files: map[string]string{"math.go": cleanSrc}},
		nil, // clean: ANY finding is a false positive
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensBoundary, Candidates(CandidateJSON{
					File: "math.go", Line: 6, Title: decoy,
					Description: "a + b may overflow",
					Severity:    "low",
					Evidence:    "unchecked addition",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				// Every refuter refutes the confabulation.
				c.OnTaskContains(decoy, RefutedJSON)
			},
		},
		funnel.Options{},
		nil,
	)
}

// --- 5. multi-bug --------------------------------------------------------

const multiBugSrc = `package fixture

// Node is a singly-linked list node.
type Node struct {
	Next *Node
	Val  int
}

// Sum walks the list summing values. It dereferences n.Next.Val without
// checking n.Next for nil: panics at the tail.
func Sum(n *Node) int {
	total := 0
	for n != nil {
		total += n.Next.Val
		n = n.Next
	}
	return total
}

// At returns the i-th value. It uses <= so the final iteration dereferences a
// nil Next: an off-by-one that panics for i == length.
func At(n *Node, i int) int {
	for k := 0; k <= i; k++ {
		n = n.Next
	}
	return n.Val
}
`

func multiBugCase() Case {
	const (
		bug1 = "nil deref of n.Next in Sum"
		bug2 = "off-by-one loop bound in At"
	)
	return NewScriptedCase(
		"multi-bug",
		FixtureSpec{Files: map[string]string{"list.go": multiBugSrc}},
		[]SeededBug{
			{File: "list.go", Line: 14, LineTolerance: 2, Kind: "nil-deref"},
			{File: "list.go", Line: 23, LineTolerance: 2, Kind: "off-by-one"},
		},
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensNil, Candidates(CandidateJSON{
					File: "list.go", Line: 14, Title: bug1,
					Description: "n.Next may be nil at the list tail",
					Severity:    "high",
					Evidence:    "loop adds n.Next.Val with no nil guard on Next",
					Confidence:  "high",
				}))
				c.OnSystemContains(lensBoundary, Candidates(CandidateJSON{
					File: "list.go", Line: 23, Title: bug2,
					Description: "k <= i walks one past the requested index",
					Severity:    "high",
					Evidence:    "loop bound <= i dereferences a nil Next for i == length",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				c.OnTaskContains(bug1, NotRefutedJSON)
				c.OnTaskContains(bug2, NotRefutedJSON)
			},
		},
		funnel.Options{},
		nil,
	)
}

// --- 6. suppressed finding -----------------------------------------------

func suppressedCase() Case {
	// Same fixture and finder flow as the nil-deref case, but the real bug's
	// fingerprint is pre-suppressed (a maintainer dismissed it). Triage must drop
	// it before verification, so the run reports ZERO findings. This is a clean
	// case from the scorer's perspective — no seeded bug to find — so any finding
	// is a false positive and fails the gate.
	const realTitle = "nil deref of cfg in Greeting"
	return NewScriptedCase(
		"suppressed-finding",
		FixtureSpec{Files: map[string]string{"greet.go": nilDerefSrc}},
		nil, // suppressed: expect zero findings, so treat as clean
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensNil, Candidates(CandidateJSON{
					File: "greet.go", Line: 10, Title: realTitle,
					Description: "cfg may be nil",
					Severity:    "high",
					Evidence:    "no nil check",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				// Should never be reached — the candidate is suppressed in triage.
				c.OnTaskContains(realTitle, NotRefutedJSON)
			},
		},
		funnel.Options{},
		[]Suppression{
			{
				Lens: lensNil, File: "greet.go", Line: 10, Title: realTitle,
				Reason: "eval: maintainer-dismissed known non-bug",
			},
		},
	)
}
