package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/llmkit/agent"
	"github.com/dpoage/llmkit/sandbox"
)

func finderExecCfg(sb sandbox.Sandbox) FinderExecConfig {
	return FinderExecConfig{
		Sandbox:   sb,
		RepoDir:   "/repo",
		Network:   sandbox.NetworkNone,
		MaxExecs:  3,
		Timeout:   120 * time.Second,
		OutputCap: 16384,
		ROMounts:  []sandbox.ROMount{{HostPath: "/host/mod", ContainerPath: "/mod", Shared: true}},
		Env:       []string{"GOFLAGS=-mod=readonly"},
		SetupCmds: [][]string{{"python", "-m", "pip", "install", "--no-index", "-e", "."}},
	}
}

func runFinderExec(t *testing.T, tool *FinderExecTool, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

func TestFinderExecTool_Def(t *testing.T) {
	none := NewFinderExecTool(finderExecCfg(&fakeSandbox{})).Def()
	hostCfg := finderExecCfg(&fakeSandbox{})
	hostCfg.Network = sandbox.NetworkHost
	host := NewFinderExecTool(hostCfg).Def()

	if none.Name != "run_code" {
		t.Errorf("tool name = %q, want run_code", none.Name)
	}

	const wantNone = "Run a shell command (bash -c) in a fresh copy of the repository with the project's environment; " +
		"returns the exit code, stdout, and stderr. Budget 3 runs, timeout 120s each. The network is disabled."
	const wantHost = "Run a shell command (bash -c) in a fresh copy of the repository with the project's environment; " +
		"returns the exit code, stdout, and stderr. Budget 3 runs, timeout 120s each. Do not rely on network access."
	if none.Description != wantNone {
		t.Errorf("description (network=none) = %q, want %q", none.Description, wantNone)
	}
	if host.Description != wantHost {
		t.Errorf("description (network=host) = %q, want %q", host.Description, wantHost)
	}

	if none.Description == host.Description {
		t.Error("the two network postures must produce different descriptions")
	}

	var schema map[string]any
	if err := json.Unmarshal(none.Parameters, &schema); err != nil {
		t.Fatalf("parameters schema is not valid JSON: %v", err)
	}
	if req, ok := schema["required"].([]any); !ok || len(req) != 1 || req[0] != "command" {
		t.Errorf("required = %v, want [command]", schema["required"])
	}
	if ap, ok := schema["additionalProperties"].(bool); !ok || ap {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
	props, _ := schema["properties"].(map[string]any)
	command, _ := props["command"].(map[string]any)
	if command["type"] != "string" {
		t.Errorf("command.type = %v, want string", command["type"])
	}
	if ml, _ := command["minLength"].(float64); ml != 1 {
		t.Errorf("command.minLength = %v, want 1", command["minLength"])
	}
	files, _ := props["files"].(map[string]any)
	if files["type"] != "object" {
		t.Errorf("files.type = %v, want object", files["type"])
	}
	if inner, _ := files["additionalProperties"].(map[string]any); inner["type"] != "string" {
		t.Errorf("files.additionalProperties.type = %v, want string", files["additionalProperties"])
	}
}

func TestFinderExecTool_BudgetPerInstance(t *testing.T) {
	for _, n := range []int{1, 3} {
		fs := &fakeSandbox{}
		cfg := finderExecCfg(fs)
		cfg.MaxExecs = n
		tool := NewFinderExecTool(cfg)

		for k := 1; k <= n; k++ {
			if _, err := runFinderExec(t, tool, map[string]any{"command": "echo hi"}); err != nil {
				t.Fatalf("N=%d: call %d must succeed: %v", n, k, err)
			}
			if got := len(fs.calls); got != k {
				t.Fatalf("N=%d: after call %d, Exec count = %d, want %d", n, k, got, k)
			}
		}

		_, err := runFinderExec(t, tool, map[string]any{"command": "echo hi"})
		if err == nil {
			t.Fatalf("N=%d: call %d must return a tool error", n, n+1)
		}
		if got := len(fs.calls); got != n {
			t.Fatalf("N=%d: rejected call reached Exec (count = %d, want %d)", n, got, n)
		}
	}
}

func TestFinderExecTool_ExecSpec(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewFinderExecTool(finderExecCfg(fs))

	if _, err := runFinderExec(t, tool, map[string]any{
		"command": "python -c 'print(1)'",
		"files":   map[string]string{"probe.py": "print(1)\n"},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("Exec called %d times, want 1", len(fs.calls))
	}

	want := sandbox.Spec{
		RepoDir:    "/repo",
		Cmd:        []string{"bash", "-c", "python -c 'print(1)'"},
		Network:    sandbox.NetworkNone,
		Timeout:    120 * time.Second,
		WriteFiles: map[string][]byte{"probe.py": []byte("print(1)\n")},
		ROMounts:   []sandbox.ROMount{{HostPath: "/host/mod", ContainerPath: "/mod", Shared: true}},
		Env:        []string{"GOFLAGS=-mod=readonly"},
		SetupCmds:  [][]string{{"python", "-m", "pip", "install", "--no-index", "-e", "."}},
	}
	if !reflect.DeepEqual(fs.calls[0], want) {
		t.Errorf("Exec Spec =\n%+v\nwant\n%+v", fs.calls[0], want)
	}
}

func TestFinderExecTool_FilesPathEscape(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewFinderExecTool(finderExecCfg(fs))

	_, err := runFinderExec(t, tool, map[string]any{
		"command": "echo hi",
		"files":   map[string]string{"../escape.py": "x"},
	})
	if err == nil {
		t.Fatal("an escaping files path must be an argument error")
	}
	var he *agent.ToolHealthError
	if errors.As(err, &he) {
		t.Errorf("escaping path must not be a *ToolHealthError, got %T: %v", err, err)
	}
	if len(fs.calls) != 0 {
		t.Errorf("escaping path must not reach Exec, got %d calls", len(fs.calls))
	}
}

func TestFinderExecTool_EmptyCommand(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewFinderExecTool(finderExecCfg(fs))

	if _, err := runFinderExec(t, tool, map[string]any{"command": ""}); err == nil {
		t.Fatal("empty command must be a tool error")
	}
	if _, err := runFinderExec(t, tool, map[string]any{"files": map[string]string{"probe.py": "x"}}); err == nil {
		t.Fatal("missing command must be a tool error")
	}
	if len(fs.calls) != 0 {
		t.Errorf("argument errors must not reach Exec, got %d calls", len(fs.calls))
	}
}

func TestFinderExecTool_RendersResultAtConfiguredCap(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stdout:   "012345678", // cap+1 bytes: cut at cap
		Stderr:   "boom",
		Duration: 2500 * time.Millisecond,
	}
	fs := &fakeSandbox{results: []sandbox.Result{res}}
	cfg := finderExecCfg(fs)
	cfg.OutputCap = 8
	tool := NewFinderExecTool(cfg)

	out, err := runFinderExec(t, tool, map[string]any{"command": "false"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := renderSandboxResult(res, 8); out != want {
		t.Errorf("run_code output = %q, want renderSandboxResult(res, 8) = %q", out, want)
	}
	const wantLiteral = "exit_code=1 timed_out=false duration=2500ms\n" +
		"\nSTDOUT:\n" +
		"01234567\n[stdout truncated at 8B]\n" +
		"\nSTDERR:\n" +
		"boom"
	if out != wantLiteral {
		t.Errorf("run_code output = %q, want %q", out, wantLiteral)
	}

	// cap=16384 keeps the historical marker bytes.
	big := sandbox.Result{ExitCode: 0, Stdout: strings.Repeat("x", sandboxOutputCap+1), Duration: time.Millisecond}
	if got := renderSandboxResult(big, sandboxOutputCap); !strings.Contains(got, "[stdout truncated at 16KiB]") {
		t.Errorf("cap=16384 must render the 16KiB marker, got %q", got[:80])
	}
}

func TestFinderExecTool_InfraErrorIsToolHealthError(t *testing.T) {
	fs := &fakeSandbox{err: errors.New("podman not found")}
	tool := NewFinderExecTool(finderExecCfg(fs))

	_, err := runFinderExec(t, tool, map[string]any{"command": "echo hi"})
	if err == nil {
		t.Fatal("infra error must be a tool error")
	}
	var he *agent.ToolHealthError
	if !errors.As(err, &he) {
		t.Fatalf("infra error must be a *agent.ToolHealthError, got %T: %v", err, err)
	}
	if he.Reason != "sandbox runtime unavailable" {
		t.Errorf("reason = %q, want %q", he.Reason, "sandbox runtime unavailable")
	}
}

func TestFinderExecTool_OnExec(t *testing.T) {
	var got []time.Duration
	fs := &fakeSandbox{results: []sandbox.Result{
		{ExitCode: 0, Duration: 10 * time.Millisecond},
		{ExitCode: 0, Duration: 20 * time.Millisecond},
	}}
	cfg := finderExecCfg(fs)
	cfg.MaxExecs = 2
	cfg.OnExec = func(d time.Duration) { got = append(got, d) }
	tool := NewFinderExecTool(cfg)

	for i := range 2 {
		if _, err := runFinderExec(t, tool, map[string]any{"command": "true"}); err != nil {
			t.Fatalf("call %d must succeed: %v", i+1, err)
		}
	}
	if _, err := runFinderExec(t, tool, map[string]any{"command": "true"}); err == nil {
		t.Fatal("third call must exhaust the budget")
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OnExec durations = %v, want %v (no call on budget error)", got, want)
	}
}

func TestFinderExecTool_OnExecNotCalledOnArgOrInfraError(t *testing.T) {
	called := 0
	cfg := finderExecCfg(&fakeSandbox{err: errors.New("runtime down")})
	cfg.OnExec = func(time.Duration) { called++ }
	tool := NewFinderExecTool(cfg)

	if _, err := runFinderExec(t, tool, map[string]any{"command": ""}); err == nil {
		t.Fatal("empty command must be a tool error")
	}
	if _, err := runFinderExec(t, tool, map[string]any{
		"command": "true",
		"files":   map[string]string{"../x": "x"},
	}); err == nil {
		t.Fatal("escaping files path must be a tool error")
	}
	if _, err := runFinderExec(t, tool, map[string]any{"command": "true"}); err == nil {
		t.Fatal("infra error must be a tool error")
	}
	if called != 0 {
		t.Errorf("OnExec called %d times on error paths, want 0", called)
	}
}
