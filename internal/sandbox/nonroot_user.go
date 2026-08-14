package sandbox

// nonroot_user.go resolves the numeric UID/GID a container image's own USER
// directive maps to, so buildRunArgs can pass podman
// "--userns=keep-id:uid=,gid=" the exact identity it needs (see
// buildRunArgs' doc comment for the full mechanism and bugbot-p8y4's
// --design for the alternatives evaluated and rejected).
//
// Config.User in an image manifest may be a bare name ("ubuntu"), a numeric
// UID, "uid:gid", or empty (root) — there is no way to resolve a NAME to a
// number without consulting the image's own /etc/passwd, which requires
// actually starting a container. So this runs a tiny, cheap probe
// (`id -u; id -g`) inside the target image once per (runtime, image) and
// caches the result for the lifetime of the process, mirroring
// capabilities.go's ProbeCapabilities cache pattern (a sync.Map keyed by a
// composed string, filled in lazily and shared by every caller).

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nonRootUserProbeTimeout bounds the identity probe's wall-clock cost. The
// probe runs no user code beyond `id`, so this is generous headroom for a
// slow image pull/cold start, not a budget for real work.
//
// This cap is spent OUTSIDE any Spec.Timeout: cli.go's Exec resolves the
// identity before its own runCtx (bounded by spec.Timeout) is created, so
// this constant directly sets Exec's worst-case ADDITIONAL wall time —
// spec.Timeout + nonRootUserProbeTimeout, not spec.Timeout alone. See
// Exec's doc comment for why that tradeoff was chosen (sharing one clock
// let a slow probe falsely TimedOut a command that fit its own budget, and
// silently poisoned capabilities.go's cached capability detection). A
// FAILED probe is never cached (resolveNonRootUser) and is retried on
// every subsequent Exec against that image with NO cap on retry attempts
// or backoff — intentionally self-healing rather than permanently
// disabled, at the cost of repaying up to this full timeout on each retry
// until the underlying condition clears.
const nonRootUserProbeTimeout = 30 * time.Second

// containerIdentity is the cached result of probing one image: the resolved
// uid/gid (both 0 for a root-USER image, which needs no override) and
// whether the probe itself succeeded (ok=false on any infrastructure
// failure — image lacks /bin/sh or `id`, timeout, runtime error — in which
// case the caller falls back to no override, exactly like the pre-p8y4
// behavior for that image, rather than blocking the run).
type containerIdentity struct {
	uid, gid int
	ok       bool
}

// nonRootUserCache is the global probe cache keyed by runtime+"|"+image.
// Only DEFINITIVE (ok=true) results are ever stored — see resolveNonRootUser.
var nonRootUserCache sync.Map

// probeIdentityFunc is the function resolveNonRootUser calls to perform the
// actual probe. A package-level variable (not a hardcoded call to
// probeContainerIdentity) so unit tests can substitute a fake and pin the
// cache's success/failure behavior without a container runtime; production
// code never reassigns it.
var probeIdentityFunc = probeContainerIdentity

