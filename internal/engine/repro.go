package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/daemon"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
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
	Sb sandbox.Sandbox
	// Spend ledgers reproducer/patch-prover usage; callers retag it with each
	// cycle's/run's scan-run id via Spend.SetScanRun.
	Spend *ledgerRecorder
}

// reproTranscriptDir resolves the reproducer/patch-prover transcript
// directory: cfg.Repro.TranscriptDir when explicitly set (it predates the
// general cfg.TranscriptDir and is honored first so an operator's existing
// per-finding artifact placement keeps working unchanged), falling back to
// cfg.TranscriptDir so reproducer/patch-prover transcripts are captured by
// default like every other agent role, without requiring separate config.
func reproTranscriptDir(cfg config.Config) string {
	if cfg.Repro.TranscriptDir != "" {
		return cfg.Repro.TranscriptDir
	}
	return cfg.TranscriptDir
}

// BuildReproducer constructs the reproducer-role LLM client, sandbox, and
// Reproducer shared by `scan --repro`'s in-run hook, `bugbot repro`'s backlog
// drain, and the daemon's post-cycle promotion step.
func BuildReproducer(ctx context.Context, cfg *config.Config, st *store.Store, repoRoot string, prog progress.EventSink) (*ReproDeps, error) {
	// Ledger repro + patch-prover spend (bugbot-58c). Callers retag the
	// recorder with each run's/cycle's scan-run id.
	rec := newLedgerRecorder(ctx, st)
	// Surface repro spend live: without this the pane/status token counters
	// freeze while reproducer/patch-prover agents run — the ledgerRecorder
	// only wrote the store ledger (bugbot-psva). Role tags the tick's stream
	// so consumers sum it with the funnel's own ticks instead of clobbering.
	if prog != nil {
		rec.onRecord = func(in, out, cached int64) {
			progress.Emit(prog, progress.Event{
				Kind: progress.KindSpendTick, Role: progress.RoleReproducer,
				InputTokens: in, OutputTokens: out, CacheReadTokens: cached,
			})
		}
	}
	client, err := config.ResolveRole(ctx, cfg, "reproducer", llm.Options{Recorder: rec})
	if err != nil {
		return nil, fmt.Errorf("build reproducer client: %w", err)
	}
	sb, err := newConfiguredSandbox(*cfg)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}
	r, err := buildReproducerWithSandbox(ctx, cfg, st, repoRoot, sb, prog, client)
	if err != nil {
		return nil, err
	}
	return &ReproDeps{Client: client, Repro: r, Sb: sb, Spend: rec}, nil
}

