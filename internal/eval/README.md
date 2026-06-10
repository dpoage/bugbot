# internal/eval — offline detection benchmark

Ground truth for tuning Bugbot's precision. The harness runs the real
`internal/funnel` pipeline over fixture repos whose bugs are known by
construction, then scores the persisted findings against that ground truth —
measuring **both** detection rate (recall) and false-positive rate (the thing
precision turns on) — offline, deterministically, and with zero API calls.

## The command

Two equivalent entrypoints run the built-in suite (`BuiltinCases`) in scripted
mode and enforce the same gate:

```
bugbot eval                 # CLI: prints the table, exits non-zero on gate failure
bugbot eval --json          # machine-readable SuiteResult dump

go test ./internal/eval/ -run TestBenchmarkSuite -v   # the CI regression test
```

Both share a single gate, `eval.Gate(SuiteResult)`, so the CLI and the test can
never disagree on what "passing" means. The suite needs **no config file, no API
keys, and makes no LLM calls** — scripted finder/verifier behavior is embedded in
the cases themselves. It prints a table per case plus an aggregate line and
**fails (non-zero exit / red test)** if:

- any clean-code case reports a false positive, or
- aggregate precision is below 1.0.

Scripted mode is fully controlled, so these are exact regression assertions, not
flaky thresholds: a precision regression in the triage/verify/scoring machinery
turns this red.

## Two modes

### Scripted (`ModeScripted`) — default

Each case carries a `ScriptedCase` describing finder/verifier behavior via a
`ScriptedClient`, which routes responses by request content
(`OnSystemContains` targets a finder lens; `OnTaskContains` targets a refuter by
candidate title). This tests the **pipeline's discrimination machinery** —
triage, adversarial verify, dedup, suppression, scoring — under exact inputs.

### Recorded (`ModeRecorded`) — regression replay

Each case carries a `RecordedCase` holding one `RoleTranscriptStore` per role
(finder, verifier). Each store is an **ordered list of recorded agent sessions**
(`agent.Transcript`), one per agent run that role performs. The store serves one
session per agent run, in recorded order, through `agent.ReplayClient` (which
validates the tool round-trip structure and errors on divergence).

Why per-role, two-level: the funnel creates *many* runners per role (one finder
runner per `(lens, chunk)`, one verifier runner per candidate), all sharing one
client. Replay must therefore be a *sequence of sessions*, each itself a
sequence of completions. Determinism requires serial execution
(`Options.MaxParallel = 1`, which `RunCase` defaults to) so the funnel drives
runs in the same order they were recorded.

## Recording a real session (future work)

The mechanism is shipped and tested with synthetic recordings. Capturing
**real-model** transcripts and committing them as `testdata/` for regression is
future work and requires real API keys. The workflow:

1. Configure real provider keys (referenced by env-var name, per
   `internal/config`) and point the funnel at a `TranscriptDir`:

   ```go
   f, _ := funnel.New(realClients, st, repo, funnel.Options{
       TranscriptDir: "/tmp/bugbot-rec",
       MaxParallel:   1, // REQUIRED for deterministic replay ordering
   })
   res, _ := f.Sweep(ctx)
   ```

   Every agent run auto-saves its transcript as a timestamped JSONL file in
   `TranscriptDir` (see `agent.Runner.autosave` / `agent.Transcript.SaveJSONL`).

2. Sort the saved JSONL files by timestamp and split them by role. With
   `MaxParallel = 1` the funnel emits finder sessions first (in lens × chunk
   order), then verifier sessions (in candidate × refuter order). Load each with
   `agent.LoadJSONL` and group into `RoleTranscriptStore`s:

   ```go
   finder := eval.NewRoleTranscriptStore("finder", caps, finderSessions...)
   verifier := eval.NewRoleTranscriptStore("verifier", caps, verifierSessions...)
   c.Recorded = &eval.RecordedCase{Finder: finder, Verifier: verifier}
   ```

3. Commit the JSONL files under `internal/eval/testdata/<case>/` and load them in
   a case constructor. Re-running `RunSuite(ctx, cases, eval.ModeRecorded)` then
   replays the captured behavior against the *current* pipeline code, catching
   prompt/pipeline regressions without spending a token.

> Note on RunJSON repair: if a recorded finder/verifier reply fails to parse,
> `RunJSON` makes one repair round-trip (a fresh agent run). That repair consumes
> an extra session from the store. Record clean, parseable replies, or include the
> repair turn as its own session.

## Adding a case

```go
eval.Case{
    Name: "my-bug",
    Repo: eval.FixtureSpec{Files: map[string]string{"x.go": src}},
    Seeded: []eval.SeededBug{{File: "x.go", Line: 12, LineTolerance: 2, Kind: "nil-deref"}},
    Scripted: &eval.ScriptedCase{
        Finder:   func(c *eval.ScriptedClient) { c.OnSystemContains("nil-safety/error-handling", eval.Candidates(/*...*/)) },
        Verifier: func(c *eval.ScriptedClient) { c.OnTaskContains("my title", eval.NotRefutedJSON) },
    },
}
```

- **Seeded bug**: matched to a finding by *file + line within `LineTolerance`*.
  `Kind` is a human label (and per-lens-breakdown hint), **not** a match gate — a
  bug found by any lens at the right place counts.
- **Clean-code case**: leave `Seeded` empty. Any finding is a false positive.
- **Suppressed case**: leave `Seeded` empty and add a `Suppression`; the bug is
  dropped in triage, so the expected outcome is zero findings.

## Scoring

- **TP** — a seeded bug matched a finding (greedy by closest line; one finding
  satisfies at most one seeded bug, so duplicates don't inflate the count).
- **FP** — a finding that matched no seeded bug (every finding, on a clean case).
- **FN** — a seeded bug no finding matched.
- **Precision** = TP / (TP + FP); **Recall** = TP / (TP + FN). Both are 1.0 when
  their denominator is zero (a case that reported nothing, or seeded nothing, is
  vacuously perfect).
- **Stage passthrough** — the funnel's `Stats` (hypothesized → triaged →
  verified, plus the four drop counters and `Killed`) ride on each `CaseResult`,
  so the report's *where-killed* column shows where seeded bugs died.
