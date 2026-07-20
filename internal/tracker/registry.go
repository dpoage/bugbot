package tracker

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// registry maps adapter names to their factories. Access is guarded by
// registryMu: Register is intended for package init() use, but tests
// register stubs at runtime, so the map is locked rather than relying on an
// unenforced init-only discipline.
var (
	registryMu sync.Mutex
	registry   = make(map[string]func(Config) (Tracker, error))
)

// Register makes a tracker factory available under name. It is intended to
// be called from an adapter package's init(). Registering two factories
// under the same name is a programmer error: Register panics with the
// duplicate name.
func Register(name string, factory func(Config) (Tracker, error)) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if factory == nil {
		panic("tracker: Register " + name + " with nil factory")
	}
	if _, dup := registry[name]; dup {
		panic("tracker: Register called twice for tracker " + name)
	}
	registry[name] = factory
}

// New constructs the tracker registered under name, passing cfg through to
// its factory. An unknown name is an error whose message names every
// registered tracker.
func New(name string, cfg Config) (Tracker, error) {
	registryMu.Lock()
	factory, ok := registry[name]
	registryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tracker: unknown tracker %q (known: %s)", name, strings.Join(Known(), ", "))
	}
	return factory(cfg)
}

// Known returns the sorted names of all registered trackers. The slice is a
// fresh copy; callers may modify it.
func Known() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	return slices.Sorted(maps.Keys(registry))
}
