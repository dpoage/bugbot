//go:build record

// Real-model corpus recorder for the eval harness.
//
// This is gated behind the `record` build tag AND the LLM_LIVE_* environment
// variables, mirroring internal/llm/live_test.go: it never runs in normal
// `go test ./...`, never needs a key in CI, and makes real (paid) API calls
// only when an operator deliberately runs it with credentials. The orchestrator
// runs it with a real MiniMax M3 key:
//
//	LLM_LIVE_BASE_URL=https://api.minimax.io/v1 \
//	LLM_LIVE_MODEL=MiniMax-M3 \
//	LLM_LIVE_API_KEY=sk-... \
//	go test -tags record ./internal/eval/ -run TestRecordCorpus -v
//
// For each of three builtin cases it runs the REAL funnel against the case
// fixture with real finder+verifier clients, capturing every agent run's
// transcript to a temp TranscriptDir. It then splits the saved transcripts into
// finder/verifier sessions (by inspecting each one's first user message), scores
// the live run against the case's seeded ground truth, and writes the corpus to
// internal/eval/testdata/recorded/<case>/ as finder-NNN.jsonl,
// verifier-NNN.jsonl, and manifest.json. The committed corpus then drives the
// no-tags determinism replay test and `bugbot eval --recorded`.
package eval

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/llm"
)

// recordEnv holds the resolved recorder configuration.
type recordEnv struct {
	baseURL string
	model   string
	apiKey  string
}

// recordAPIKeyEnvVar names the env var holding the live key. It is wired into a
// synthetic config.Provider so NewClient's production construction path resolves
// the key exactly as ResolveRole would in production (see live_test.go).
const recordAPIKeyEnvVar = "LLM_LIVE_API_KEY"

// requireRecordEnv reads the recorder environment and skips when any required
// variable is absent, exactly like the live probe.
func requireRecordEnv(t *testing.T) recordEnv {
	t.Helper()
	env := recordEnv{
		baseURL: os.Getenv("LLM_LIVE_BASE_URL"),
		model:   os.Getenv("LLM_LIVE_MODEL"),
		apiKey:  os.Getenv(recordAPIKeyEnvVar),
	}
	var missing []string
	if env.baseURL == "" {
		missing = append(missing, "LLM_LIVE_BASE_URL")
	}
	if env.model == "" {
		missing = append(missing, "LLM_LIVE_MODEL")
	}
	if env.apiKey == "" {
		missing = append(missing, recordAPIKeyEnvVar)
	}
	if len(missing) > 0 {
		t.Skipf("corpus recorder skipped: set %s (and run with -tags record) to record a real-model corpus",
			strings.Join(missing, ", "))
	}
	return env
}

// newRecordClient builds a Client the way production does: a config.Provider of
// type openai-compatible whose APIKeyEnv names the live key var, resolved
// through config.Config.APIKey and handed to llm.NewClient. role tags the
// emitted UsageEvents. This is the same construction path as live_test.go.
func newRecordClient(t *testing.T, env recordEnv, role string) llm.Client {
	t.Helper()
	provider := config.Provider{
		Type:      config.ProviderOpenAICompatible,
		BaseURL:   env.baseURL,
		APIKeyEnv: recordAPIKeyEnvVar,
	}
	cfg := &config.Config{Providers: map[string]config.Provider{"live": provider}}
	apiKey, err := cfg.APIKey("live")
	if err != nil {
		t.Fatalf("resolve live api key: %v", err)
	}
	client, err := llm.NewClient(context.Background(), provider, "live", env.model, apiKey, llm.Options{Role: role})
	if err != nil {
		t.Fatalf("build %s client: %v", role, err)
	}
	return client
}

// recordedCorpusDir is the on-disk target for committed recordings.
const recordedCorpusDir = DefaultRecordedDir

// recorderCases returns the three builtin cases this recorder captures: the
// nil-deref seeded case, the resource-leak seeded case, and the clean-code FP
// canary. They span the precision/recall surface (two real bugs, one clean).
func recorderCases(t *testing.T) []Case {
	t.Helper()
	want := []string{"nil-deref-seeded", "resource-leak-seeded", "clean-code"}
	var out []Case
	for _, name := range want {
		c, ok := builtinCaseByName(name)
		if !ok {
			t.Fatalf("recorder: builtin case %q not found", name)
		}
		out = append(out, c)
	}
	return out
}

