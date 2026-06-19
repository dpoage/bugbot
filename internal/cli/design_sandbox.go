package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// sandboxProposal is the structured output the agent tier produces via RunJSON.
// It mirrors the fields of config.Sandbox that the designer controls, so it
// can be validated by runjson.go's deep schema validator.
type sandboxProposal struct {
	Image       string     `json:"image"`
	DepStrategy string     `json:"dep_strategy"`
	SetupCmds   [][]string `json:"setup_cmds"`
	LocalMounts []struct {
		Host      string `json:"host"`
		Container string `json:"container"`
	} `json:"local_mounts"`
	Rationale string `json:"rationale"`
}

// sandboxProposalSchema is the JSON Schema for the agent tier's output,
// validated by agent.Runner.RunJSON. It enforces all required fields so the
// deep schema validator (runjson.go) can produce precise repair feedback.
var sandboxProposalSchema = json.RawMessage(`{
  "type": "object",
  "required": ["image", "dep_strategy", "setup_cmds", "local_mounts", "rationale"],
  "additionalProperties": false,
  "properties": {
    "image":        { "type": "string", "minLength": 1 },
    "dep_strategy": { "type": "string", "enum": ["off", "host", "fetch"] },
    "setup_cmds": {
      "type": "array",
      "items": {
        "type": "array",
        "items": { "type": "string" },
        "minItems": 1
      }
    },
    "local_mounts": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["host", "container"],
        "additionalProperties": false,
        "properties": {
          "host":      { "type": "string", "minLength": 1 },
          "container": { "type": "string", "minLength": 1 }
        }
      }
    },
    "rationale": { "type": "string", "minLength": 1 }
  }
}`)

// candidateBlock holds the proposed sandbox configuration along with metadata
// about which tier produced it and what the verify verdict was.
type candidateBlock struct {
	Image       string
	DepStrategy string
	Network     string
	CPUs        int
	MemoryMB    int
	TimeoutSecs int
	SetupCmds   [][]string
	LocalMounts []config.LocalMount

	// Metadata
	Tier    string // "deterministic" | "agent"
	Verdict repro.SmokeVerdict
	// Rationale from agent tier (empty for deterministic tier).
	Rationale string
}

// defaultCPUs / defaultMemoryMB / defaultTimeoutSecs are the sensible defaults
// emitted by the deterministic tier when the existing config does not specify
// resources.
const (
	defaultCPUs        = 2
	defaultMemoryMB    = 2048
	defaultTimeoutSecs = 120
)

// maxAgentIterations caps the agent-verify loop.
const maxAgentIterations = 3

