package sandboxenv

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
	ecoreg "github.com/dpoage/bugbot/internal/ecosystem"
)

// probeCache memoizes ProbeCapabilities results for the process lifetime;
// sandbox.Probe itself re-probes on every call.
var probeCache sync.Map

// ProbeCapabilities runs ecoreg.ProbeEntries against sb with base as the
// template Spec and returns the resulting CapabilitySet. Results are cached
// per process, keyed by sb's backend identity (see backendIdentity) plus
// base's image, mounts, and env. A Spec the backend refuses is returned as
// the error and never cached.
func ProbeCapabilities(ctx context.Context, sb sandbox.Sandbox, cfg config.Sandbox, base sandbox.Spec) (sandbox.CapabilitySet, error) {
	key := backendIdentity(sb, cfg) + "|" + specCacheKey(base)
	if v, ok := probeCache.Load(key); ok {
		return v.(sandbox.CapabilitySet), nil
	}
	cs, err := sandbox.Probe(ctx, sb, base, ecoreg.ProbeEntries)
	if err != nil {
		return nil, err
	}
	actual, _ := probeCache.LoadOrStore(key, cs)
	return actual.(sandbox.CapabilitySet), nil
}

// backendIdentity names what a probe result depends on beyond the Spec: the
// backend kind and the config it was built from. Unrecognized backends (the
// Mock, test fakes) are keyed per instance so they never share entries.
func backendIdentity(sb sandbox.Sandbox, cfg config.Sandbox) string {
	toolchains := strings.Join(cfg.HostToolchains, "\x1f")
	switch sb.(type) {
	case *sandbox.CLI:
		return "cli\x00" + cfg.Runtime + "\x00" + cfg.Image + "\x00" + toolchains
	case *sandbox.Bwrap:
		return "bwrap\x00" + toolchains
	case *sandbox.HostExec:
		return "host"
	default:
		return fmt.Sprintf("%T@%p", sb, sb)
	}
}

// specCacheKey is an order-independent key over base's image, mounts, and
// env; "rw:" vs "m:" keeps a read-only and a writable mount of the same path
// distinct.
func specCacheKey(base sandbox.Spec) string {
	parts := make([]string, 0, len(base.ROMounts)+len(base.RWMounts)+len(base.Env))
	for _, m := range base.ROMounts {
		parts = append(parts, "m:"+m.HostPath+"->"+m.ContainerPath)
	}
	for _, m := range base.RWMounts {
		parts = append(parts, "rw:"+m.HostPath+"->"+m.ContainerPath)
	}
	parts = append(parts, base.Env...)
	sort.Strings(parts)
	return base.Image + "|" + strings.Join(parts, ",")
}
