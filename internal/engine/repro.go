package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/daemon"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// ReproDeps bundles a constructed reproducer with its LLM client and spend
// tagger. Exported (unlike the pre-refactor cli.reproDeps) because
// internal/cli/daemon.go builds the daemon's reproducer directly through
// BuildReproducer rather than through a Dispatcher verb — the daemon's own
// polling loop is out of scope for this refactor and keeps wiring its own
// internal/daemon.Deps.
type ReproDeps struct {
	Client llm.Client
	Repro  *repro.Reproducer
	// Sb backs the reproducer; callers Close it alongside Repro.Close to release
	// the pristine-workspace cache (internal/sandbox wsCache) when the
	// reproducer's scope ends.
	Sb *sandbox.CLI
	// Spend ledgers reproducer/patch-prover usage; callers retag it with each
	// cycle's/run's scan-run id via Spend.SetScanRun.
	Spend *ledgerRecorder
}

// BuildReproducer constructs the reproducer-role LLM client, sandbox, and
// Reproducer shared by `scan --repro`'s in-run hook, `bugbot repro`'s backlog
// drain, and the daemon's post-cycle promotion step.
func BuildReproducer(ctx context.Context, cfg *config.Config, st *store.Store, repoRoot, runtime string, prog progress.EventSink) (*ReproDeps, error) {
	// Ledger repro + patch-prover spend (bugbot-58c). Callers retag the
	// recorder with each run's/cycle's scan-run id.
	rec := newLedgerRecorder(ctx, st)
	client, err := config.ResolveRole(ctx, cfg, "reproducer", llm.Options{Recorder: rec})
	if err != nil {
		return nil, fmt.Errorf("build reproducer client: %w", err)
	}
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(*cfg)...)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}
	// Probe image capabilities once; result is cached per image so repeated
	// daemon restarts or re-calls to BuildReproducer are free.
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, repoRoot)
	r, err := repro.New(client, sb, repoRoot, repro.Options{
		MaxAttempts:      cfg.Repro.MaxAttempts,
		Image:            cfg.Sandbox.Image,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(*cfg),
		Capabilities:     caps,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    cfg.Repro.TranscriptDir,
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
		MaxParallel:      cfg.Repro.MaxParallel,
	})
	if err != nil {
		return nil, fmt.Errorf("build reproducer: %w", err)
	}
	return &ReproDeps{Client: client, Repro: r, Sb: sb, Spend: rec}, nil
}

// buildReproHookForScan constructs the in-run reproducer hook when --repro is
// requested. It returns the hook closure and the associated reproducer state
// needed by the post-scan catch-up drain. When DoRepro is false or no
// container runtime is available the hook is nil and the other return values
// are zero.
func buildReproHookForScan(
	ctx context.Context,
	out io.Writer,
	cfg config.Config,
	st *store.Store,
	opts ScanOpts,
	prog progress.EventSink,
) (
	hook func(ctx context.Context, scanRunID string, finding domain.Finding) error,
	rec *ledgerRecorder,
	r *repro.Reproducer,
	attempted *sync.Map,
	err error,
) {
	attempted = &sync.Map{}
	if !opts.DoRepro || opts.DoEstimate {
		return nil, nil, nil, attempted, nil
	}
	runtime, rtOK := sandbox.Detect()
	if !rtOK {
		_, _ = fmt.Fprintln(out, "Reproduce stage skipped: no container runtime (podman/docker) found on PATH.")
		// hook stays nil so the catch-up drain prints a note; DoRepro check in
		// the caller still runs (with r == nil) so no catch-up is attempted.
		return nil, nil, nil, attempted, nil
	}
	sb, sbErr := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
	if sbErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build sandbox: %w", sbErr)
	}
	// Preflight: probe the sandbox toolchain once per process before burning
	// per-finding repro attempts on an image that cannot run the target
	// ecosystem (bugbot-u6td). A probe infrastructure error does not gate:
	// repro attempts remain meaningful evidence when the probe itself failed.
	if verdict, vErr := repro.VerifySandboxOnce(ctx, opts.Target, cfg); vErr == nil && verdict.BlocksRepro() {
		_, _ = fmt.Fprintf(out,
			"Reproduce stage skipped: sandbox toolchain check failed (%s): %s\n  Run `bugbot doctor` and set sandbox.image to a toolchain-capable image.\n",
			verdict.Category, verdict.Detail)
		return nil, nil, nil, attempted, nil
	}
	// Ledger repro + patch-prover spend; the scan run id is pinned by the
	// hook on first use (the funnel supplies it), and again after the sweep
	// for the catch-up drain.
	rec = newLedgerRecorder(ctx, st)
	reproClient, rErr := config.ResolveRole(ctx, &cfg, "reproducer", llm.Options{Recorder: rec})
	if rErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build reproducer client: %w", rErr)
	}
	// Probe image capabilities once; result is cached per image so
	// subsequent daemon cycles and parallel scan runs are free.
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, opts.Target)
	r, rNewErr := repro.New(reproClient, sb, opts.Target, repro.Options{
		MaxAttempts:      cfg.Repro.MaxAttempts,
		Image:            cfg.Sandbox.Image,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(cfg),
		Capabilities:     caps,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    cfg.Repro.TranscriptDir,
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
		MaxParallel:      cfg.Repro.MaxParallel,
	})
	if rNewErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build reproducer: %w", rNewErr)
	}
	if r == nil {
		return nil, nil, nil, attempted, nil
	}
	// Hook: called in-run for each Tier-2 finding. Uses PromoteOne
	// (one finding = one hook call = one idle slot; the funnel's
	// consumer goroutine is the parallelism bound). The hook calls
	// PromoteOne which calls Attempt internally.
	var runOnce sync.Once
	hook = func(hCtx context.Context, scanRunID string, finding domain.Finding) error {
		runOnce.Do(func() { rec.SetScanRun(scanRunID) })
		attempted.Store(finding.Fingerprint, true)
		_, hErr := r.PromoteOne(hCtx, st, finding)
		return hErr
	}
	return hook, rec, r, attempted, nil
}

