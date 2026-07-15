package sandbox

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeResolver builds lookPath/evalSymlinks funcs from a name -> resolved
// final path map: lookPath returns a shim path ("/shim/<name>") for known
// names, and evalSymlinks maps that shim to the configured final path —
// mimicking a PATH hit whose symlink closure lands in the mapped directory.
func fakeResolver(resolved map[string]string) (func(string) (string, error), func(string) (string, error)) {
	lookPath := func(name string) (string, error) {
		if _, ok := resolved[name]; ok {
			return "/shim/" + name, nil
		}
		return "", errors.New("not found")
	}
	evalSymlinks := func(p string) (string, error) {
		name := strings.TrimPrefix(p, "/shim/")
		if final, ok := resolved[name]; ok {
			return final, nil
		}
		return "", errors.New("dangling")
	}
	return lookPath, evalSymlinks
}

// TestFilterBwrapBaseline_FHSNoOp pins the FHS no-op guarantee: utilities
// whose symlink-resolved home is a DefaultContainerPath directory are
// dropped, so a standard distro resolves an EMPTY baseline and the sandbox
// argv stays byte-identical to the pre-baseline behavior.
func TestFilterBwrapBaseline_FHSNoOp(t *testing.T) {
	lookPath, evalSymlinks := fakeResolver(map[string]string{
		"mkdir": "/usr/bin/mkdir",
		"grep":  "/usr/bin/grep",
		"sed":   "/bin/sed",
		"bash":  "/usr/bin/bash",
	})
	got := filterBwrapBaseline([]string{"mkdir", "grep", "sed", "awk", "bash"}, lookPath, evalSymlinks)
	if got != nil {
		t.Errorf("FHS layout must filter to an empty baseline; got %q", got)
	}
}

// TestFilterBwrapBaseline_StoreLayout pins the store-distro behavior: names
// resolving outside DefaultContainerPath survive, names sharing one package
// directory collapse to the first (nix coreutils applets, findutils'
// find+xargs), and unresolvable names are skipped.
func TestFilterBwrapBaseline_StoreLayout(t *testing.T) {
	lookPath, evalSymlinks := fakeResolver(map[string]string{
		// coreutils multi-call: mkdir would collapse with cp/rm — here only
		// mkdir is requested from that dir.
		"mkdir": "/nix/store/aaa-coreutils-9.8/bin/coreutils",
		"grep":  "/nix/store/bbb-gnugrep-3.11/bin/grep",
		// find and xargs share the findutils package dir: only find kept.
		"find":  "/nix/store/ccc-findutils-4.9/bin/find",
		"xargs": "/nix/store/ccc-findutils-4.9/bin/xargs",
		// awk missing from the host entirely (not in the map).
	})
	got := filterBwrapBaseline([]string{"mkdir", "grep", "awk", "find", "xargs"}, lookPath, evalSymlinks)
	want := []string{"mkdir", "grep", "find"}
	if !slices.Equal(got, want) {
		t.Errorf("filterBwrapBaseline = %q, want %q", got, want)
	}
}

// TestAppendBaselinePath pins the PATH suffix semantics: empty baseline is a
// strict no-op, the baseline is appended exactly once, and a value already
// carrying the suffix is left alone (repeated Spec.Env PATH entries).
func TestAppendBaselinePath(t *testing.T) {
	cases := []struct {
		name, path, baseline, want string
	}{
		{"empty baseline no-op", "/usr/bin:/bin", "", "/usr/bin:/bin"},
		{"appended once", "/usr/bin:/bin", "/opt/bugbot-toolchains/mkdir/bin", "/usr/bin:/bin:/opt/bugbot-toolchains/mkdir/bin"},
		{"already suffixed", "/usr/bin:/opt/bugbot-toolchains/mkdir/bin", "/opt/bugbot-toolchains/mkdir/bin", "/usr/bin:/opt/bugbot-toolchains/mkdir/bin"},
		{"empty path takes baseline", "", "/opt/bugbot-toolchains/mkdir/bin", "/opt/bugbot-toolchains/mkdir/bin"},
		{"exact match unchanged", "/opt/bugbot-toolchains/mkdir/bin", "/opt/bugbot-toolchains/mkdir/bin", "/opt/bugbot-toolchains/mkdir/bin"},
	}
	for _, tc := range cases {
		if got := appendBaselinePath(tc.path, tc.baseline); got != tc.want {
			t.Errorf("%s: appendBaselinePath(%q, %q) = %q, want %q", tc.name, tc.path, tc.baseline, got, tc.want)
		}
	}
}

// TestBuildBwrapArgsBaselinePath pins the composition order in the rendered
// argv — [prepend:]DefaultContainerPath[:baseline] — and that a
// caller-supplied PATH in env gets the baseline re-appended so utilities
// survive engine-constructed probe PATHs (bugbot-qg8b acceptance 3).
func TestBuildBwrapArgsBaselinePath(t *testing.T) {
	base := "/opt/bugbot-toolchains/mkdir/bin:/opt/bugbot-toolchains/grep/bin"
	args := buildBwrapArgs(bwrapParams{
		workspace:            "/tmp/ws",
		cmd:                  []string{"true"},
		toolchainPathPrepend: "/opt/bugbot-toolchains/node/bin",
		baselinePathAppend:   base,
		env:                  []string{"PATH=/probe/bin:" + DefaultContainerPath},
	})
	wantDefault := "/opt/bugbot-toolchains/node/bin:" + DefaultContainerPath + ":" + base
	mustContainSeq(t, args, "--setenv", "PATH", wantDefault)
	wantEnv := "/probe/bin:" + DefaultContainerPath + ":" + base
	mustContainSeq(t, args, "--setenv", "PATH", wantEnv)

	// Without a baseline the argv is byte-identical to the historical shape:
	// no trailing suffix on either PATH.
	plain := buildBwrapArgs(bwrapParams{
		workspace: "/tmp/ws",
		cmd:       []string{"true"},
		env:       []string{"PATH=/probe/bin"},
	})
	mustContainSeq(t, plain, "--setenv", "PATH", DefaultContainerPath)
	mustContainSeq(t, plain, "--setenv", "PATH", "/probe/bin")
}
