package sandbox

import (
	"context"
	"testing"
)

// TestResolveNonRootUserSkipsNonPodman: the mechanism has no Docker
// equivalent, so resolveNonRootUser must never probe (and must return the
// no-override zero value) for any runtime other than exactly "podman" —
// including the empty/unset case.
func TestResolveNonRootUserSkipsNonPodman(t *testing.T) {
	for _, rt := range []string{"docker", "", "bogus"} {
		uid, gid := resolveNonRootUser(context.Background(), rt, "some-image:latest")
		if uid != 0 || gid != 0 {
			t.Errorf("runtime %q: resolveNonRootUser = (%d,%d), want (0,0) — no probe for non-podman runtimes", rt, uid, gid)
		}
	}
}

// TestResolveNonRootUserEmptyImage: an empty image string must short-circuit
// without attempting a probe (there is nothing to run).
func TestResolveNonRootUserEmptyImage(t *testing.T) {
	uid, gid := resolveNonRootUser(context.Background(), "podman", "")
	if uid != 0 || gid != 0 {
		t.Errorf("resolveNonRootUser with empty image = (%d,%d), want (0,0)", uid, gid)
	}
}

// TestProbeContainerIdentityParsesTwoLines pins the probe's output-parsing
// contract directly (without needing a real container runtime): exactly two
// whitespace-separated non-negative integers, in "uid\ngid\n" order, from
// the "id -u; id -g" probe command. Malformed output — including the
// non-numeric error text a failed `podman run` would print to stdout, or a
// single line, or extra lines — must resolve to ok=false, never a partial
// or misparsed identity.
func TestProbeContainerIdentityParsesTwoLines(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		wantOK  bool
		wantUID int
		wantGID int
	}{
		{"well_formed", "1500\n1500\n", true, 1500, 1500},
		{"different_uid_gid", "1000\n100\n", true, 1000, 100},
		{"root", "0\n0\n", true, 0, 0},
		{"single_line", "1500\n", false, 0, 0},
		{"three_lines", "1500\n1500\nextra\n", false, 0, 0},
		{"non_numeric", "no such image\n", false, 0, 0},
		{"empty", "", false, 0, 0},
		{"negative", "-1\n-1\n", false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := parseIdentityProbeOutput(tc.out)
			if id.ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (out=%q)", id.ok, tc.wantOK, tc.out)
			}
			if tc.wantOK && (id.uid != tc.wantUID || id.gid != tc.wantGID) {
				t.Errorf("(uid,gid) = (%d,%d), want (%d,%d)", id.uid, id.gid, tc.wantUID, tc.wantGID)
			}
		})
	}
}

// TestInvalidateNonRootUserCacheScopedToImage pins InvalidateNonRootUserCache's
// contract directly (no container runtime needed): it must remove exactly the
// cache entries for the named image, leaving other images' cached entries
// untouched. The end-to-end "does a real Exec actually populate the cache"
// claim is covered by the integration test
// TestIntegrationNonRootUserIdentityCaching, which white-box-inspects
// nonRootUserCache after a real Exec against a non-root-USER image rather
// than a timing measurement (container-launch timing is too noisy on a
// shared host to assert on reliably).
func TestInvalidateNonRootUserCacheScopedToImage(t *testing.T) {
	nonRootUserCache.Store("podman|image-a:latest", containerIdentity{uid: 1500, gid: 1500, ok: true})
	nonRootUserCache.Store("docker|image-a:latest", containerIdentity{uid: 1500, gid: 1500, ok: true})
	nonRootUserCache.Store("podman|image-b:latest", containerIdentity{uid: 1000, gid: 1000, ok: true})

	InvalidateNonRootUserCache("image-a:latest")

	if _, hit := nonRootUserCache.Load("podman|image-a:latest"); hit {
		t.Error("podman|image-a:latest should have been invalidated")
	}
	if _, hit := nonRootUserCache.Load("docker|image-a:latest"); hit {
		t.Error("docker|image-a:latest should have been invalidated")
	}
	if _, hit := nonRootUserCache.Load("podman|image-b:latest"); !hit {
		t.Error("podman|image-b:latest must survive invalidating a different image")
	}
	nonRootUserCache.Delete("podman|image-b:latest") // test cleanup
}

// TestResolveNonRootUserDoesNotCacheProbeFailure — oracle-review NIT turned
// blocking-adjacent (ArgvOracleB round 2): a FAILED probe (transient — a
// slow cold pull that outran nonRootUserProbeTimeout, a momentary runtime
// hiccup) must NOT be cached. Caching it would permanently disable the
// non-root-USER fix for that image for the rest of the process lifetime
// after a single bad probe. probeIdentityFunc is swapped for a fake so this
// is hermetic (no container runtime needed) and can count calls precisely.
func TestResolveNonRootUserDoesNotCacheProbeFailure(t *testing.T) {
	orig := probeIdentityFunc
	t.Cleanup(func() { probeIdentityFunc = orig })

	calls := 0
	probeIdentityFunc = func(context.Context, string, string) containerIdentity {
		calls++
		return containerIdentity{} // ok=false: simulates a failed probe
	}

	const image = "test-image-cache-failure-not-persisted"
	InvalidateNonRootUserCache(image)
	t.Cleanup(func() { InvalidateNonRootUserCache(image) })

	uid1, gid1 := resolveNonRootUser(context.Background(), "podman", image)
	if uid1 != 0 || gid1 != 0 {
		t.Fatalf("resolveNonRootUser (call 1) = (%d,%d), want (0,0) on a failed probe", uid1, gid1)
	}
	if _, hit := nonRootUserCache.Load("podman|" + image); hit {
		t.Error("a failed probe must not be cached")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 after the first resolveNonRootUser call", calls)
	}

	// A second call must RETRY the probe (not short-circuit on a bad
	// cache entry) — this is what makes the fix self-healing.
	uid2, gid2 := resolveNonRootUser(context.Background(), "podman", image)
	if uid2 != 0 || gid2 != 0 {
		t.Fatalf("resolveNonRootUser (call 2) = (%d,%d), want (0,0) on a failed probe", uid2, gid2)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 — the second call should have retried the probe instead of trusting a cached failure", calls)
	}
}

// TestResolveNonRootUserCachesSuccessfulProbe: the counterpart to the test
// above — a DEFINITIVE (ok=true) result, including a root-USER image's
// uid==0, IS cached, so a second call for the same image never re-probes.
func TestResolveNonRootUserCachesSuccessfulProbe(t *testing.T) {
	orig := probeIdentityFunc
	t.Cleanup(func() { probeIdentityFunc = orig })

	calls := 0
	probeIdentityFunc = func(context.Context, string, string) containerIdentity {
		calls++
		return containerIdentity{uid: 1500, gid: 1500, ok: true}
	}

	const image = "test-image-cache-success-persisted"
	InvalidateNonRootUserCache(image)
	t.Cleanup(func() { InvalidateNonRootUserCache(image) })

	uid1, gid1 := resolveNonRootUser(context.Background(), "podman", image)
	if uid1 != 1500 || gid1 != 1500 {
		t.Fatalf("resolveNonRootUser (call 1) = (%d,%d), want (1500,1500)", uid1, gid1)
	}
	uid2, gid2 := resolveNonRootUser(context.Background(), "podman", image)
	if uid2 != 1500 || gid2 != 1500 {
		t.Fatalf("resolveNonRootUser (call 2) = (%d,%d), want (1500,1500) from cache", uid2, gid2)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 — the second call should have hit the cache, not re-probed", calls)
	}
}
