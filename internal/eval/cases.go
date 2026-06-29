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
//  7. split-arbiter-refutes — a split panel on CLEAN code; the agentic arbiter
//     grounds the dissent and REFUTES, killing the false positive. Also a
//     regression gate vs the pre-mi5.17 one-shot arbiter (bugbot-mi5.17).
//  8. split-arbiter-keeps — a split panel on a REAL bug; the arbiter grounds and
//     KEEPS it, so a true bug survives a 1-refute split.
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
		splitArbiterRefutesCase(),
		splitArbiterKeepsCase(),
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

// Greeting builds a greeting string from cfg.
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

// ReadFirst opens path and returns its first n bytes.
// The returned slice holds the file's first n bytes.
func ReadFirst(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, max(n, 0))
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

// Last returns the last element of xs.
// It walks the slice to find the trailing value.
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

// Add returns the sum of a and b.
// Inputs are added directly without further processing.
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

// Sum walks the list summing values.
// It accumulates the total during traversal.
func Sum(n *Node) int {
	total := 0
	for n != nil {
		total += n.Next.Val
		n = n.Next
	}
	return total
}

// At returns the i-th value.
// It advances the cursor until the index is reached.
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

// --- 7 & 8. split-verdict arbiter (bugbot-mi5.17) ------------------------

// splitFPSrc is clean code: lookupID dereferences dev, but every caller passes a
// freshly constructed (never-nil) Device, so the flagged nil-deref path is
// unreachable.
const splitFPSrc = `package fixture

// Device is always constructed non-nil by its callers.
type Device struct{ id int }

func (d *Device) ID() int { return d.id }

// lookupID returns the id of the given device. Its only caller, useLookup
// below, constructs the Device inline before calling it.
func lookupID(dev *Device) int {
	return dev.ID()
}

func useLookup() int {
	return lookupID(&Device{id: 7})
}
`

// splitArbiterRefutesCase is the false-positive canary for the split-verdict
// arbiter: a split panel on clean code where the arbiter must REFUTE. It is also
// a regression gate against the pre-mi5.17 one-shot arbiter — the scripted
// arbiter response carries the now-REQUIRED evidence field, which the old
// arbiterSchema rejects, falling back to majorityRefuted (a 1-1 tie that
// survives) and producing the false positive this case forbids.
func splitArbiterRefutesCase() Case {
	const title = "possible nil deref of dev in lookupID"
	return NewScriptedCase(
		"split-arbiter-refutes",
		FixtureSpec{Files: map[string]string{"device.go": splitFPSrc}},
		nil, // clean: ANY surviving finding is a false positive
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensNil, Candidates(CandidateJSON{
					File: "device.go", Line: 11, Title: title,
					Description: "dev is dereferenced without a nil guard",
					Severity:    "high",
					Evidence:    "lookupID returns dev.ID() with no nil check",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				// Split: reachability seat refutes, semantics seat cannot.
				c.OnSystemContains("(reachability)", RefutedJSON)
				c.OnSystemContains("(semantics)", NotRefutedJSON)
				// The arbiter grounds the caller set and refutes (evidence-backed).
				c.OnTaskContains("PANEL VERDICTS", RefutedArbiterJSON)
			},
		},
		funnel.Options{Limits: funnel.StageLimits{Refuters: 2}},
		nil,
	)
}

// splitRealSrc is a real, reachable nil deref: handleAnon calls resolve(nil), so
// the deref panics on the anonymous path.
const splitRealSrc = `package fixture

type Session struct{ token string }

func (s *Session) Token() string { return s.token }

// resolve returns the token associated with the session.
// It is the access path used by the other entry points.
func resolve(sess *Session) string {
	return sess.Token()
}

func handleAnon() string {
	return resolve(nil)
}
`

// splitArbiterKeepsCase pins the other half of the split-verdict design: a split
// panel on a REAL bug where the arbiter grounds the dissent and KEEPS the
// finding, so a true bug survives a 1-refute split that a single refutation
// would otherwise demote.
func splitArbiterKeepsCase() Case {
	const title = "nil deref of sess in resolve"
	return NewScriptedCase(
		"split-arbiter-keeps",
		FixtureSpec{Files: map[string]string{"session.go": splitRealSrc}},
		[]SeededBug{
			{File: "session.go", Line: 10, LineTolerance: 2, Kind: "nil-deref"},
		},
		&ScriptedCase{
			Finder: func(c *ScriptedClient) {
				c.OnSystemContains(lensNil, Candidates(CandidateJSON{
					File: "session.go", Line: 10, Title: title,
					Description: "sess may be nil; handleAnon passes nil",
					Severity:    "high",
					Evidence:    "resolve returns sess.Token() and handleAnon calls resolve(nil)",
					Confidence:  "high",
				}))
			},
			Verifier: func(c *ScriptedClient) {
				// Split: reachability seat refutes (wrongly), semantics seat keeps.
				c.OnSystemContains("(reachability)", RefutedJSON)
				c.OnSystemContains("(semantics)", NotRefutedJSON)
				// The arbiter grounds the nil caller and KEEPS the bug.
				c.OnTaskContains("PANEL VERDICTS", NotRefutedArbiterJSON)
			},
		},
		funnel.Options{Limits: funnel.StageLimits{Refuters: 2}},
		nil,
	)
}
