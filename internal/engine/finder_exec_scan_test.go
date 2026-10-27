package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
)

// fakeRuntime is an executable stand-in for podman: Available sees it, every invocation fails.
func fakeRuntime(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// scanGitRepo is initReviewTestRepo plus untracked files; Scan and ResolveDeps read the working tree.
func scanGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := initReviewTestRepo(t)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var goRepoFiles = map[string]string{
	"go.mod":  "module example.com/fx\n\ngo 1.22\n",
	"main.go": "package main\n\nfunc div(a, b int) int { return a / b }\n\nfunc main() { _ = div(4, 2) }\n",
}

// scanTestCfg: finder exec in mode over a fake container runtime, verify off.
func scanTestCfg(t *testing.T, mode string) config.Config {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	cfg := testConfig(t)
	cfg.Scan.FinderExec = mode
	cfg.Sandbox.Runtime = fakeRuntime(t)
	return cfg
}

func openScanDispatcher(t *testing.T, cfg config.Config) *Dispatcher {
	t.Helper()
	d, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	d.finder = &fakeArbiterClient{body: `{"findings": []}`}
	d.verifier = &fakeArbiterClient{body: `{"findings": []}`}
	return d
}

type closeCountingSandbox struct {
	*sandbox.Mock
	closes int
}

func (c *closeCountingSandbox) Close() error { c.closes++; return nil }

func stubFinderExecSandbox(t *testing.T) *closeCountingSandbox {
	t.Helper()
	sb := &closeCountingSandbox{Mock: sandbox.NewMock(sandbox.MockResponse{})}
	orig := newFinderExecSandbox
	newFinderExecSandbox = func(config.Sandbox) (sandbox.Sandbox, error) { return sb, nil }
	t.Cleanup(func() { newFinderExecSandbox = orig })
	return sb
}

func TestProvisionSandboxBuildFailureDegrades(t *testing.T) {
	for _, mode := range []string{"auto", "on"} {
		t.Run(mode, func(t *testing.T) {
			cfg := scanTestCfg(t, mode)
			cfg.Sandbox.HostToolchains = []string{"nosuchtool-ypei"}
			opts, owned := provisionFinderExecForScan(cfg, funnel.SandboxOpts{}, io.Discard)
			want := "sandbox build failed: sandbox.host_toolchains: nosuchtool-ypei did not resolve on this host"
			if opts.OffReason != want {
				t.Errorf("OffReason = %q, want %q", opts.OffReason, want)
			}
			if opts.Sandbox != nil || owned != nil {
				t.Errorf("Sandbox/owned = %T/%T, want nil after a failed build", opts.Sandbox, owned)
			}
		})
	}
}

func TestScanSandboxBuildFailureDoesNotAbort(t *testing.T) {
	for _, mode := range []string{"auto", "on"} {
		t.Run(mode, func(t *testing.T) {
			cfg := scanTestCfg(t, mode)
			cfg.Sandbox.HostToolchains = []string{"nosuchtool-ypei"}
			d := openScanDispatcher(t, cfg)
			repo := scanGitRepo(t, goRepoFiles)
			res, err := d.Scan(context.Background(), ScanOpts{Target: repo, Out: io.Discard, ErrOut: io.Discard})
			if err != nil {
				t.Fatalf("Scan aborted on a finder-exec sandbox build failure: %v", err)
			}
			want := "Finder exec: off (sandbox build failed: sandbox.host_toolchains: nosuchtool-ypei did not resolve on this host)"
			if res.Result.FinderExecStatus != want {
				t.Errorf("status = %q, want %q", res.Result.FinderExecStatus, want)
			}
			if _, err := d.Scan(context.Background(), ScanOpts{Target: repo, DoEstimate: true, Out: io.Discard, ErrOut: io.Discard}); err != nil {
				t.Errorf("--estimate aborted on a finder-exec sandbox build failure: %v", err)
			}
		})
	}
}

func TestScanDepResolutionFailureDoesNotAbort(t *testing.T) {
	for _, mode := range []string{"auto", "on"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GOMODCACHE", "/nonexistent-ypei-modcache")
			cfg := scanTestCfg(t, mode)
			cfg.Sandbox.DepStrategy = "host"
			stubFinderExecSandbox(t)
			d := openScanDispatcher(t, cfg)
			repo := scanGitRepo(t, goRepoFiles)
			res, err := d.Scan(context.Background(), ScanOpts{Target: repo, Out: io.Discard, ErrOut: io.Discard})
			if err != nil {
				t.Fatalf("Scan aborted on a finder-exec dependency resolution failure: %v", err)
			}
			want := `Finder exec: off (dependency resolution failed: sandbox: host module cache "/nonexistent-ypei-modcache" does not exist`
			if !strings.HasPrefix(res.Result.FinderExecStatus, want) {
				t.Errorf("status = %q, want prefix %q", res.Result.FinderExecStatus, want)
			}
			if _, err := d.Scan(context.Background(), ScanOpts{Target: repo, DoEstimate: true, Out: io.Discard, ErrOut: io.Discard}); err != nil {
				t.Errorf("--estimate aborted on a finder-exec dependency resolution failure: %v", err)
			}
		})
	}
}

