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

// TestResolveHostToolchains_RefusesHomeBinAscent guards against over-mounting
// $HOME when a toolchain lives directly at $HOME/bin/<name> (a common manual
// install layout): ascending past bin/ here would RO-mount the ENTIRE home
// directory (SSH keys, git credentials, unrelated dotfiles) into the sandbox.
// The mount must stay narrowed to the bin/ directory itself.
func TestResolveHostToolchains_RefusesHomeBinAscent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME/bin layout assumed POSIX")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "fakenode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho fake-node version 9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	res, err := ResolveHostToolchains([]string{"fakenode"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d: %+v", len(res.Mounts), res.Mounts)
	}
	if res.Mounts[0].HostPath != binDir {
		t.Errorf("mount HostPath = %q, want the narrow bin dir %q (must NOT ascend to $HOME %q)",
			res.Mounts[0].HostPath, binDir, home)
	}
}

// TestResolveHostToolchains_RefusesLocalBinAscent covers the ~/.local/bin/<name>
// layout the same way.
func TestResolveHostToolchains_RefusesLocalBinAscent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("~/.local/bin layout assumed POSIX")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	localDir := filepath.Join(home, ".local")
	binDir := filepath.Join(localDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "fakenode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho fake-node version 9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	res, err := ResolveHostToolchains([]string{"fakenode"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 || res.Mounts[0].HostPath != binDir {
		t.Fatalf("mount HostPath = %+v, want the narrow bin dir %q (must NOT ascend to ~/.local %q)",
			res.Mounts, binDir, localDir)
	}
}

// TestResolveHostToolchains_StillAscendsForNarrowVersionedRoot verifies the
// guard does NOT block the legitimate nvm/asdf case: ascending out of a
// bin/ directory that sits under a narrow, versioned toolchain root (not
// $HOME or a broad catch-all) still happens, exactly as
// TestResolveHostToolchains_SymlinkClosure already pins — this test just
// makes the "guard does not over-trigger" property explicit against a
// $HOME-adjacent-but-not-equal path.
func TestResolveHostToolchains_StillAscendsForNarrowVersionedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("versioned bin layout assumed POSIX")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	versionedRoot := filepath.Join(home, ".nvm", "versions", "node", "v18.0.0")
	binDir := filepath.Join(versionedRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "fakenode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho fake-node version 18.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	res, err := ResolveHostToolchains([]string{"fakenode"})
	if err != nil {
		t.Fatalf("ResolveHostToolchains: %v", err)
	}
	if len(res.Mounts) != 1 || res.Mounts[0].HostPath != versionedRoot {
		t.Fatalf("mount HostPath = %+v, want the versioned root %q (narrow ascent must still happen for legitimate nvm/asdf layouts)",
			res.Mounts, versionedRoot)
	}
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
