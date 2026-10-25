package cli

// finder_exec_test.go holds the cli legs of P2 (daemon refusal, doctor
// report) and P4 (the printResult status line).
import (
	"bytes"
	"context"
	"errors"
	"github.com/dpoage/llmkit/sandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/sandboxenv"
)

// TestPrintResultFinderExecLine (P4): scan stdout carries exactly one
// "Finder exec: " line and it equals Result.FinderExecStatus verbatim.
func TestPrintResultFinderExecLine(t *testing.T) {
	status := "Finder exec: on (sandbox=UNSANDBOXED host, network=host, max_execs=8 per finder)"
	res := &funnel.Result{Commit: "abc1234", FinderExecStatus: status}
	res.Stats.FinderRuns = 1
	var buf bytes.Buffer
	printResult(&buf, res)

	var got []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, funnel.FinderExecLinePrefix) {
			got = append(got, line)
		}
	}
	if len(got) != 1 {
		t.Fatalf("Finder exec lines = %d (%q), want exactly 1", len(got), got)
	}
	if got[0] != status {
		t.Errorf("line = %q, want %q", got[0], status)
	}

	// An unprovisioned Result prints no Finder exec line.
	var none bytes.Buffer
	printResult(&none, &funnel.Result{Commit: "abc1234"})
	for _, line := range strings.Split(none.String(), "\n") {
		if strings.HasPrefix(line, funnel.FinderExecLinePrefix) {
			t.Errorf("unprovisioned result printed %q", line)
		}
	}
}

const daemonTestConfig = `providers:
  testprovider:
    type: anthropic
    api_key_env: FAKE_API_KEY_ENV
roles:
  finder:     {provider: testprovider, model: m}
  verifier:   {provider: testprovider, model: m}
  reproducer: {provider: testprovider, model: m}
sandbox:
  backend: unsandboxed
storage:
  path: %CFGDIR%/state.db
`

// TestDaemonRefusesUnsandboxedBackend (P2): the daemon never starts under the
// resolved unsandboxed backend — via yaml and via BUGBOT_SANDBOX_BACKEND.
func TestDaemonRefusesUnsandboxedBackend(t *testing.T) {
	run := func(t *testing.T, yaml string, env map[string]string) {
		t.Helper()
		dir := t.TempDir()
		for k, v := range env {
			t.Setenv(k, v)
		}
		if err := os.WriteFile(filepath.Join(dir, config.DefaultFileName), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		orig, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(orig) }()

		cmd := newDaemonCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unsandboxed") {
			t.Fatalf("daemon error = %v, want a refusal naming unsandboxed", err)
		}
		if !strings.Contains(err.Error(), "daemon") {
			t.Errorf("daemon error = %v, want it to name the daemon", err)
		}
	}

	dir := t.TempDir()
	run(t, strings.ReplaceAll(daemonTestConfig, "%CFGDIR%", dir), nil)

	noBackend := strings.Replace(strings.ReplaceAll(daemonTestConfig, "  backend: unsandboxed\n", ""), "%CFGDIR%", t.TempDir(), 1)
	run(t, noBackend, map[string]string{"BUGBOT_SANDBOX_BACKEND": "unsandboxed"})
}

const doctorUnsandboxedConfig = `providers:
  testprovider:
    type: anthropic
    api_key_env: FAKE_API_KEY_ENV
roles:
  finder:     {provider: testprovider, model: m}
  verifier:   {provider: testprovider, model: m}
  reproducer: {provider: testprovider, model: m}
sandbox:
  backend: unsandboxed
  runtime: podman
  image: docker.io/library/debian:stable-slim
  cpus: 2
  memory_mb: 2048
  timeout_seconds: 60
storage:
  path: %CFGDIR%/state.db
report:
  dir: %CFGDIR%/reports
`

// TestDoctorUnsandboxedBackendReports (P2/A6): doctor reports the unsandboxed
// backend and never fails on it, and never tries to build it.
func TestDoctorUnsandboxedBackendReports(t *testing.T) {
	dir := t.TempDir()
	yaml := strings.ReplaceAll(doctorUnsandboxedConfig, "%CFGDIR%", dir)
	path := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	newSandboxCalls := 0
	env := allGreenEnv(t, path)
	env.newSandbox = func(cfg config.Sandbox) (sandbox.Sandbox, error) {
		newSandboxCalls++
		return nil, errors.New("must not be called under unsandboxed")
	}
	env.out = &bytes.Buffer{}

	results := runChecks(context.Background(), env, true)

	if newSandboxCalls != 0 {
		t.Errorf("newSandbox called %d times under unsandboxed, want 0", newSandboxCalls)
	}
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			t.Errorf("hard failure under unsandboxed: %s: %s", r.Name, r.Detail)
		}
	}
	found := false
	verifierSkip := false
	for _, r := range results {
		if r.Name == "sandbox backend" {
			found = true
			if r.Status != statusPass || r.Detail != "unsandboxed (finder exec only, no isolation)" {
				t.Errorf("sandbox backend row = %s/%s, want PASS/unsandboxed (finder exec only, no isolation)", r.Status, r.Detail)
			}
		}
		if r.Name == "sandbox verifier" && r.Status == statusSkip && strings.Contains(r.Detail, "unsandboxed") {
			verifierSkip = true
		}
	}
	if !found {
		t.Error("no sandbox backend row reported")
	}
	if !verifierSkip {
		t.Error("sandbox verifier did not skip under unsandboxed")
	}
	if sandboxenv.BackendLabel(config.Sandbox{Backend: "unsandboxed"}) != "unsandboxed" {
		t.Error("BackendLabel sanity check failed")
	}
}
