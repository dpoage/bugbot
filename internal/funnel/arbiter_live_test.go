//go:build arbiterlive

// Live-model proof for the agentic split-verdict arbiter (bugbot-mi5.17 AC4 /
// bugbot-mi5.19). Gated behind the `arbiterlive` build tag and a real
// MINIMAX_API_KEY so it never runs in CI and only makes paid API calls when an
// operator deliberately invokes it:
//
//	set -a; . ~/.config/bugbot/env; set +a
//	go test -tags arbiterlive ./internal/funnel/ -run TestArbiterLive_ReplaySplits -v -timeout 30m
//
// What it does: for each target finding published by a prior real scan of the
// controller repo (whose panel SPLIT and was rubber-stamped "survive" by the
// pre-mi5.17 one-shot arbiter), it reconstructs the EXACT original panel from
// the stored verification trace and re-runs ONLY the NEW arbiter against the
// real code with the real model. This isolates the arbiter change (refuters are
// unchanged, so replaying their recorded verdicts is faithful) and is
// deterministic w.r.t. the split (no dependence on refuters re-splitting).
//
// NOTE: mi5.18's dep-source read reach is Go-only (GOROOT/src + module cache).
// The controller repo is C++/shell/python, so the arbiter gets NO extra dep
// reach here — any flip is driven by the rewritten drive-to-ground prompt, the
// larger budget (50 iterations), and thorough in-repo grounding (find_references,
// reading the cited file and its callers). That is the honest scope of this test.
package funnel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// liveTarget names one published finding to replay, by file suffix + line, with
// the disposition we EXPECT the improved arbiter to reach.
type liveTarget struct {
	fileSuffix string
	line       int
	want       string // "refute" (false positive) | "keep" (legitimate) | "either" (trivial-true)
	note       string
}

// arbiterLiveTargets are the controller-repo split findings from bugbot-mi5.17's
// motivation. dac121c.cc:25 and can_battery_pack.h:25 from the bead are absent
// from this scan's state.db (a prior iteration), so they are omitted.
//
// EXPECTATIONS CORRECTED 2026-06-24 by the bugbot-mi5.21 human adjudication:
// mi5.17 AC4 assumed all three FP-labeled splits should flip to refuted. That
// premise was falsified for speed_config (a REAL bug), so its want is now "keep".
var arbiterLiveTargets = []liveTarget{
	{"common/speed_config.cc", 29, "keep", "REAL bug: header documents throw-on-missing-fields but NLOHMANN_..._WITH_DEFAULT silently defaults missing keys (mi5.21)"},
	{"tools/remote_provision.sh", 17, "refute", "FP: set -e is NOT exempt in an if-then body (verified empirically); jq-missing exits, keys.env-missing caught by the tailscale guard (mi5.21)"},
	{"peripherals/pwm_pin.cc", 21, "refute", "FP: null GpioInterfacePtr unreachable — every caller passes make_shared/non-null (mi5.21)"},
	{"control/wheel_jog_controller.cc", 31, "keep", "LEGIT bug: uninitialized virtual PanicHandler base => watchdog OnPanic no-op; must NOT be demoted"},
	{"setup/encoder_tool/encoder_tool.py", 49, "either", "true-but-trivial fd leak in a short-lived CLI"},
}