// resolveNonRootUser resolves image's default (non-flag-overridden) UID/GID
// under runtime, caching the result for the process lifetime. It returns
// uid=0 (meaning "no override needed") whenever the image runs as root, the
// probe fails for any reason, or runtime is not "podman" (the mechanism
// this exists for — see buildRunArgs — has no Docker equivalent, so
// resolving an identity for it would be pointless).
//
// Best-effort by design, matching capabilities.go's ProbeCapabilities and
// the rest of this package's "a probe failure degrades a feature, it never
// fails the run" convention (see e.g. readCaptureFile): a broken probe here
// simply leaves buildRunArgs on the pre-p8y4 default behavior (no identity
// override), reproducing whatever ORIGINAL failure mode that one image had
// without this fix — typically a permission-denied read of /workspace, but
// for a USER UID that exceeds the invoking host user's /etc/subuid range
// (e.g. an OpenShift-style high UID) the probe itself fails with a hard OCI
// runtime error ("crun: setresuid ... Invalid argument") rather than a
// read failure; either way this function degrades to no override rather
// than blocking every other Exec.
//
// The probe itself: `podman run --rm --network=none --entrypoint=
// --read-only --cap-drop ALL --security-opt no-new-privileges IMAGE sh -c
// "id -u; id -g"`. It mounts nothing, runs no Spec-supplied command, and
// touches no workspace — it only reads the image's own baked-in /etc/passwd
// entry for its default USER, so the same defense-in-depth flags buildRunArgs
// applies to a real run are cheap insurance here too, not a load-bearing
// requirement.
func resolveNonRootUser(ctx context.Context, runtime, image string) (uid, gid int) {
	if runtime != "podman" || image == "" {
		return 0, 0
	}
	key := runtime + "|" + image
	if v, hit := nonRootUserCache.Load(key); hit {
		id := v.(containerIdentity)
		return id.uid, id.gid
	}

	id := probeIdentityFunc(ctx, runtime, image)
	if !id.ok {
		// Do NOT cache a failed probe: caching it would permanently disable
		// the non-root-USER fix for this image, for the rest of the
		// process lifetime, after a single TRANSIENT failure (a slow
		// cold pull that outran nonRootUserProbeTimeout, a momentary
		// runtime hiccup). Leaving it uncached means every later call for
		// the same image simply retries — bounded by the same
		// nonRootUserProbeTimeout cap each time, and self-healing once the
		// image is warm or the transient condition clears.
		return 0, 0
	}
	// Only a DEFINITIVE result (the probe genuinely completed and its
	// output parsed) is cached — including uid==0 (a real root-USER
	// image: a stable answer worth remembering, needs no override either
	// way). Races between concurrent callers for the same
	// never-before-seen image just probe twice; LoadOrStore keeps
	// whichever finished first as the cached value so they agree
	// afterward — its "loaded" return is ignored deliberately, since a
	// concurrent winner's containerIdentity is exactly as valid as ours.
	actual, _ := nonRootUserCache.LoadOrStore(key, id)
	cached := actual.(containerIdentity)
	return cached.uid, cached.gid
}

// probeContainerIdentity runs the actual `id -u`/`id -g` probe. Split out
// from resolveNonRootUser so the cache-lookup/store logic stays free of I/O
// concerns.
func probeContainerIdentity(ctx context.Context, runtime, image string) containerIdentity {
	probeCtx, cancel := context.WithTimeout(ctx, nonRootUserProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, runtime,
		"run", "--rm", "--network=none", "--entrypoint=",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		image, "sh", "-c", "id -u; id -g",
	)
	out, err := cmd.Output()
	if err != nil {
		return containerIdentity{}
	}
	return parseIdentityProbeOutput(string(out))
}

// parseIdentityProbeOutput parses the "id -u; id -g" probe's stdout: exactly
// two whitespace-separated non-negative integers, uid then gid. Split out
// from probeContainerIdentity so the parsing contract can be pinned in a
// unit test without a container runtime — malformed output (wrong line
// count, non-numeric text such as a runtime error message printed to
// stdout, a negative number) always resolves to the zero-value
// containerIdentity{} (ok=false), matching probeContainerIdentity's own
// fail-closed-to-"no override" behavior on a command error.
func parseIdentityProbeOutput(out string) containerIdentity {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return containerIdentity{}
	}
	uid, uErr := strconv.Atoi(fields[0])
	gid, gErr := strconv.Atoi(fields[1])
	if uErr != nil || gErr != nil || uid < 0 || gid < 0 {
		return containerIdentity{}
	}
	return containerIdentity{uid: uid, gid: gid, ok: true}
}

// InvalidateNonRootUserCache removes a cached identity for image (all
// runtimes), forcing the next resolveNonRootUser call to re-probe. Intended
// for tests that need a clean slate, mirroring
// capabilities.go's InvalidateCapabilityCache.
func InvalidateNonRootUserCache(image string) {
	suffix := "|" + image
	nonRootUserCache.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && strings.HasSuffix(key, suffix) {
			nonRootUserCache.Delete(key)
		}
		return true
	})
}