// TestRecordCorpus records the real-model transcript corpus for the three
// chosen builtin cases. It is skipped unless run with -tags record AND the
// LLM_LIVE_* vars are set.
func TestRecordCorpus(t *testing.T) {
	requireGit(t)
	env := requireRecordEnv(t)

	host := hostOf(env.baseURL)

	for _, base := range recorderCases(t) {
		base := base
		t.Run(base.Name, func(t *testing.T) {
			// A real run can wander; bound it generously but firmly. The whole
			// recorder is operator-initiated and paid, so a per-run wall clock plus
			// modest per-agent limits keep a misbehaving model from burning the
			// budget unbounded.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()

			transcriptDir := filepath.Join(t.TempDir(), "transcripts")

			// Modest per-agent limits: MaxIterations bounds the tool loop, TokenBudget
			// caps each runner's spend so a wandering model can't run away.
			limits := agent.Limits{MaxIterations: 12, TokenBudget: 200_000}

			c := base
			c.Scripted = nil
			c.Recorded = nil
			c.Options = funnel.Options{
				TranscriptDir:  transcriptDir,
				MaxParallel:    1, // REQUIRED for deterministic replay ordering
				FinderLimits:   limits,
				VerifierLimits: limits,
			}

			finderClient := newRecordClient(t, env, "finder")
			verifierClient := newRecordClient(t, env, "verifier")
			clients := funnel.RoleClients{Finder: finderClient, Verifier: verifierClient}

			// Drive the EXACT pipeline path RunCase uses, with real clients injected.
			result, err := runWithClients(ctx, c, clients)
			if err != nil {
				t.Fatalf("live funnel run for %q: %v", base.Name, err)
			}

			// Load the saved transcripts (filename = chronological order) and split
			// them into finder/verifier sessions by inspecting each first user
			// message. Unclassifiable transcripts are a hard error.
			names, trs, err := loadTranscriptDir(transcriptDir)
			if err != nil {
				t.Fatalf("load saved transcripts for %q: %v", base.Name, err)
			}
			if len(trs) == 0 {
				t.Fatalf("no transcripts were saved for %q; expected at least one finder run", base.Name)
			}
			finderSessions, verifierSessions, err := splitByRole(names, trs)
			if err != nil {
				t.Fatalf("split transcripts for %q: %v", base.Name, err)
			}

			caseDir := filepath.Join(recordedCorpusDir, base.Name)
			if err := writeCorpus(caseDir, finderSessions, verifierSessions); err != nil {
				t.Fatalf("write corpus for %q: %v", base.Name, err)
			}

			manifest := RecordedManifest{
				Case:        base.Name,
				Model:       env.model,
				BaseURLHost: host,
				RecordedAt:  time.Now().UTC().Format(time.RFC3339),
				Scores: RecordedScores{
					TruePositives:  result.TruePositives,
					FalsePositives: result.FalsePositives,
					FalseNegatives: result.FalseNegatives,
					Precision:      result.Precision(),
					Recall:         result.Recall(),
					Stats:          result.Stats,
				},
				FinderSessions:   len(finderSessions),
				VerifierSessions: len(verifierSessions),
			}
			if err := writeManifest(caseDir, manifest); err != nil {
				t.Fatalf("write manifest for %q: %v", base.Name, err)
			}

			// Per-case summary line.
			t.Logf("recorded %q: tp=%d fp=%d fn=%d precision=%.3f recall=%.3f | finder-sessions=%d verifier-sessions=%d | model=%s host=%s",
				base.Name,
				result.TruePositives, result.FalsePositives, result.FalseNegatives,
				result.Precision(), result.Recall(),
				len(finderSessions), len(verifierSessions),
				env.model, host,
			)
		})
	}
}

// hostOf returns the host of a base URL, never the credentials or path. Used so
// the manifest records WHERE the corpus came from without leaking the key.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw // best effort; never a key, base URLs carry no secret
	}
	return u.Host
}

// writeCorpus writes the finder/verifier sessions as zero-padded, ordered JSONL
// files into caseDir, replacing any prior recordings for the case so a re-record
// never mixes stale sessions with fresh ones.
func writeCorpus(caseDir string, finder, verifier []*agent.Transcript) error {
	if err := os.RemoveAll(caseDir); err != nil {
		return fmt.Errorf("clear case dir %q: %w", caseDir, err)
	}
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", caseDir, err)
	}
	if err := writeSessions(caseDir, "finder", finder); err != nil {
		return err
	}
	return writeSessions(caseDir, "verifier", verifier)
}

// writeSessions writes one role's sessions as "<role>-NNN.jsonl", zero-padded so
// lexical order is record order.
func writeSessions(caseDir, role string, sessions []*agent.Transcript) error {
	for i, tr := range sessions {
		name := fmt.Sprintf("%s-%03d.jsonl", role, i)
		f, err := os.Create(filepath.Join(caseDir, name))
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if err := tr.SaveJSONL(f); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
	}
	return nil
}