func TestArbiterLive_ReplaySplits(t *testing.T) {
	if os.Getenv("MINIMAX_API_KEY") == "" {
		t.Skip("arbiterlive: set MINIMAX_API_KEY (source ~/.config/bugbot/env) to run the live arbiter replay")
	}
	ctx := context.Background()
	home, _ := os.UserHomeDir()
	repoPath := envOr("BUGBOT_LIVE_REPO", filepath.Join(home, "code", "controller"))
	cfgPath := envOr("BUGBOT_LIVE_CONFIG", filepath.Join(repoPath, "bugbot.yaml"))
	dbPath := envOr("BUGBOT_LIVE_DB", filepath.Join(repoPath, ".bugbot", "state.db"))
	transcriptDir := envOr("BUGBOT_LIVE_TRANSCRIPTS", filepath.Join(os.TempDir(), "arbiter-live-transcripts"))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config %s: %v", cfgPath, err)
	}
	verifier, err := config.ResolveRole(ctx, &cfg, "verifier", llm.Options{})
	if err != nil {
		t.Fatalf("resolve verifier client: %v", err)
	}
	repo, err := ingest.Open(ctx, repoPath)
	if err != nil {
		t.Fatalf("open repo %s: %v", repoPath, err)
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store %s: %v", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	f, err := New(RoleClients{Finder: verifier, Verifier: verifier}, st, repo, Options{TranscriptDir: transcriptDir})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	t.Logf("arbiter dep-source roots discovered (Go-only): %v", f.depRoots.Roots())
	t.Logf("transcripts -> %s", transcriptDir)

	open, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}

	arbiterTools, err := f.readOnlyToolsWithDepRoots(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("build arbiter tools: %v", err)
	}

	type outcome struct {
		tgt      liveTarget
		refuted  bool
		conf     string
		evidence []string
		reason   string
		tokens   int64
		stopped  bool
	}
	var results []outcome

	for _, tgt := range arbiterLiveTargets {
		if filter := os.Getenv("BUGBOT_LIVE_TARGET"); filter != "" && !strings.Contains(tgt.fileSuffix, filter) {
			continue // bounded-spend: replay only the matching target(s)
		}
		fnd, ok := findTarget(open, tgt)
		if !ok {
			t.Errorf("target not found in store: %s:%d", tgt.fileSuffix, tgt.line)
			continue
		}
		verdicts, seatNames := parseTraceVerdicts(fnd.Reasoning)
		if len(verdicts) < 2 {
			t.Errorf("%s:%d: parsed %d seat verdicts from trace, want >=2 (split) — trace:\n%s",
				tgt.fileSuffix, tgt.line, len(verdicts), fnd.Reasoning)
			continue
		}
		c := candidateFromFinding(fnd)
		persona := ingest.PersonaLanguages([]ingest.Language{ingest.DetectLanguage(fnd.File)}, nil)
		scope := progress.NewAgentScope(nil, progress.RoleVerifier, c.Title)
		av, tokens, stopped, aerr := f.runArbiter(ctx, verifier, arbiterTools, persona, c, verdicts, seatNames, &budgetState{}, scope)
		if aerr != nil {
			t.Errorf("%s:%d: runArbiter error: %v", tgt.fileSuffix, tgt.line, aerr)
			continue
		}
		o := outcome{tgt: tgt, tokens: tokens, stopped: stopped}
		if av != nil {
			o.refuted, o.conf, o.evidence, o.reason = av.Refuted, av.Confidence, av.Evidence, av.Reasoning
		}
		results = append(results, o)

		t.Logf("\n=== %s:%d (%s) ===\n  want=%s  arbiter=%s confidence=%s stopped=%v tokens=%d\n  evidence=%v\n  reasoning=%s\n  seats=%v",
			tgt.fileSuffix, tgt.line, tgt.note, tgt.want, verdictWord(av), o.conf, stopped, tokens, o.evidence, truncate(o.reason, 600), seatNames)

		// Hard regression guard: the legitimate bug must NEVER be demoted.
		if tgt.want == "keep" && o.refuted {
			t.Errorf("REGRESSION: arbiter REFUTED the legitimate finding %s:%d — must be kept", tgt.fileSuffix, tgt.line)
		}
		// bugbot-mi5.20 AC3: arbiterTools here is read-only (no sandbox_exec), so
		// any claim of an executed command/probe in the verdict is fabricated.
		if !hasSandboxExec(arbiterTools) {
			if hint := fabricatedProbeHint(o.evidence, o.reason); hint != "" {
				t.Errorf("%s:%d: FABRICATED PROBE in arbiter evidence with no exec tool wired (marker %q) — bugbot-mi5.20", tgt.fileSuffix, tgt.line, hint)
			}
		}
	}

	// Summary.
	var flipped, fpTotal int
	var b strings.Builder
	b.WriteString("\n================ ARBITER LIVE REPLAY SUMMARY ================\n")
	for _, o := range results {
		disp := "KEPT"
		if o.refuted {
			disp = "REFUTED"
		}
		ok := "—"
		switch o.tgt.want {
		case "refute":
			fpTotal++
			if o.refuted {
				flipped++
				ok = "✓ flipped"
			} else {
				ok = "✗ still kept"
			}
		case "keep":
			if o.refuted {
				ok = "✗ DEMOTED (regression)"
			} else {
				ok = "✓ preserved"
			}
		}
		fmt.Fprintf(&b, "  %-38s want=%-7s got=%-8s %s\n", fmt.Sprintf("%s:%d", o.tgt.fileSuffix, o.tgt.line), o.tgt.want, disp, ok)
	}
	fmt.Fprintf(&b, "  FALSE-POSITIVE FLIP RATE: %d/%d\n", flipped, fpTotal)
	b.WriteString("============================================================\n")
	t.Log(b.String())
}