// buildReproducerWithSandbox builds a repro.Reproducer against a caller-supplied
// Sandbox (the container CLI backend for every normal path, or
// sandbox.NewHostExec() for the bugbot-14g0 fix-C attended escape hatch —
// see Dispatcher.reproOne). Factored out of BuildReproducer so the escape
// hatch reuses the exact same Options wiring (host toolchains, capability
// probing, dep strategy, ...) instead of a second, drift-prone copy.
func buildReproducerWithSandbox(ctx context.Context, cfg *config.Config, st *store.Store, repoRoot string, sb sandbox.Sandbox, prog progress.EventSink, client llm.Client) (*repro.Reproducer, error) {
	// Probe image capabilities once; result is cached per image+mounts+env so
	// repeated daemon restarts or re-calls to BuildReproducer are free.
	// depProbeInputs threads dep-strategy mounts/env, local_mounts, AND host
	// toolchain mounts through, so a mounted toolchain or dependency cache
	// shows up as available (bugbot-14g0 acceptance 4, bugbot-48ya acceptance
	// 3). Against HostExec this probes the operator's own host directly —
	// cfg.Sandbox.Image is irrelevant there but harmless as a cache-key
	// component (HostExec has no image concept).
	probeMounts, probeEnv := depProbeInputs(*cfg, sb, repoRoot)
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, repoRoot, probeMounts, probeEnv)
	// Verified-command playbook (bugbot-u2v5): run the same bounded probe
	// battery repro.PlaybookOnce caches per (repoRoot HEAD, dep-resolution
	// fingerprint), against the exact spec/deps this reproducer's own sandbox
	// runs use. Degrades to an inactive (empty) Playbook on any battery
	// failure — see PlaybookOnce's doc — so this never blocks reproducer
	// construction.
	pb := repro.PlaybookOnce(ctx, sb, repoRoot, sandbox.Spec{Image: cfg.Sandbox.Image}, resolveDepsForPlaybook(*cfg, sb, repoRoot), ingest.DetectBuildSystems(repoRoot))
	r, err := repro.New(client, sb, repoRoot, repro.Options{
		MaxAttempts:      cfg.Repro.MaxAttempts,
		Image:            cfg.Sandbox.Image,
		Network:          cfg.Sandbox.Network,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(*cfg),
		HostToolchains:   cfg.Sandbox.HostToolchains,
		Capabilities:     caps,
		Playbook:         pb,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    reproTranscriptDir(*cfg),
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
		MaxParallel:      cfg.Repro.MaxParallel,
		TryMaxExecs:      cfg.Repro.TryMaxExecs,
	})
	if err != nil {
		return nil, fmt.Errorf("build reproducer: %w", err)
	}
	return r, nil
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
	if !sandboxAvailable(cfg) {
		_, _ = fmt.Fprintln(out, "Reproduce stage skipped: no sandbox backend (container runtime or bwrap) available.")
		// hook stays nil so the catch-up drain prints a note; DoRepro check in
		// the caller still runs (with r == nil) so no catch-up is attempted.
		return nil, nil, nil, attempted, nil
	}
	sb, sbErr := newConfiguredSandbox(cfg)
	if sbErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build sandbox: %w", sbErr)
	}
	// Preflight: probe the sandbox toolchain once per process before burning
	// per-finding repro attempts on an image that cannot run the target
	// ecosystem (bugbot-u6td). A probe infrastructure error does not gate:
	// repro attempts remain meaningful evidence when the probe itself failed.
	if verdict, vErr := repro.VerifySandboxOnce(ctx, opts.Target, cfg); vErr == nil && verdict.BlocksRepro() {
		_, _ = fmt.Fprintf(out,
			"Reproduce stage skipped: sandbox toolchain check failed (%s): %s\n  Run `bugbot doctor` and %s.\n",
			verdict.Category, verdict.Detail, SandboxRemediationHint(cfg))
		return nil, nil, nil, attempted, nil
	}
	// Ledger repro + patch-prover spend; the scan run id is pinned by the
	// hook on first use (the funnel supplies it), and again after the sweep
	// for the catch-up drain.
	rec = newLedgerRecorder(ctx, st)
	// Same live spend tick as BuildReproducer (bugbot-psva): the in-run hook's
	// recorder is constructed separately, so it wires its own emission.
	if prog != nil {
		rec.onRecord = func(in, out, cached int64) {
			progress.Emit(prog, progress.Event{
				Kind: progress.KindSpendTick, Role: progress.RoleReproducer,
				InputTokens: in, OutputTokens: out, CacheReadTokens: cached,
			})
		}
	}
	reproClient, rErr := config.ResolveRole(ctx, &cfg, "reproducer", llm.Options{Recorder: rec})
	if rErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build reproducer client: %w", rErr)
	}
	// Probe image capabilities once; result is cached per image+mounts+env so
	// subsequent daemon cycles and parallel scan runs are free. depProbeInputs
	// threads dep-strategy mounts/env, local_mounts, and host toolchain mounts
	// through, so a mounted toolchain or dependency cache shows up as available.
	probeMounts, probeEnv := depProbeInputs(cfg, sb, opts.Target)
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, opts.Target, probeMounts, probeEnv)
	// Verified-command playbook (bugbot-u2v5): same battery as
	// buildReproducerWithSandbox above, run once per (repoDir HEAD,
	// dep-resolution fingerprint) and degrading to an inactive Playbook on
	// any failure — never blocks reproducer construction.
	pb := repro.PlaybookOnce(ctx, sb, opts.Target, sandbox.Spec{Image: cfg.Sandbox.Image}, resolveDepsForPlaybook(cfg, sb, opts.Target), ingest.DetectBuildSystems(opts.Target))
	r, rNewErr := repro.New(reproClient, sb, opts.Target, repro.Options{
		MaxAttempts:      cfg.Repro.MaxAttempts,
		Image:            cfg.Sandbox.Image,
		Network:          cfg.Sandbox.Network,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(cfg),
		HostToolchains:   cfg.Sandbox.HostToolchains,
		Capabilities:     caps,
		Playbook:         pb,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    reproTranscriptDir(cfg),
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
		MaxParallel:      cfg.Repro.MaxParallel,
		TryMaxExecs:      cfg.Repro.TryMaxExecs,
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
	emitReproBlocked(out, nil, r.SummarizeBlocked(pending))
	summary, err := r.PromoteAll(ctx, st, pending)
	if err != nil {
		return fmt.Errorf("reproduce catch-up: %w", err)
	}
	printReproSummary(out, summary)
	return nil
}

// emitReproBlocked prints the bugbot-14g0 acceptance-2 stage-start aggregate
// to out ("N finding(s) blocked: image lacks X", one line per missing
// ecosystem's actual binary, sorted for determinism) and delegates the
// event side to progress.EmitReproBlocked, which emits a KindReproBlocked
// event per ecosystem so a running daemon's status.json (and any other
// progress sink) carries the same aggregate with the same wording. A
// nil/empty blocked map is a silent no-op — nothing was blocked, nothing to
// report. sink may be nil (e.g. the scan catch-up drain, whose progress sink
// belongs to the funnel stages, not the repro backlog preview).
func emitReproBlocked(out io.Writer, sink progress.EventSink, blocked map[string]int) {
	if len(blocked) == 0 {
		return
	}
	ecos := make([]string, 0, len(blocked))
	for eco := range blocked {
		ecos = append(ecos, eco)
	}
	sort.Strings(ecos)
	for _, eco := range ecos {
		binary := ecosystem.ToolchainBinary(ecosystem.Ecosystem(eco))

		_, _ = fmt.Fprintf(out, "%d finding(s) blocked: image lacks %s\n", blocked[eco], binary)
	}
	progress.EmitReproBlocked(sink, blocked)
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
	if s.BlockedToolchain > 0 {
		ecos := make([]string, 0, len(s.BlockedByEcosystem))
		for eco := range s.BlockedByEcosystem {
			ecos = append(ecos, eco)
		}
		sort.Strings(ecos)
		for _, eco := range ecos {
			binary := ecosystem.ToolchainBinary(ecosystem.Ecosystem(eco))

			_, _ = fmt.Fprintf(out, "Blocked toolchain: %d finding(s) — image lacks %s\n", s.BlockedByEcosystem[eco], binary)
		}
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
	StopProgress func()
	// FindingID, when non-empty, switches Repro from the backlog batch drain
	// to a single-finding attended rerun of the finding with this exact id or
	// unambiguous id prefix (resolved via report.ResolveID). Required for
	// Unsandboxed (bugbot-14g0 fix C: the escape hatch is single-finding only).
	FindingID string
	// Unsandboxed opts into the fix-C attended escape hatch: the finding
	// named by FindingID runs directly on the host (sandbox.HostExec) against
	// a workspace copy, with no container isolation. Refused with an error
	// unless FindingID is also set — this is what keeps the escape hatch out
	// of the backlog batch path and, structurally, out of the daemon (which
	// never calls Dispatcher.Repro at all; see daemon.promoteNewFindings).
	Unsandboxed bool
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
	// Hard refusal (bugbot-14g0 fix C): the unsandboxed escape hatch is
	// single-finding-attended only. Without FindingID this call is the
	// backlog batch drain — exactly the unattended path the escape hatch must
	// never reach — so refuse loudly rather than silently ignoring the flag.
	if opts.Unsandboxed && opts.FindingID == "" {
		return nil, fmt.Errorf(
			"repro: --unsandboxed requires a single finding id (e.g. `bugbot repro <finding-id> --unsandboxed`); " +
				"it is refused for the backlog batch path")
	}

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

	// Resolve the target repo path the same way every other Dispatcher verb
	// does (openRepo -> ingest.Open, which validates a git work tree and
	// resolves to `git rev-parse --show-toplevel`, NOT merely
	// filepath.Abs). An unset opts.Target (the TUI dispatch palette never
	// populates it — bugbot-pt83) resolves via ingest.Open("") ==
	// ingest.Open(cwd), matching every sibling verb; the resolved toplevel
	// (not just an absolute cwd) matters because repro.New keys dependency
	// detection, build-system detection, and finding file paths off the
	// repo ROOT, and a TUI launched from a subdirectory would otherwise
	// silently repro against the wrong (sub)tree. Without resolving to a
	// real path at all, BuildReproducer/repro.New rejected an empty
	// repoDir outright ("repro: empty repoDir") whenever the backlog was
	// non-empty.
	repo, err := d.openRepo(ctx, opts.Target)
	if err != nil {
		return nil, err
	}
	opts.Target = repo.Root()
	// Single-finding attended path (opts.FindingID set): resolve one finding
	// and run it, sandboxed or unsandboxed (bugbot-14g0 fix C), instead of
	// draining the backlog. Diverges completely from the batch path below.
	if opts.FindingID != "" {
		return d.reproOne(ctx, opts, cfg, st, out)
	}

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

	if !sandboxAvailable(cfg) {
		_, _ = fmt.Fprintln(out, "Repro backlog skipped: no sandbox backend (container runtime or bwrap) available.")
		return &ReproResult{Skipped: "no sandbox backend"}, nil
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
			"sandbox toolchain check failed (%s): %s — run `bugbot doctor` and %s",
			verdict.Category, verdict.Detail, SandboxRemediationHint(cfg))
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
	rd, err := BuildReproducer(ctx, &cfg, st, opts.Target, d.sink)
	if err != nil {
		return nil, err
	}
	defer rd.Repro.Close() //nolint:errcheck
	defer closeSandbox(rd.Sb)

	_, _ = fmt.Fprintf(out,
		"\nRepro backlog: %d eligible, attempting %d (max=%d, backend=%s)...\n",
		len(backlog), len(batch), batchSize, sandboxBackendLabel(cfg),
	)
	if cfg.Repro.TranscriptDir != "" {
		_, _ = fmt.Fprintf(out, "Transcripts: %s\n", cfg.Repro.TranscriptDir)
	}

	// Stage-start aggregate (bugbot-14g0 acceptance 2): a zero-container
	// preview against the already-probed CapabilitySet, printed to CLI
	// output AND emitted as progress events (status.json via d.sink) BEFORE
	// any per-finding claim/attempt happens.
	emitReproBlocked(out, d.sink, rd.Repro.SummarizeBlocked(batch))
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

// reproOne implements the bugbot-14g0 fix-C single-finding attended path:
// resolve exactly one finding (by id or unambiguous id prefix) and run it
// through the reproducer, either sandboxed (the normal container backend) or,
// with opts.Unsandboxed, via sandbox.HostExec — directly on the host against
// a workspace copy, never the live checkout. The unsandboxed choice is
// recorded on the finding's repro_attempts row via MarkReproAttemptUnsandboxed
// regardless of outcome, so a T1 promoted this way is distinguishable
// (acceptance 5). Dispatcher.Repro's opt-in gate (opts.Unsandboxed requires
// opts.FindingID) and its structural separation from the daemon's own
// PromoteAll call site (daemon.promoteNewFindings never reaches here) are
// what keep this path out of every unattended flow.
func (d *Dispatcher) reproOne(ctx context.Context, opts ReproOpts, cfg config.Config, st *store.Store, out io.Writer) (*ReproResult, error) {
	fnd, err := report.ResolveID(ctx, st, opts.FindingID)
	if err != nil {
		return nil, fmt.Errorf("resolve finding %q: %w", opts.FindingID, err)
	}

	if opts.TranscriptDir != "" {
		cfg.Repro.TranscriptDir = opts.TranscriptDir
	}

	var sb sandbox.Sandbox
	if opts.Unsandboxed {
		_, _ = fmt.Fprintf(out,
			"\n!!! UNSANDBOXED: %q will run DIRECTLY ON THE HOST (workspace copy, no container isolation, "+
				"full network access, your OS user's privileges). Proceed only if you are attended and trust "+
				"this repro's command. !!!\n\n", fnd.Title)
		sb = sandbox.NewHostExec()
	} else {
		runtime, ok := sandbox.Detect()
		if !ok {
			_, _ = fmt.Fprintln(out, "Repro skipped: no container runtime (podman/docker) found on PATH.")
			return &ReproResult{Skipped: "no container runtime"}, nil
		}
		var cerr error
		sb, cerr = sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
		if cerr != nil {
			return nil, fmt.Errorf("build sandbox: %w", cerr)
		}
	}
	if cliSb, ok := sb.(*sandbox.CLI); ok {
		defer func() { _ = cliSb.Close() }()
	}

	rec := newLedgerRecorder(ctx, st)
	client, err := config.ResolveRole(ctx, &cfg, "reproducer", llm.Options{Recorder: rec})
	if err != nil {
		return nil, fmt.Errorf("build reproducer client: %w", err)
	}
	r, err := buildReproducerWithSandbox(ctx, &cfg, st, opts.Target, sb, d.sink, client)
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck

	outcome, err := r.PromoteOne(ctx, st, fnd)
	// Record the unsandboxed provenance flag regardless of outcome/error: the
	// attempt (or blocked/skip decision) still ran unsandboxed. EnqueueRepro
	// inside PromoteOne/promoteOne has already ensured the row exists by now.
	if opts.Unsandboxed {
		if merr := st.MarkReproAttemptUnsandboxed(ctx, fnd.Fingerprint); merr != nil {
			_, _ = fmt.Fprintf(out, "warning: failed to record unsandboxed provenance: %v\n", merr)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reproduce %s: %w", fnd.Title, err)
	}
	if opts.StopProgress != nil {
		opts.StopProgress()
	}

	summary := &repro.Summary{PerFinding: []repro.FindingOutcome{*outcome}}
	switch {
	case outcome.BlockedToolchain:
		summary.BlockedToolchain = 1
		summary.BlockedByEcosystem = map[string]int{outcome.MissingEcosystem: 1}
	case outcome.Skipped:
		summary.Skipped = 1
	default:
		summary.Attempted = 1
		switch {
		case outcome.Promoted:
			summary.Promoted = 1
		case outcome.Witnessed:
			summary.Witnessed = 1
		default:
			summary.Failed = 1
		}
	}
	printReproSummary(out, summary)
	return &ReproResult{Summary: summary}, nil
}