// With verify's sandbox built, a dependency resolution failure stays fatal (base behavior).
func TestScanVerifyOnDepResolutionStillFatal(t *testing.T) {
	t.Setenv("GOMODCACHE", "/nonexistent-ypei-modcache")
	cfg := scanTestCfg(t, "auto")
	cfg.Verify.SandboxExec = true
	cfg.Sandbox.DepStrategy = "host"
	d := openScanDispatcher(t, cfg)
	_, err := d.Scan(context.Background(), ScanOpts{Target: scanGitRepo(t, goRepoFiles), Out: io.Discard, ErrOut: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "funnel: resolve dependency strategy") {
		t.Fatalf("Scan error = %v, want the fatal dependency resolution error", err)
	}
}

func TestScanClosesOwnedFinderExecSandbox(t *testing.T) {
	cases := []struct {
		name    string
		opts    ScanOpts
		wantErr bool
	}{
		{name: "full scan", opts: ScanOpts{}},
		{name: "estimate", opts: ScanOpts{DoEstimate: true}},
		{name: "error after provisioning", opts: ScanOpts{Since: "no-such-ref-ypei"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := scanTestCfg(t, "on")
			sb := stubFinderExecSandbox(t)
			d := openScanDispatcher(t, cfg)
			opts := tc.opts
			opts.Target, opts.Out, opts.ErrOut = scanGitRepo(t, goRepoFiles), io.Discard, io.Discard
			_, err := d.Scan(context.Background(), opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Scan error = %v, wantErr %v", err, tc.wantErr)
			}
			if sb.closes != 1 {
				t.Errorf("finder-exec sandbox closed %d times, want 1", sb.closes)
			}
		})
	}
}

// Network is pinned none and limits come from scan.finder_exec_* on both real-backend paths.
func TestProvisionRealBackendNetworkAndLimits(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "constructed", true: "shared with verify"}[shared], func(t *testing.T) {
			cfg := scanTestCfg(t, "auto")
			cfg.Sandbox.Network = "host"
			cfg.Scan.FinderExecMaxExecs = 5
			cfg.Scan.FinderExecTimeoutSeconds = 45
			cfg.Scan.FinderExecOutputBytes = 4096
			built := stubFinderExecSandbox(t)
			verifySB := funnel.SandboxOpts{}
			if shared {
				verifySB = gateVerifyOpts(sandbox.NewMock(sandbox.MockResponse{}))
			}
			opts, owned := provisionFinderExecForScan(cfg, verifySB, io.Discard)
			if opts.Sandbox == nil {
				t.Fatalf("no sandbox provisioned: OffReason %q", opts.OffReason)
			}
			if !shared && (opts.Sandbox != built || owned != built) {
				t.Errorf("Sandbox/owned = %T/%T, want the constructed backend", opts.Sandbox, owned)
			}
			if opts.Network != sandbox.NetworkNone {
				t.Errorf("Network = %q, want none (sandbox.network is not consulted)", opts.Network)
			}
			if opts.MaxExecs != 5 || opts.Timeout != 45*time.Second || opts.OutputCap != 4096 {
				t.Errorf("limits = %d/%v/%d, want 5/45s/4096", opts.MaxExecs, opts.Timeout, opts.OutputCap)
			}
		})
	}
}

func TestDegradedWarningNamesUnsandboxedOnEveryVerb(t *testing.T) {
	want := "Warning: " + sandboxDegradedUnsandboxedWarning + "\n"
	newCfg := func(t *testing.T) config.Config {
		cfg := testConfig(t)
		cfg.Sandbox.Backend = "unsandboxed"
		cfg.Verify.SandboxExec = true
		return cfg
	}
	check := func(t *testing.T, got string) {
		t.Helper()
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
		if strings.Contains(got, SandboxDegradedWarning) {
			t.Errorf("output carries the no-container-runtime text: %q", got)
		}
	}
	ctx := context.Background()

	t.Run("sweep", func(t *testing.T) {
		d := openScanDispatcher(t, newCfg(t))
		var errOut bytes.Buffer
		_, _ = d.Sweep(ctx, SweepOpts{Target: scanGitRepo(t, goRepoFiles), Out: io.Discard, ErrOut: &errOut})
		check(t, errOut.String())
	})
	t.Run("reconcile", func(t *testing.T) {
		d := openScanDispatcher(t, newCfg(t))
		var out bytes.Buffer
		_, _ = d.Reconcile(ctx, ReconcileOpts{Out: &out})
		check(t, out.String())
	})
	t.Run("review", func(t *testing.T) {
		d := openScanDispatcher(t, newCfg(t))
		repo, err := ingest.Open(ctx, scanGitRepo(t, goRepoFiles))
		if err != nil {
			t.Fatal(err)
		}
		head, err := repo.HeadCommit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var errOut bytes.Buffer
		_, _ = d.Review(ctx, ReviewOpts{Repo: repo, BaseSHA: head, HeadSHA: head, Out: io.Discard, ErrOut: &errOut})
		check(t, errOut.String())
	})
}