// runReproCatchUp runs a backlog-style drain over the run's Tier-2 findings
// that have no prior repro attempt (ReproPath empty, NeedsHuman false). This
// is a cheap no-op when the in-run hook covered everything; it acts as a
// safety net for findings that overflowed the reproCh buffer. It uses
// PromoteAll (the daemon's batch path) so the rotation logic (touch failed
// findings) also runs.
func runReproCatchUp(ctx context.Context, out io.Writer, r *repro.Reproducer, st *store.Store, findings []domain.Finding, attempted *sync.Map) error {
	// Filter to T2 findings with no prior attempt. "Prior attempt" includes
	// in-run attempts that EXHAUSTED: a failed repro leaves no store-visible
	// marker (ReproPath stays empty, NeedsHuman stays false), so without the
	// attempted set this drain would re-burn sandbox time on exactly the
	// findings the in-run hook just failed on.
	var pending []domain.Finding
	for _, f := range findings {
		if f.Tier != domain.TierVerified || f.ReproPath != "" || f.NeedsHuman {
			continue
		}
		if attempted != nil {
			if _, ok := attempted.Load(f.Fingerprint); ok {
				continue
			}
		}
		// Re-read from store to get the latest state (in-run hook may have promoted it).
		current, err := st.GetFinding(ctx, f.ID)
		if err != nil {
			continue // best-effort
		}
		if current.ReproPath == "" && !current.NeedsHuman {
			pending = append(pending, current)
		}
	}
	if len(pending) == 0 {
		return nil // no-op when in-run hook covered all findings
	}

	_, _ = fmt.Fprintf(out, "\nReproduce catch-up: %d finding(s) not yet attempted...\n", len(pending))
	summary, err := r.PromoteAll(ctx, st, pending)
	if err != nil {
		return fmt.Errorf("reproduce catch-up: %w", err)
	}
	printReproSummary(out, summary)
	return nil
}

// printReproSummary renders the promotion outcome. Shared by the scan
// catch-up drain and the one-shot `bugbot repro` backlog drain.
func printReproSummary(out io.Writer, s *repro.Summary) {
	_, _ = fmt.Fprintf(out, "Reproduced: %d promoted to T1, %d not reproduced (of %d attempted)\n",
		s.Promoted, s.Failed, s.Attempted)
	if s.FixWitnessed > 0 || s.NeedsHuman > 0 {
		_, _ = fmt.Fprintf(out, "Patch-prover: %d fix-witnessed (T0), %d needs-human\n",
			s.FixWitnessed, s.NeedsHuman)
	}
	for _, o := range s.PerFinding {
		if o.FixWitnessed {
			_, _ = fmt.Fprintf(out, "  [T0] %s -> fix witnessed\n", o.Title)
		} else if o.Promoted {
			_, _ = fmt.Fprintf(out, "  [T1] %s -> %s\n", o.Title, o.ArtifactPath)
		} else {
			reason := o.Reason
			if reason == "" {
				reason = "not demonstrated"
			}
			_, _ = fmt.Fprintf(out, "  [T2] %s (%s)\n", o.Title, reason)
		}
		if o.NeedsHuman {
			_, _ = fmt.Fprintf(out, "       (patch-prover: needs human review)\n")
		}
	}
}

// ReproOpts holds the parsed flag values for `bugbot repro`.
type ReproOpts struct {
	Target        string
	MaxN          int
	TranscriptDir string
	Out           io.Writer
	// StopProgress, if non-nil, is called right before the summary prints so a
	// CLI-owned live pane can clear its in-place status lines first. Safe to
	// call multiple times.
	StopProgress  func()
}

