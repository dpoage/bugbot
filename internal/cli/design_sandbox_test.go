package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/repro"
	"gopkg.in/yaml.v3"
)

// ── deterministic tier ────────────────────────────────────────────────────────

func TestSynthesisSandboxGo(t *testing.T) {
	facts := primeFacts{
		Target:       "/repo",
		BuildSystems: []ingest.BuildSystem{ingest.BuildSystemGoModule},
		GoVersion:    "1.21",
		Image:        "docker.io/library/golang:1.21-alpine",
		DepStrategy:  "host",
	}
	mined := minerOutput{}
	c := synthesisSandbox(facts, mined)

	if c.Image != "docker.io/library/golang:1.21-alpine" {
		t.Errorf("Image = %q", c.Image)
	}
	if c.DepStrategy != "host" {
		t.Errorf("DepStrategy = %q", c.DepStrategy)
	}
	if c.Network != "none" {
		t.Errorf("Network = %q, want none", c.Network)
	}
	if c.CPUs != defaultCPUs {
		t.Errorf("CPUs = %d", c.CPUs)
	}
	if c.MemoryMB != defaultMemoryMB {
		t.Errorf("MemoryMB = %d", c.MemoryMB)
	}
	if c.Tier != "deterministic" {
		t.Errorf("Tier = %q", c.Tier)
	}
}

func TestSynthesisSandboxWithSystemDeps(t *testing.T) {
	facts := primeFacts{
		Target:      "/repo",
		Image:       "docker.io/library/python:3-slim",
		DepStrategy: "fetch",
	}
	mined := minerOutput{
		SystemDeps: []string{"libpq-dev", "gcc"},
	}
	c := synthesisSandbox(facts, mined)

	if len(c.SetupCmds) == 0 {
		t.Fatal("expected SetupCmds from mined system deps")
	}
	cmd := c.SetupCmds[0]
	if len(cmd) < 2 || cmd[0] != "apt-get" {
		t.Errorf("SetupCmds[0] = %v, want apt-get ...", cmd)
	}
	foundPkg := func(want string) bool {
		for _, arg := range cmd {
			if arg == want {
				return true
			}
		}
		return false
	}
	if !foundPkg("libpq-dev") {
		t.Errorf("libpq-dev not in setup_cmds %v", cmd)
	}
	if !foundPkg("gcc") {
		t.Errorf("gcc not in setup_cmds %v", cmd)
	}
}

func TestSynthesisSandboxMinerImageFallback(t *testing.T) {
	// When prime can't pick an image (unknown build system) and the miner has one,
	// the miner image should be used.
	facts := primeFacts{
		Target:      "/repo",
		Image:       "", // unknown build system → no prime recommendation
		DepStrategy: "off",
	}
	mined := minerOutput{
		BaseImage: "custom-registry/myapp:latest",
	}
	c := synthesisSandbox(facts, mined)
	if c.Image != "custom-registry/myapp:latest" {
		t.Errorf("Image = %q, want custom-registry/myapp:latest", c.Image)
	}
}

func TestSynthesisSandboxNoSetupCmdsWhenNoSystemDeps(t *testing.T) {
	facts := primeFacts{Image: "docker.io/library/golang:1.21-alpine", DepStrategy: "host"}
	mined := minerOutput{}
	c := synthesisSandbox(facts, mined)
	if len(c.SetupCmds) != 0 {
		t.Errorf("expected no setup_cmds, got %v", c.SetupCmds)
	}
}

// ── renderSandboxBlock ────────────────────────────────────────────────────────

func TestRenderSandboxBlock(t *testing.T) {
	c := candidateBlock{
		Image:       "docker.io/library/golang:1.21-alpine",
		DepStrategy: "host",
		Network:     "none",
		CPUs:        2,
		MemoryMB:    2048,
		TimeoutSecs: 120,
	}
	out := renderSandboxBlock(candidateToSandboxConfig(c))
	if !strings.Contains(out, "sandbox:") {
		t.Errorf("missing 'sandbox:' in rendered block:\n%s", out)
	}
	if !strings.Contains(out, "golang:1.21-alpine") {
		t.Errorf("missing image in rendered block:\n%s", out)
	}
}

