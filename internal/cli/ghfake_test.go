package cli

import (
	"context"
	"fmt"
	"strings"
)

// fakeGH is a test engine.GHRunner that routes on the joined args and records
// every invocation, so publish_test.go can assert the exact gh api calls (and
// their bodies) publish mode makes without touching the network.
//
// This is a deliberate duplicate of internal/engine's own fakeGH (used by its
// resolvePR/prcomments/review tests): both packages need the same tiny fake
// GH double, but it is test-only and gh/review orchestration itself lives in
// internal/engine now (bugbot-2p8z.5) — sharing a test helper across package
// boundaries would mean exporting a fake solely for tests, which is worse
// than the ~30 lines of duplication.
type fakeGH struct {
	keys      []string
	responses map[string][]byte
	errs      map[string]error
	calls     [][]string
}

func newFakeGH() *fakeGH {
	return &fakeGH{responses: map[string][]byte{}, errs: map[string]error{}}
}

// on registers a canned JSON response for invocations whose joined args contain
// substr. Routes are checked in registration order.
func (f *fakeGH) on(substr string, resp []byte) *fakeGH {
	f.keys = append(f.keys, substr)
	f.responses[substr] = resp
	return f
}

// run is the engine.GHRunner the code under test calls.
func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	for _, k := range f.keys {
		if strings.Contains(joined, k) {
			if err, ok := f.errs[k]; ok {
				return nil, err
			}
			return f.responses[k], nil
		}
	}
	return nil, fmt.Errorf("fakeGH: no route for: %s", joined)
}

// callsContaining returns every recorded call whose joined args contain substr.
func (f *fakeGH) callsContaining(substr string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			out = append(out, c)
		}
	}
	return out
}

// argValue extracts the value of an -f/-F key=value pair from a recorded call,
// e.g. argValue(call, "body") returns the body posted.
func argValue(call []string, key string) (string, bool) {
	for i := 0; i < len(call); i++ {
		if (call[i] == "-f" || call[i] == "-F") && i+1 < len(call) {
			if k, v, ok := strings.Cut(call[i+1], "="); ok && k == key {
				return v, true
			}
		}
	}
	return "", false
}