// ReproResult is the outcome of a Dispatcher.Repro call.
type ReproResult struct {
	// Summary is nil when the backlog was empty or no container runtime was
	// available (both graceful no-ops; check Skipped for the reason).
	Summary *repro.Summary
	Skipped string
}

// Repro implements `bugbot repro`'s one-shot backlog drain: it queries the
// store for open Tier-2/3 findings with no reproduction attempt and runs them
// through the reproduce+patch-prover pipeline, promoting demonstrated
// findings to Tier-1 (or Tier-0 when the patch-prover witnesses a fix). This
// is the same backlog logic the daemon runs on its periodic backlog timer.
func (d *Dispatcher) Repro(ctx context.Context, opts ReproOpts) (*ReproResult, error) {
	// main's `bugbot repro` had no advisory-lock gate at all — it opened the
	// store (flock) and proceeded unconditionally. force=true here is the
	// faithful translation: checkScanLock's heuristic never refuses, so a
	// fresh Observer (heartbeat-only, flock free after a crash) escalates and
	// proceeds exactly like main; a genuinely live writer still refuses via
	// escalateToOwner's store.Open ErrLocked, matching main's ErrLocked too.
	if err := d.ensureOwner(ctx, true); err != nil {
		return nil, err
	}
	cfg := d.cfg
	st := d.store
	out := opts.Out

	// --max overrides the config default; 0 means "use config".
	batchSize := cfg.Repro.BacklogBatch
	if opts.MaxN > 0 {
		batchSize = opts.MaxN
	}

	// --transcript-dir overrides repro.transcript_dir from config. When set,
	// every reproducer agent's JSONL transcript is auto-saved there (one file
	// per finding per attempt), independent of target language — the seam
	// for diagnosing why a finding did or did not reproduce.
	if opts.TranscriptDir != "" {
		cfg.Repro.TranscriptDir = opts.TranscriptDir
	}

	runtime, ok := sandbox.Detect()
	if !ok {
		_, _ = fmt.Fprintln(out, "Repro backlog skipped: no container runtime (podman/docker) found on PATH.")
		return &ReproResult{Skipped: "no container runtime"}, nil
	}

	backlog, err := daemon.OpenBacklog(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("query backlog: %w", err)
	}
	if len(backlog) == 0 {
		_, _ = fmt.Fprintln(out, "Repro backlog: no eligible findings.")
		return &ReproResult{Skipped: "no eligible findings"}, nil
	}

	// Preflight: reproduction is this command's entire purpose, so a sandbox
	// image that cannot run the target ecosystem is a hard error, not a
	// silent per-finding environment_error burn (bugbot-u6td). Runs after the
	// empty-backlog exit: no work means no probe.
	if verdict, vErr := repro.VerifySandboxOnce(ctx, opts.Target, cfg); vErr == nil && verdict.BlocksRepro() {
		return nil, fmt.Errorf(
			"sandbox toolchain check failed (%s): %s — run `bugbot doctor` and set sandbox.image to a toolchain-capable image",
			verdict.Category, verdict.Detail)
	}

	batch := backlog
	if len(batch) > batchSize {
		batch = batch[:batchSize]
	}

	// Build the reproducer using the same helper the daemon command uses.
	// Ledger spend with an empty scan-run id: backlog findings span multiple
	// past runs, so there is no single run to attribute to. This matches the
	// daemon's backlog attribution choice.
	// d.sink is the CLI-owned live pane/log renderer (see cli/repro.go): it
	// surfaces repro attempts as they happen but is NOT a SnapshotSink, so it
	// never races a running daemon's single-writer status.json.
	rd, err := BuildReproducer(ctx, &cfg, st, opts.Target, runtime, d.sink)
	if err != nil {
		return nil, err
	}
	defer rd.Repro.Close() //nolint:errcheck
	defer func() { _ = rd.Sb.Close() }()

	_, _ = fmt.Fprintf(out,
		"\nRepro backlog: %d eligible, attempting %d (max=%d, runtime=%s)...\n",
		len(backlog), len(batch), batchSize, runtime,
	)
	if cfg.Repro.TranscriptDir != "" {
		_, _ = fmt.Fprintf(out, "Transcripts: %s\n", cfg.Repro.TranscriptDir)
	}

	summary, err := rd.Repro.PromoteAll(ctx, st, batch)
	if err != nil {
		return nil, fmt.Errorf("reproduce: %w", err)
	}
	if opts.StopProgress != nil {
		opts.StopProgress()
	}
	printReproSummary(out, summary)

	// Touch attempted-but-not-promoted findings to bump updated_at so that
	// OpenBacklog's oldest-first ordering rotates them to the back of the
	// queue on the next run, preventing unbounded retries on the same
	// unreproducible findings.
	daemon.TouchBacklogFailures(ctx, st, slog.Default(), batch)
	return &ReproResult{Summary: summary}, nil
}
