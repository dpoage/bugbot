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
