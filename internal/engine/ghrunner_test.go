package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeGH is a test GHRunner that routes on the joined args and records every
// invocation, so tests can assert the exact gh api calls (and their bodies)
// review mode makes without touching the network.
type fakeGH struct {
	// routes maps a substring matched against the joined args to a canned
	// response. The first matching route wins (insertion order via keys slice).
	keys      []string
	responses map[string][]byte
	errs      map[string]error
	// calls records every invocation's args in order.
	calls [][]string
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

// run is the GHRunner the code under test calls.
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

// flagValue extracts the argument following a literal flag from a recorded
// call, e.g. flagValue(call, "-X") returns the HTTP method.
func flagValue(call []string, flag string) (string, bool) {
	for i := 0; i < len(call)-1; i++ {
		if call[i] == flag {
			return call[i+1], true
		}
	}
	return "", false
}

func prPayload(baseSHA, headSHA, headRef string, number int) []byte {
	return []byte(fmt.Sprintf(
		`{"number":%d,"base":{"sha":%q},"head":{"sha":%q,"ref":%q}}`,
		number, baseSHA, headSHA, headRef))
}

// TestResolvePR_Parse confirms base/head/ref are parsed from the gh payload.
func TestResolvePR_Parse(t *testing.T) {
	gh := newFakeGH().on("pulls/7", prPayload("baseSHA", "headSHA", "feature", 7))
	pr, err := resolvePR(context.Background(), gh.run, 7)
	if err != nil {
		t.Fatalf("resolvePR: %v", err)
	}
	if pr.BaseSHA != "baseSHA" || pr.HeadSHA != "headSHA" || pr.HeadRef != "feature" {
		t.Errorf("parsed PR = %+v", pr)
	}
	// gh must be called with the auto-filled owner/repo placeholder.
	if len(gh.callsContaining("repos/{owner}/{repo}/pulls/7")) != 1 {
		t.Errorf("expected one gh api call to the templated pulls endpoint; calls=%v", gh.calls)
	}
}

// TestResolvePR_MissingSHA errors when the payload lacks SHAs.
func TestResolvePR_MissingSHA(t *testing.T) {
	gh := newFakeGH().on("pulls/7", []byte(`{"number":7,"base":{},"head":{}}`))
	if _, err := resolvePR(context.Background(), gh.run, 7); err == nil {
		t.Fatal("expected error for missing SHAs")
	}
}

// TestScrubNUL verifies the exec-boundary guard: NUL bytes are removed from
// every argument (a NUL makes forkExec fail with EINVAL), the input slice is
// never mutated in place, and a clean slice is returned unchanged with the same
// backing array (no allocation on the common path).
func TestScrubNUL(t *testing.T) {
	clean := []string{"api", "repos/{owner}/{repo}/issues", "-f", "body=hello world"}
	got := scrubNUL(clean)
	if len(got) == 0 || &got[0] != &clean[0] {
		t.Errorf("scrubNUL reallocated a clean slice; want same backing array")
	}
	for i := range clean {
		if got[i] != clean[i] {
			t.Errorf("clean arg %d mutated: got %q want %q", i, got[i], clean[i])
		}
	}

	dirty := []string{"-f", "body=ab\x00cd", "-f", "title=x\x00"}
	out := scrubNUL(dirty)
	if out[1] != "body=abcd" {
		t.Errorf("body NUL not stripped: got %q", out[1])
	}
	if out[3] != "title=x" {
		t.Errorf("title NUL not stripped: got %q", out[3])
	}
	if dirty[1] != "body=ab\x00cd" {
		t.Errorf("scrubNUL mutated input slice element: %q", dirty[1])
	}
	for i, a := range out {
		if strings.IndexByte(a, 0) >= 0 {
			t.Errorf("arg %d still contains NUL: %q", i, a)
		}
	}
}