// ── sandboxProposalSchema validation ─────────────────────────────────────────

func TestSandboxProposalSchemaValid(t *testing.T) {
	validJSON := `{
		"image": "docker.io/library/golang:1.21-alpine",
		"dep_strategy": "host",
		"setup_cmds": [],
		"local_mounts": [],
		"rationale": "Go module repo, host dep strategy"
	}`
	var out sandboxProposal
	if err := json.Unmarshal([]byte(validJSON), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Image != "docker.io/library/golang:1.21-alpine" {
		t.Errorf("Image = %q", out.Image)
	}
}

// ── agent tier: fake client + mock sandbox (env_error → ok) ──────────────────

// scriptedLLMClient is a minimal scripted llm.Client for design_sandbox tests.
type scriptedLLMClient struct {
	bodies []string
	idx    int
}

func (c *scriptedLLMClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (c *scriptedLLMClient) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	body := "{}"
	if c.idx < len(c.bodies) {
		body = c.bodies[c.idx]
	}
	c.idx++
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
	}, nil
}

var _ llm.Client = (*scriptedLLMClient)(nil)

// verifyFn is the injected verifier type for agent-tier tests.
type verifyFn func(context.Context, string, candidateBlock) (repro.SmokeVerdict, error)

// runAgentTierTestable is the testable variant of runAgentTier that accepts an
// injected verify function so tests don't need a real podman runtime.
func runAgentTierTestable(
	ctx context.Context,
	w *bytes.Buffer,
	repoDir string,
	initial candidateBlock,
	mined minerOutput,
	firstVerdict repro.SmokeVerdict,
	runner *agent.Runner,
	verify verifyFn,
) (candidateBlock, error) {
	current := initial
	verdict := firstVerdict

	for i := 0; i < maxAgentIterations; i++ {
		task := buildAgentTask(current, mined, verdict)

		var proposal sandboxProposal
		if _, err := runner.RunJSON(ctx, task, sandboxProposalSchema, &proposal); err != nil {
			return current, err
		}

		next := current
		next.Image = proposal.Image
		next.DepStrategy = proposal.DepStrategy
		next.SetupCmds = proposal.SetupCmds
		next.LocalMounts = make([]config.LocalMount, 0, len(proposal.LocalMounts))
		for _, m := range proposal.LocalMounts {
			next.LocalMounts = append(next.LocalMounts, config.LocalMount{
				Host:      m.Host,
				Container: m.Container,
			})
		}
		next.Tier = "agent"
		next.Rationale = proposal.Rationale

		newVerdict, verifyErr := verify(ctx, repoDir, next)
		if verifyErr != nil {
			next.Verdict = newVerdict
			return next, nil
		}
		next.Verdict = newVerdict
		current = next
		verdict = newVerdict

		if verdict.OK {
			return current, nil
		}
	}
	return current, nil
}

