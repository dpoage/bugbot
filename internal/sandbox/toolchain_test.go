package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeToolchainHost builds a synthetic toolchain layout under a temp dir:
//
//	<root>/versions/1.2.3/bin/<name>   (the "real" executable, a tiny shell script)
//	<root>/shim/<name>                 -> symlink to the versioned bin (nvm/asdf shim pattern)
//
// and returns (root, shimDir) so a test can point PATH at shimDir to exercise
// the symlink-closure resolution in resolveToolchainRoot.
func fakeToolchainHost(t *testing.T, name string) (root, shimDir string) {
	t.Helper()
	root = t.TempDir()
	versionedBin := filepath.Join(root, "versions", "1.2.3", "bin")
	if err := os.MkdirAll(versionedBin, 0o755); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(versionedBin, name)
	script := "#!/bin/sh\necho fake-" + name + " version 1.2.3\n"
	if err := os.WriteFile(realExe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	shimDir = filepath.Join(root, "shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExe, filepath.Join(shimDir, name)); err != nil {
		t.Fatal(err)
	}
	return root, shimDir
}

func TestResolveHostToolchains_SymlinkClosure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + shebang semantics assumed POSIX")
	}
	root, shimDir := fakeToolchainHost(t, "fakenode")
	t.Setenv("PATH", shimDir)

	res, err := ResolveHostToolchains([]string{"fakenode"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d: %+v", len(res.Mounts), res.Mounts)
	}
	wantRoot := filepath.Join(root, "versions", "1.2.3")
	if res.Mounts[0].HostPath != wantRoot {
		t.Errorf("mount HostPath = %q, want %q (the versioned toolchain root, not the shim dir)", res.Mounts[0].HostPath, wantRoot)
	}
	if !res.Mounts[0].Shared {
		t.Error("host toolchain mount must have Shared=true (host-owned dir, no :Z relabel)")
	}
	if res.Mounts[0].ContainerPath != hostToolchainMountRoot+"/fakenode" {
		t.Errorf("ContainerPath = %q, want %s/fakenode", res.Mounts[0].ContainerPath, hostToolchainMountRoot)
	}
	wantPathDir := hostToolchainMountRoot + "/fakenode/bin"
	if res.PathPrepend != wantPathDir {
		t.Errorf("PathPrepend = %q, want %q", res.PathPrepend, wantPathDir)
	}
	if len(res.Fingerprints) != 1 || res.Fingerprints[0].Name != "fakenode" {
		t.Fatalf("Fingerprints = %+v, want one entry named fakenode", res.Fingerprints)
	}
	if res.Fingerprints[0].Path != wantRoot {
		t.Errorf("fingerprint Path = %q, want %q", res.Fingerprints[0].Path, wantRoot)
	}
	if res.Fingerprints[0].Version == "" {
		t.Error("fingerprint Version should be populated from the fake --version script output")
	}
}

func TestResolveHostToolchains_UnresolvableNameSkipped(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH: nothing resolves

	res, err := ResolveHostToolchains([]string{"definitely-not-a-real-toolchain-xyz"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains should be best-effort (no error), got %v", err)
	}
	if len(res.Mounts) != 0 || len(res.Fingerprints) != 0 || res.PathPrepend != "" {
		t.Errorf("unresolvable name should produce an empty resolution, got %+v", res)
	}
}

func TestResolveHostToolchains_ExplicitDir(t *testing.T) {
	dir := t.TempDir()

	res, err := ResolveHostToolchains([]string{dir})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 || res.Mounts[0].HostPath != dir {
		t.Fatalf("want one mount at %q, got %+v", dir, res.Mounts)
	}
	if res.PathPrepend != "" {
		t.Errorf("an explicit dir with no resolved executable should not contribute a PATH entry, got %q", res.PathPrepend)
	}
	// An explicit dir still gets a fingerprint (path only; version probing
	// against a bare directory fails silently).
	if len(res.Fingerprints) != 1 || res.Fingerprints[0].Path != dir {
		t.Errorf("Fingerprints = %+v, want one entry for %q", res.Fingerprints, dir)
	}
}

func TestResolveHostToolchains_DedupesContainerPaths(t *testing.T) {
	_, shimDir := fakeToolchainHost(t, "fakenode")
	t.Setenv("PATH", shimDir)

	res, err := ResolveHostToolchains([]string{"fakenode", "fakenode"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 {
		t.Errorf("duplicate entries should collapse to one mount, got %d: %+v", len(res.Mounts), res.Mounts)
	}
}

func TestResolveHostToolchains_EmptyAndBlankEntriesSkipped(t *testing.T) {
	res, err := ResolveHostToolchains([]string{"", "   "})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 0 {
		t.Errorf("blank entries should produce no mounts, got %+v", res.Mounts)
	}
}
