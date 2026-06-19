package funnel

// TestCodeNavInjection verifies the two lifecycle modes:
//
//  1. Injected (daemon-owned) nav: funnel.Close() must NOT close it.
//  2. Self-owned nav: funnel.Close() MUST close it.
//
// Both tests use the internal funnel.Options.CodeNav field directly (this file
// is in package funnel, so it has access to unexported fields for verification).

import (
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
)

// TestCodeNavInjected_CloseDoesNotCloseNav checks that a funnel receiving a
// pre-built CodeNav via Options.CodeNav leaves that nav open after Close().
func TestCodeNavInjected_CloseDoesNotCloseNav(t *testing.T) {
	st, repo := openFixture(t)

	// Build a real CodeNav pointing at the fixture repo.
	nav, err := agent.NewCodeNav(repo.Root())
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	// Caller (simulating daemon) owns the lifecycle; close it on test cleanup.
	t.Cleanup(func() { _ = nav.Close() })

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		CodeNav: nav,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Verify the funnel recorded the injection correctly.
	if f.nav != nav {
		t.Fatal("funnel.nav != injected nav")
	}
	if f.ownsNav {
		t.Fatal("funnel.ownsNav = true for injected nav; want false")
	}

	// Close the funnel. Must NOT close the nav (nav is daemon-owned).
	if err := f.Close(); err != nil {
		t.Fatalf("f.Close: %v", err)
	}

	// Confirm the nav is still usable: Tools() panics or errors only when the
	// nav is in a broken state; checking the count is the lightest safe probe.
	tools := nav.Tools()
	if len(tools) == 0 {
		t.Error("nav.Tools() returned no tools after funnel.Close() — nav may have been closed")
	}
}

// TestCodeNavSelfOwned_CloseClosesNav checks that when Options.CodeNav is nil
// the funnel constructs its own nav and DOES close it on Close().
// We verify ownsNav rather than the liveness of the nav (the lsp.Manager's
// Close is idempotent and cheap; testing internal state is the only portable
// signal without injecting a fake navigator here).
func TestCodeNavSelfOwned_CloseClosesNav(t *testing.T) {
	st, repo := openFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Trigger lazy nav creation by calling codeNav().
	nav, err := f.codeNav()
	if err != nil {
		t.Fatalf("codeNav: %v", err)
	}
	if nav == nil {
		t.Fatal("codeNav returned nil")
	}
	if !f.ownsNav {
		t.Fatal("funnel.ownsNav = false for self-created nav; want true")
	}

	// Close must not error; the nav is self-owned so this is the right closer.
	if err := f.Close(); err != nil {
		t.Fatalf("f.Close: %v", err)
	}
	// Second close must be safe (funnel.Close is documented safe to call multiple times).
	if err := f.Close(); err != nil {
		t.Fatalf("f.Close (second call): %v", err)
	}
}

// TestCodeNavInjected_NavField checks that when Options.CodeNav is set, codeNav()
// returns exactly the injected instance without constructing a new one.
func TestCodeNavInjected_NavField(t *testing.T) {
	st, repo := openFixture(t)

	nav, err := agent.NewCodeNav(repo.Root())
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	t.Cleanup(func() { _ = nav.Close() })

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		CodeNav: nav,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := f.codeNav()
	if err != nil {
		t.Fatalf("codeNav: %v", err)
	}
	if got != nav {
		t.Errorf("codeNav() returned a different instance than the injected nav")
	}
}