// TestAgentTierConverges verifies the iterate loop terminates and converges when
// the mock verifier returns env_error on the first attempt and ok on the second.
func TestAgentTierConverges(t *testing.T) {
	repoDir := t.TempDir()

	proposalJSON := `{
		"image": "docker.io/library/golang:1.21-alpine",
		"dep_strategy": "host",
		"setup_cmds": [],
		"local_mounts": [],
		"rationale": "proposed by agent after env_error"
	}`
	client := &scriptedLLMClient{bodies: []string{proposalJSON, proposalJSON, proposalJSON}}

	tools, err := designSandboxReadOnlyTools(repoDir)
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	runner := agent.NewRunner(client, tools, designSandboxSystemPrompt(),
		agent.WithLimits(agent.Limits{MaxIterations: 20}))

	initial := candidateBlock{
		Image:       "docker.io/library/ubuntu:22.04",
		DepStrategy: "off",
		Network:     "none",
		CPUs:        2,
		MemoryMB:    2048,
		TimeoutSecs: 120,
		Tier:        "deterministic",
	}
	firstVerdict := repro.SmokeVerdict{OK: false, Category: "env_error", Detail: "go not found"}
	mined := minerOutput{RawArtifacts: map[string]string{}}

	result, err := runAgentTierTestable(
		context.Background(),
		&bytes.Buffer{},
		repoDir,
		initial,
		mined,
		firstVerdict,
		runner,
		// Mock verifier: always ok (simulates agent proposal fixed it).
		func(_ context.Context, _ string, _ candidateBlock) (repro.SmokeVerdict, error) {
			return repro.SmokeVerdict{OK: true, Category: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("runAgentTierTestable: %v", err)
	}
	if result.Tier != "agent" {
		t.Errorf("Tier = %q, want agent", result.Tier)
	}
	if !result.Verdict.OK {
		t.Errorf("Verdict.OK = false, want true")
	}
	if result.Rationale == "" {
		t.Errorf("Rationale empty, want agent rationale")
	}
}

// TestAgentTierExhausts verifies the loop terminates after maxAgentIterations
// even if verify never returns ok.
func TestAgentTierExhausts(t *testing.T) {
	repoDir := t.TempDir()
	proposalJSON := `{
		"image": "docker.io/library/ubuntu:22.04",
		"dep_strategy": "off",
		"setup_cmds": [],
		"local_mounts": [],
		"rationale": "still failing"
	}`
	bodies := make([]string, maxAgentIterations)
	for i := range bodies {
		bodies[i] = proposalJSON
	}
	client := &scriptedLLMClient{bodies: bodies}

	tools, err := designSandboxReadOnlyTools(repoDir)
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	runner := agent.NewRunner(client, tools, designSandboxSystemPrompt(),
		agent.WithLimits(agent.Limits{MaxIterations: 20}))

	initial := candidateBlock{
		Image: "docker.io/library/ubuntu:22.04", DepStrategy: "off",
		Network: "none", CPUs: 2, MemoryMB: 2048, TimeoutSecs: 120,
		Tier: "deterministic",
	}
	firstVerdict := repro.SmokeVerdict{OK: false, Category: "dep_missing", Detail: "go not found"}
	mined := minerOutput{RawArtifacts: map[string]string{}}

	callCount := 0
	result, err := runAgentTierTestable(
		context.Background(),
		&bytes.Buffer{},
		repoDir,
		initial,
		mined,
		firstVerdict,
		runner,
		func(_ context.Context, _ string, _ candidateBlock) (repro.SmokeVerdict, error) {
			callCount++
			return repro.SmokeVerdict{OK: false, Category: "dep_missing"}, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount > maxAgentIterations {
		t.Errorf("verify called %d times, want ≤ %d", callCount, maxAgentIterations)
	}
	_ = result
}

// ── --write merge tests ───────────────────────────────────────────────────────

func TestMergeSandboxBlockPreservesUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bugbot.yaml")

	existing := `storage:
  path: .bugbot/state.db
roles:
  finder:
    provider: anthropic
    model: claude-3-5-sonnet-20241022
sandbox:
  image: old-image:latest
  dep_strategy: "off"
  network: none
  cpus: 1
  memory_mb: 1024
  timeout_seconds: 60
  setup_cmds: []
  local_mounts: []
`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	proposed := candidateBlock{
		Image:       "docker.io/library/golang:1.21-alpine",
		DepStrategy: "host",
		Network:     "none",
		CPUs:        2,
		MemoryMB:    2048,
		TimeoutSecs: 120,
		Tier:        "deterministic",
	}

	if err := mergeSandboxBlockToPath(cfgPath, candidateToSandboxConfig(proposed)); err != nil {
		t.Fatalf("mergeSandboxBlockToPath: %v", err)
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	outStr := string(out)

	if !strings.Contains(outStr, "storage:") {
		t.Errorf("storage: key lost after merge:\n%s", outStr)
	}
	if !strings.Contains(outStr, "roles:") {
		t.Errorf("roles: key lost after merge:\n%s", outStr)
	}
	if !strings.Contains(outStr, "golang:1.21-alpine") {
		t.Errorf("new image not present:\n%s", outStr)
	}
	if strings.Contains(outStr, "old-image:latest") {
		t.Errorf("old image still present:\n%s", outStr)
	}
}

func TestMergeSandboxBlockAddsKeyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bugbot.yaml")

	existing := `storage:
  path: .bugbot/state.db
`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	proposed := candidateBlock{
		Image: "docker.io/library/python:3-slim", DepStrategy: "fetch",
		Network: "none", CPUs: 2, MemoryMB: 2048, TimeoutSecs: 120,
	}
	if err := mergeSandboxBlockToPath(cfgPath, candidateToSandboxConfig(proposed)); err != nil {
		t.Fatalf("mergeSandboxBlockToPath: %v", err)
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "sandbox:") {
		t.Errorf("sandbox: key not added:\n%s", outStr)
	}
	if !strings.Contains(outStr, "python:3-slim") {
		t.Errorf("image not in output:\n%s", outStr)
	}
	if !strings.Contains(outStr, "storage:") {
		t.Errorf("storage: key lost:\n%s", outStr)
	}
}

func TestMergedYAMLIsValidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bugbot.yaml")

	existing := `storage:
  path: .bugbot/state.db
sandbox:
  image: old:latest
  dep_strategy: "off"
  network: none
  cpus: 1
  memory_mb: 512
  timeout_seconds: 30
  setup_cmds: []
  local_mounts: []
`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	proposed := candidateBlock{
		Image: "docker.io/library/golang:1.22-alpine", DepStrategy: "host",
		Network: "none", CPUs: 4, MemoryMB: 4096, TimeoutSecs: 300,
	}
	if err := mergeSandboxBlockToPath(cfgPath, candidateToSandboxConfig(proposed)); err != nil {
		t.Fatalf("merge: %v", err)
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := yaml.Unmarshal(out, &generic); err != nil {
		t.Fatalf("merged yaml invalid: %v\n%s", err, out)
	}
	if _, ok := generic["sandbox"]; !ok {
		t.Errorf("sandbox key missing in merged yaml:\n%s", out)
	}
}

// ── candidateToSandboxConfig ──────────────────────────────────────────────────

func TestCandidateToSandboxConfig(t *testing.T) {
	c := candidateBlock{
		Image:       "docker.io/library/golang:1.21-alpine",
		DepStrategy: "off",
		Network:     "none",
		CPUs:        2,
		MemoryMB:    2048,
		TimeoutSecs: 60,
	}
	cfg := candidateToSandboxConfig(c)
	if cfg.Image != c.Image {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.Network != "none" {
		t.Errorf("Network = %q", cfg.Network)
	}
	if cfg.DepStrategy != "off" {
		t.Errorf("DepStrategy = %q", cfg.DepStrategy)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// mergeSandboxBlockToPath is a testable variant of mergeSandboxBlock that
// operates on an explicit path.
func mergeSandboxBlockToPath(path string, proposed config.Sandbox) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var root yaml.Node
	if len(existing) > 0 {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return err
		}
	}

	proposedData, err := yaml.Marshal(proposed)
	if err != nil {
		return err
	}
	var proposedNode yaml.Node
	if err := yaml.Unmarshal(proposedData, &proposedNode); err != nil {
		return err
	}

	merged, err := spliceYAMLKey(&root, "sandbox", &proposedNode)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