// newDesignSandboxCmd returns the `bugbot design-sandbox` cobra command.
func newDesignSandboxCmd() *cobra.Command {
	var (
		target      string
		enableAgent bool
		doVerify    bool
		doWrite     bool
	)

	cmd := &cobra.Command{
		Use:   "design-sandbox",
		Short: "Detect and synthesize a sandbox configuration for the target repo",
		Long: `design-sandbox detects the target repository's build environment and
synthesizes a concrete sandbox: block for bugbot.yaml.

It runs in three tiers:
  1. Deterministic: artifact mining + gatherPrimeFacts → candidate sandbox block.
  2. Verify (default ON): smoke-test the candidate with the ftd.1 offline verifier.
     On success, done. On failure: print actionable error (no --agent) or hand off
     to the agent tier (--agent).
  3. Agent (--agent): read-only LLM agent reads mined artifacts + smoke feedback,
     proposes image/setup_cmds/local_mounts, re-verifies in a bounded loop (≤3).

The proposed sandbox: block is printed + diffed against the existing config.
Pass --write to merge it into bugbot.yaml (preserving all other keys).

Trust contract: never writes without --write; never runs mined commands outside
the hardened sandbox smoke test.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()

			repoDir, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", target, err)
			}

			// ── Tier 1: detect + deterministic synthesis ──────────────────────
			rt, runtimeOK := sandbox.Detect()
			facts := gatherPrimeFacts(repoDir, rt, runtimeOK)
			mined := mineArtifacts(repoDir)

			candidate := synthesisSandbox(facts, mined)

			// ── Tier 2: verify (default ON) ───────────────────────────────────
			if doVerify {
				verdict, verifyErr := verifyCandidate(ctx, repoDir, candidate)
				if verifyErr != nil {
					// Infrastructure failure (no runtime, etc.) — report but continue.
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"⚠  verify skipped (infrastructure error): %v\n", verifyErr)
				} else {
					candidate.Verdict = verdict
					if !verdict.OK {
						if !enableAgent {
							// Print actionable failure and return; no --agent.
							return fmt.Errorf("sandbox verify failed [%s]: %s\n"+
								"Hint: re-run with --agent to let the LLM agent propose a fix",
								verdict.Category, verdict.Detail)
						}
						// Tier 3: hand off to agent.
						agentCand, agentErr := runAgentTier(ctx, cmd, repoDir, candidate, mined, verdict)
						if agentErr != nil {
							return fmt.Errorf("agent tier failed: %w", agentErr)
						}
						candidate = agentCand
					}
				}
			}

			// ── Output ────────────────────────────────────────────────────────
			proposed := candidateToSandboxConfig(candidate)
			proposedYAML := renderSandboxBlock(proposed)

			w := cmd.OutOrStdout()

			_, _ = fmt.Fprintf(w, "=== design-sandbox proposal (tier: %s) ===\n\n", candidate.Tier)
			if candidate.Rationale != "" {
				_, _ = fmt.Fprintf(w, "Rationale: %s\n\n", candidate.Rationale)
			}
			if candidate.Verdict.Category != "" {
				_, _ = fmt.Fprintf(w, "Verify verdict: [%s] %s\n\n",
					candidate.Verdict.Category, candidate.Verdict.Detail)
			}
			_, _ = fmt.Fprintln(w, "Proposed sandbox block:")
			_, _ = fmt.Fprintln(w, proposedYAML)

			if !doWrite {
				_, _ = fmt.Fprintln(w,
					"(Pass --write to merge this into bugbot.yaml; unrelated keys are preserved.)")
				return nil
			}

			// ── --write: merge into existing bugbot.yaml ───────────────────────
			return mergeSandboxBlock(cmd, proposed)
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&enableAgent, "agent", false, "enable the LLM agent tier for edge-case resolution")
	cmd.Flags().BoolVar(&doVerify, "verify", true, "run the offline smoke-test verifier on the candidate")
	cmd.Flags().BoolVar(&doWrite, "write", false, "merge the proposed sandbox block into bugbot.yaml")

	return cmd
}

// synthesisSandbox is the pure deterministic tier: given detected facts and
// miner output, produce a candidateBlock with no LLM involvement.
func synthesisSandbox(facts primeFacts, mined minerOutput) candidateBlock {
	c := candidateBlock{
		Image:       facts.Image,
		DepStrategy: facts.DepStrategy,
		Network:     "none",
		CPUs:        defaultCPUs,
		MemoryMB:    defaultMemoryMB,
		TimeoutSecs: defaultTimeoutSecs,
		Tier:        "deterministic",
	}

	// If the miner found a more specific base image, prefer it only when prime
	// could not derive one (i.e. unknown build system → generic fallback).
	if c.Image == "" && mined.BaseImage != "" {
		c.Image = mined.BaseImage
	}

	// Synthesize setup_cmds from mined system deps. These run inside the
	// network=none sandbox, so apt-get update WON'T work. We emit only the
	// apt-get install argv; the operator must bake packages into the image or
	// accept that setup_cmds may fail offline. We annotate this in the output.
	//
	// Note: setup_cmds run with network=none. Packages emitted here MUST
	// already be cached in the image's APT layer or the command will fail.
	// For Alpine-based images we'd emit `apk add --no-network …` instead, but
	// we can't distinguish at this point; emit apt-get for Debian-derived images
	// and let the operator adjust.
	if len(mined.SystemDeps) > 0 {
		argv := make([]string, 0, 2+len(mined.SystemDeps))
		argv = append(argv, "apt-get", "install", "-y", "--no-install-recommends")
		argv = append(argv, mined.SystemDeps...)
		c.SetupCmds = [][]string{argv}
	}

	// LocalMounts: v1 only emits mounts when the miner can prove a local-path
	// dep exists (go.work workspace with a sibling module, etc.). We detect
	// go.work siblings conservatively — the full monorepo mount design lives
	// in bugbot-ixu. For now: emit nothing; the field stays nil.

	return c
}

// verifyCandidate runs the ftd.1 verifier against a candidate block.
func verifyCandidate(ctx context.Context, repoDir string, c candidateBlock) (repro.SmokeVerdict, error) {
	cfg := config.Default()
	cfg.Sandbox.Image = c.Image
	cfg.Sandbox.DepStrategy = c.DepStrategy
	cfg.Sandbox.Network = c.Network
	cfg.Sandbox.CPUs = c.CPUs
	cfg.Sandbox.MemoryMB = c.MemoryMB
	cfg.Sandbox.TimeoutSeconds = c.TimeoutSecs
	cfg.Sandbox.SetupCmds = c.SetupCmds
	cfg.Sandbox.LocalMounts = c.LocalMounts
	return repro.RunSandboxVerify(ctx, repoDir, cfg)
}

// runAgentTier runs the LLM agent tier: read-only tool loop, RunJSON against
// sandboxProposalSchema, then re-verifies the proposal in a bounded loop.
func runAgentTier(
	ctx context.Context,
	cmd *cobra.Command,
	repoDir string,
	initial candidateBlock,
	mined minerOutput,
	firstVerdict repro.SmokeVerdict,
) (candidateBlock, error) {
	// Load config for LLM role resolution.
	cfg, err := config.Load(configPathFromCmd(cmd))
	if err != nil {
		return initial, fmt.Errorf("load config: %w (is bugbot.yaml configured?)", err)
	}
	client, err := llm.ResolveRole(ctx, &cfg, "reproducer", llm.Options{})
	if err != nil {
		return initial, fmt.Errorf("resolve LLM role: %w", err)
	}

	tools, err := designSandboxReadOnlyTools(repoDir)
	if err != nil {
		return initial, fmt.Errorf("build agent tools: %w", err)
	}

	runner := agent.NewRunner(client, tools, designSandboxSystemPrompt(),
		agent.WithLimits(agent.Limits{MaxIterations: 20}))

	current := initial
	verdict := firstVerdict

	for i := 0; i < maxAgentIterations; i++ {
		task := buildAgentTask(current, mined, verdict)

		var proposal sandboxProposal
		_, err := runner.RunJSON(ctx, task, sandboxProposalSchema, &proposal)
		if err != nil {
			return current, fmt.Errorf("agent RunJSON (iter %d): %w", i+1, err)
		}

		// Apply proposal to candidate.
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

		newVerdict, verifyErr := verifyCandidate(ctx, repoDir, next)
		if verifyErr != nil {
			// Infrastructure error; stop and return whatever we have.
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"⚠  agent tier verify error (iter %d): %v\n", i+1, verifyErr)
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

	// Exhausted iterations without a passing verdict — return best effort.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"⚠  agent tier exhausted %d iterations; last verdict: [%s] %s\n",
		maxAgentIterations, verdict.Category, verdict.Detail)
	return current, nil
}

// designSandboxReadOnlyTools builds the read-only tool set for the designer agent,
// mirroring repro.readOnlyTools but rooted at the target repo.
func designSandboxReadOnlyTools(repoDir string) ([]agent.Tool, error) {
	read, err := agent.NewReadFile(repoDir)
	if err != nil {
		return nil, err
	}
	list, err := agent.NewListDir(repoDir)
	if err != nil {
		return nil, err
	}
	grep, err := agent.NewGrep(repoDir)
	if err != nil {
		return nil, err
	}
	return []agent.Tool{read, list, grep}, nil
}

// designSandboxSystemPrompt returns the system prompt for the designer agent.
func designSandboxSystemPrompt() string {
	return `You are a sandbox configuration specialist for the Bugbot code-analysis harness.
Your job is to propose a concrete sandbox: configuration block (image, dep_strategy,
setup_cmds, local_mounts) that makes the offline smoke test pass for the target repository.

Rules:
- PROPOSE CONFIG ONLY. Never suggest running commands on the host.
- The sandbox runs with network=none. setup_cmds must not require network access;
  any package installs must already be available in the image layer.
- Read the mined artifacts and smoke-test failure to understand what is missing.
- Prefer publicly-available Docker Hub images. Prefer slim/alpine variants.
- dep_strategy must be one of: "off", "host", "fetch".
- local_mounts should be empty unless you have strong evidence of a monorepo
  with local-path dependencies that must be bind-mounted.
- Your JSON answer must match the schema exactly.`
}

// buildAgentTask constructs the task prompt for the agent, including the smoke
// verdict, the current candidate, and the raw mined artifacts.
func buildAgentTask(c candidateBlock, mined minerOutput, verdict repro.SmokeVerdict) string {
	var sb strings.Builder
	sb.WriteString("The smoke-test verifier failed with:\n")
	fmt.Fprintf(&sb, "  category: %s\n  detail: %s\n\n", verdict.Category, verdict.Detail)

	sb.WriteString("Current candidate sandbox block:\n")
	fmt.Fprintf(&sb, "  image: %s\n  dep_strategy: %s\n  network: none\n",
		c.Image, c.DepStrategy)
	if len(c.SetupCmds) > 0 {
		fmt.Fprintf(&sb, "  setup_cmds: %v\n", c.SetupCmds)
	}
	sb.WriteString("\n")

	if len(mined.RawArtifacts) > 0 {
		sb.WriteString("Mined repository artifacts:\n")
		for label, content := range mined.RawArtifacts {
			// Truncate very large artifacts.
			if len(content) > 4000 {
				content = content[:4000] + "\n... (truncated)"
			}
			fmt.Fprintf(&sb, "\n--- %s ---\n%s\n", label, content)
		}
	}

	sb.WriteString("\nPropose a corrected sandbox configuration as JSON matching the schema.\n")
	return sb.String()
}

// candidateToSandboxConfig converts a candidateBlock to a config.Sandbox for
// YAML marshaling and merge.
func candidateToSandboxConfig(c candidateBlock) config.Sandbox {
	sb := config.Sandbox{
		Image:          c.Image,
		DepStrategy:    c.DepStrategy,
		Network:        c.Network,
		CPUs:           c.CPUs,
		MemoryMB:       c.MemoryMB,
		TimeoutSeconds: c.TimeoutSecs,
		SetupCmds:      c.SetupCmds,
		LocalMounts:    c.LocalMounts,
	}
	return sb
}

// renderSandboxBlock marshals a config.Sandbox to a YAML string under the
// `sandbox:` key, suitable for printing.
func renderSandboxBlock(sb config.Sandbox) string {
	type wrapper struct {
		Sandbox config.Sandbox `yaml:"sandbox"`
	}
	data, err := yaml.Marshal(wrapper{Sandbox: sb})
	if err != nil {
		return fmt.Sprintf("(marshal error: %v)", err)
	}
	return string(data)
}

// mergeSandboxBlock reads the existing bugbot.yaml, replaces (or adds) the
// sandbox: key with the proposed one, and writes the file back. It prints a
// unified diff of the change.
func mergeSandboxBlock(cmd *cobra.Command, proposed config.Sandbox) error {
	path := config.DefaultFileName

	// Read existing file (or start empty if absent).
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if os.IsNotExist(err) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"bugbot.yaml not found — creating it with the sandbox block only.\n"+
				"Run `bugbot init` first to get a fully-annotated starter file,\n"+
				"then re-run `bugbot design-sandbox --write` to merge the sandbox block.\n")
		// Write a minimal file with just the sandbox key.
		type minimalCfg struct {
			Sandbox config.Sandbox `yaml:"sandbox"`
		}
		out, merr := yaml.Marshal(minimalCfg{Sandbox: proposed})
		if merr != nil {
			return fmt.Errorf("marshal new config: %w", merr)
		}
		return os.WriteFile(path, out, 0o644)
	}

	// Unmarshal into a generic node tree so we can replace only sandbox:.
	var root yaml.Node
	if err := yaml.Unmarshal(existing, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	// Marshal proposed sandbox to a node.
	proposedData, err := yaml.Marshal(proposed)
	if err != nil {
		return fmt.Errorf("marshal proposed sandbox: %w", err)
	}
	var proposedNode yaml.Node
	if err := yaml.Unmarshal(proposedData, &proposedNode); err != nil {
		return fmt.Errorf("unmarshal proposed sandbox node: %w", err)
	}

	// Splice into the document node.
	merged, err := spliceYAMLKey(&root, "sandbox", &proposedNode)
	if err != nil {
		return fmt.Errorf("splice sandbox key: %w", err)
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal merged config: %w", err)
	}

	// Print diff before writing.
	diff := unifiedDiff(string(existing), string(out), path)
	if diff != "" {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), diff)
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no changes)")
		return nil
	}

	return os.WriteFile(path, out, 0o644)
}

// spliceYAMLKey sets (or adds) key in the YAML document root to val.
// root is a *yaml.Node from yaml.Unmarshal; val is the value node to splice in.
// Returns the modified root.
func spliceYAMLKey(root *yaml.Node, key string, val *yaml.Node) (*yaml.Node, error) {
	if root == nil {
		return nil, fmt.Errorf("nil root node")
	}

	// yaml.Unmarshal wraps in a DocumentNode.
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}

	// Ensure we have a mapping.
	if doc.Kind != yaml.MappingNode {
		// Wrap val as a new mapping.
		doc = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	// The value node from unmarshaling is a DocumentNode; unwrap.
	valNode := val
	if valNode.Kind == yaml.DocumentNode && len(valNode.Content) > 0 {
		valNode = valNode.Content[0]
	}

	// Walk pairs looking for an existing "sandbox" key.
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == key {
			doc.Content[i+1] = valNode
			// Rebuild document wrapper.
			out := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc}}
			return out, nil
		}
	}

	// Key not found — append it.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	doc.Content = append(doc.Content, keyNode, valNode)
	out := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc}}
	return out, nil
}

// unifiedDiff produces a simple line-by-line diff between before and after.
// This is a minimal implementation — we don't import the diff library; the
// output is readable enough for a configuration proposal.
func unifiedDiff(before, after, filename string) string {
	if before == after {
		return ""
	}
	blines := strings.Split(before, "\n")
	alines := strings.Split(after, "\n")

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--- %s (before)\n+++ %s (after)\n", filename, filename)

	// Emit a simple context-free diff: removed lines prefixed with -, added with +.
	// For a config merge this is sufficient readability without importing a full diff lib.
	bset := make(map[string]bool, len(blines))
	for _, l := range blines {
		bset[l] = true
	}
	aset := make(map[string]bool, len(alines))
	for _, l := range alines {
		aset[l] = true
	}

	for _, l := range blines {
		if !aset[l] {
			fmt.Fprintf(&buf, "- %s\n", l)
		}
	}
	for _, l := range alines {
		if !bset[l] {
			fmt.Fprintf(&buf, "+ %s\n", l)
		}
	}
	return buf.String()
}