// fabricatedProbeHint returns a non-empty marker when the arbiter's evidence or
// reasoning claims it EXECUTED a command/probe. With no exec tool wired such a
// claim is fabricated (bugbot-mi5.20). Markers are execution CLAIMS, not mere
// code quotes, to keep false positives low.
func fabricatedProbeHint(evidence []string, reasoning string) string {
	hay := strings.ToLower(strings.Join(append(append([]string{}, evidence...), reasoning), "\n"))
	markers := []string{"bash -c", "i ran ", "i executed", "ran the command", "ran the probe", "running the probe", "the probe prints", "probe confirms", "the command output", "when i run", "executing the"}
	for _, m := range markers {
		if strings.Contains(hay, m) {
			return m
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func findTarget(findings []domain.Finding, tgt liveTarget) (domain.Finding, bool) {
	for _, fnd := range findings {
		if fnd.Line == tgt.line && strings.HasSuffix(fnd.File, tgt.fileSuffix) {
			return fnd, true
		}
	}
	return domain.Finding{}, false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// traceHeaderRE matches a per-seat refuter line in buildReasoning's trace:
//
//	"  refuter 1 [reachability, could not refute, confidence=high]: <reasoning>"
//	"  refuter 1 [could not refute, confidence=high]: <reasoning>"   (n==1, no seat)
var traceHeaderRE = regexp.MustCompile(`^\s*refuter\s+\d+\s+\[(.+?)\]:\s?(.*)$`)

// arbiterHeaderRE matches the appended arbiter line, which we stop at (we are
// re-running the arbiter, so its old verdict is not part of the panel).
var arbiterHeaderRE = regexp.MustCompile(`^\s*arbiter\s+\[`)

// parseTraceVerdicts reconstructs the per-seat []refutation and seat names from
// a stored verification trace (buildReasoning's output). Continuation lines
// (model reasoning that contained newlines) are appended to the current seat's
// reasoning until the next refuter/arbiter header.
func parseTraceVerdicts(trace string) ([]refutation, []string) {
	var verdicts []refutation
	var seats []string
	var cur *refutation
	flush := func() {
		if cur != nil {
			cur.Reasoning = strings.TrimSpace(cur.Reasoning)
			verdicts = append(verdicts, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(trace, "\n") {
		if arbiterHeaderRE.MatchString(line) {
			break // panel ends; the rest is the old arbiter verdict
		}
		m := traceHeaderRE.FindStringSubmatch(line)
		if m == nil {
			if cur != nil {
				cur.Reasoning += "\n" + line
			}
			continue
		}
		flush()
		seat, r := parseSeatBracket(m[1])
		r.Reasoning = m[2]
		seats = append(seats, seat)
		cur = &r
	}
	flush()
	return verdicts, seats
}

// parseSeatBracket parses the bracket body of a trace header into a seat name
// (empty for the n==1 unlabeled form) and a refutation with the verdict +
// confidence set. Bracket forms:
//
//	"reachability, could not refute, confidence=high"  -> seat + verdict + conf
//	"could not refute, confidence=high"                -> verdict + conf (no seat)
func parseSeatBracket(body string) (string, refutation) {
	parts := strings.Split(body, ", ")
	var seat, verdict, conf string
	switch {
	case len(parts) >= 3:
		seat, verdict = parts[0], parts[1]
		conf = strings.TrimPrefix(parts[2], "confidence=")
	case len(parts) == 2:
		verdict = parts[0]
		conf = strings.TrimPrefix(parts[1], "confidence=")
	default:
		verdict = body
	}
	r := refutation{Confidence: conf}
	switch {
	case verdict == "refuted":
		r.Refuted = true
	case strings.HasPrefix(verdict, "abstained"):
		r.CouldNotReadCode = true
	case strings.HasPrefix(verdict, "no verdict"):
		r.NoVerdict = true
		// "could not refute" -> Refuted stays false (a genuine survive vote)
	}
	return seat, r
}
