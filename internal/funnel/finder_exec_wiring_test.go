package funnel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandboxenv"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	wiringMaxExecs = 2
	wiringExecDur  = 42 * time.Millisecond
)

// execProbeSandbox records every Spec and returns a scripted result.
type execProbeSandbox struct {
	mu    sync.Mutex
	specs []sandbox.Spec
}

func (s *execProbeSandbox) Exec(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	return sandbox.Result{Stdout: "probe-out\n", Duration: wiringExecDur}, nil
}

func (s *execProbeSandbox) MaterializeWorkspace(string) (string, error) {
	return "", errors.New("execProbeSandbox: MaterializeWorkspace unsupported")
}

func (s *execProbeSandbox) snapshot() []sandbox.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sandbox.Spec(nil), s.specs...)
}

// runCodeSpecs returns the recorded Specs that came from run_code.
func (s *execProbeSandbox) runCodeSpecs() []sandbox.Spec {
	var out []sandbox.Spec
	for _, sp := range s.snapshot() {
		if len(sp.Cmd) == 3 && sp.Cmd[0] == "bash" && sp.Cmd[1] == "-c" {
			out = append(out, sp)
		}
	}
	return out
}

var _ sandbox.Sandbox = (*execProbeSandbox)(nil)

// finderSeen is one finder request as the fake model saw it.
type finderSeen struct {
	system  string
	tools   []string
	results []string // tool-result texts fed back so far
}

// probeFinder is a finder client that calls run_code once per entry of
// calls (sequentially), then reports no candidates.
type probeFinder struct {
	mu    sync.Mutex
	calls []string
	seen  []finderSeen
}

func (c *probeFinder) Capabilities() llmkit.Capabilities { return llmkit.Capabilities{} }

func (c *probeFinder) Complete(_ context.Context, req llmkit.Request) (llmkit.Response, error) {
	usage := llmkit.Usage{InputTokens: 10, OutputTokens: 5}
	if !strings.Contains(req.System, "YOUR ASSIGNED FOCUS (") {
		return llmkit.Response{Text: emptyCandidates, StopReason: llmkit.StopEndTurn, Usage: usage}, nil
	}
	var results []string
	for _, m := range req.Messages {
		if m.Role == llmkit.RoleToolResult {
			results = append(results, m.Text())
		}
	}
	var names []string
	for _, td := range req.Tools {
		names = append(names, td.Name)
	}
	c.mu.Lock()
	c.seen = append(c.seen, finderSeen{system: req.System, tools: names, results: results})
	c.mu.Unlock()
	n := len(results)
	if n < len(c.calls) {
		return llmkit.Response{
			ToolCalls: []llmkit.ToolCall{{
				ID: fmt.Sprintf("rc-%d", n), Name: "run_code", Arguments: json.RawMessage(c.calls[n]),
			}},
			StopReason: llmkit.StopToolUse, Usage: usage,
		}, nil
	}
	return llmkit.Response{Text: emptyCandidates, StopReason: llmkit.StopEndTurn, Usage: usage}, nil
}

func (c *probeFinder) snapshot() []finderSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]finderSeen(nil), c.seen...)
}

func countName(names []string, want string) int {
	n := 0
	for _, s := range names {
		if s == want {
			n++
		}
	}
	return n
}

// openGoExecRepo opens a committed Go repo (go.mod + the fixture sources).
func openGoExecRepo(t *testing.T) (*store.Store, *ingest.Repo, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	writeGateFile(t, filepath.Join(dir, "go.mod"), "module example.com/fixture\n\ngo 1.22\n")
	writeGateFile(t, filepath.Join(dir, "bug.go"), realBugFile)
	writeGateFile(t, filepath.Join(dir, "clean.go"), cleanFile)
	runGateGit(t, dir, "init", "-q")
	runGateGit(t, dir, "add", ".")
	runGateGit(t, dir, "commit", "-q", "-m", "seed")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo, dir
}

var wiringLenses = []string{"nil-safety/error-handling", "concurrency", "resource-leaks"}

func sweepWithFinder(t *testing.T, fe FinderExecOpts, fin *probeFinder) (*Funnel, *Result, string) {
	t.Helper()
	st, repo, dir := openGoExecRepo(t)
	f, err := New(
		RoleClients{Finder: fin, Verifier: newScriptedClient()},
		st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: wiringLenses}, FinderExec: fe},
	)
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	res, err := f.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return f, res, dir
}

func wiringExecOpts(sb sandbox.Sandbox) FinderExecOpts {
	return FinderExecOpts{
		Sandbox: sb, BackendLabel: "podman", Network: sandbox.NetworkNone, SkipProbe: true,
		MaxExecs: wiringMaxExecs, Timeout: 90 * time.Second, OutputCap: 4096,
	}
}

// finderUnits groups finder requests by system prompt (one per unit here).
func finderUnits(t *testing.T, seen []finderSeen) map[string][]finderSeen {
	t.Helper()
	units := map[string][]finderSeen{}
	for _, s := range seen {
		units[s.system] = append(units[s.system], s)
	}
	if len(units) < len(wiringLenses) {
		t.Fatalf("saw %d finder units, want >= %d", len(units), len(wiringLenses))
	}
	return units
}

// One run_code per unit when on, none when off, never shared.
func TestFinderExecWiring_PerUnitBudget(t *testing.T) {
	sb := &execProbeSandbox{}
	fin := &probeFinder{calls: []string{`{"command":"true"}`, `{"command":"true"}`, `{"command":"true"}`}}
	_, _, _ = sweepWithFinder(t, wiringExecOpts(sb), fin)

	units := finderUnits(t, fin.snapshot())
	for sys, reqs := range units {
		for _, r := range reqs {
			if got := countName(r.tools, "run_code"); got != 1 {
				t.Errorf("unit %.40q: run_code count = %d, want exactly 1 (tools %v)", sys, got, r.tools)
			}
		}
		last := reqs[len(reqs)-1]
		if len(last.results) != 3 {
			t.Fatalf("unit %.40q: %d tool results, want 3", sys, len(last.results))
		}
		for i := range wiringMaxExecs {
			if strings.Contains(last.results[i], "budget exhausted") {
				t.Errorf("unit %.40q: call %d refused; another unit consumed this unit's budget: %q", sys, i+1, last.results[i])
			}
		}
		if !strings.Contains(last.results[wiringMaxExecs], "run_code budget exhausted") {
			t.Errorf("unit %.40q: call %d = %q, want budget exhaustion", sys, wiringMaxExecs+1, last.results[wiringMaxExecs])
		}
	}
	if got, want := len(sb.runCodeSpecs()), len(units)*wiringMaxExecs; got != want {
		t.Errorf("sandbox execs = %d, want %d (units x MaxExecs)", got, want)
	}
}

func TestFinderExecWiring_OffHasNoRunCode(t *testing.T) {
	fin := &probeFinder{}
	_, _, _ = sweepWithFinder(t, FinderExecOpts{OffReason: "scan.finder_exec=off"}, fin)
	for sys, reqs := range finderUnits(t, fin.snapshot()) {
		for _, r := range reqs {
			if countName(r.tools, "run_code") != 0 {
				t.Errorf("unit %.40q: run_code offered with finder exec off: %v", sys, r.tools)
			}
		}
	}
}

// Paragraph iff run_code in the unit's tools, in the real run.
func TestFinderExecWiring_PromptParagraphIffRunCode(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("on=%v", on), func(t *testing.T) {
			fe := FinderExecOpts{OffReason: "scan.finder_exec=off"}
			if on {
				fe = wiringExecOpts(&execProbeSandbox{})
			}
			fin := &probeFinder{}
			_, _, _ = sweepWithFinder(t, fe, fin)
			for sys, reqs := range finderUnits(t, fin.snapshot()) {
				hasTool := countName(reqs[0].tools, "run_code") == 1
				hasPara := strings.HasSuffix(sys, "\n\n"+finderExecParagraph)
				if hasTool != on || hasPara != hasTool {
					t.Errorf("unit %.40q: run_code=%v paragraph=%v, want both %v", sys, hasTool, hasPara, on)
				}
				if strings.Contains(sys, "run_code") != on {
					t.Errorf("unit %.40q: system mentions run_code = %v, want %v", sys, !on, on)
				}
			}
		})
	}
}

// off is byte-identical to base for every lens x strategy; on appends
// exactly the blank-line-separated paragraph.
func TestWithFinderExecGuidance_ByteIdentityAndSuffix(t *testing.T) {
	sb := &execProbeSandbox{}
	_, repo, _ := openGoExecRepo(t)
	on := &Funnel{repo: repo, finderExec: finderExecState{Sandbox: sb, Network: sandbox.NetworkNone, MaxExecs: 1, Timeout: time.Second, OutputCap: 1}}
	off := &Funnel{repo: repo}
	var execs atomic.Int32
	var millis atomic.Int64
	onTools := []agent.Tool{on.buildFinderExecTool(&execs, &millis)}
	if onTools[0] == nil {
		t.Fatal("enabled funnel built no run_code tool")
	}
	if off.buildFinderExecTool(&execs, &millis) != nil {
		t.Fatal("disabled funnel built a run_code tool")
	}
	for _, l := range BuiltinLenses() {
		for _, st := range builtinStrategies() {
			base := composeFinderSystemPrompt("senior Go engineer", l, []ingest.Language{ingest.LangGo}, st)
			if got := withFinderExecGuidance(base, nil); got != base {
				t.Errorf("%s/%s: off prompt differs from base", l.Name, st.Name)
			}
			if got, want := withFinderExecGuidance(base, onTools), base+"\n\n"+finderExecParagraph; got != want {
				t.Errorf("%s/%s: on prompt is not base + blank line + paragraph", l.Name, st.Name)
			}
		}
	}
}

// Spec fields carried through the wiring; prefetch precedes every run_code exec.
func TestFinderExecWiring_SpecCarriesDepsAndLimits(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GOMODCACHE", t.TempDir())
	mount := sandbox.ROMount{HostPath: t.TempDir(), ContainerPath: "/sibling", Shared: true}
	setup := [][]string{{"sh", "-c", "true"}}
	sb := &execProbeSandbox{}
	fe := wiringExecOpts(sb)
	fe.DepStrategy = sandboxenv.DepStrategyFetch
	fe.LocalMounts = []sandbox.ROMount{mount}
	fe.SetupCmds = setup
	fin := &probeFinder{calls: []string{`{"command":"python -c 'print(1)'","files":{"probe.py":"x = 1\n"}}`}}
	f, _, _ := sweepWithFinder(t, fe, fin)

	if f.deps.Prefetch == nil || len(f.deps.ROMounts) == 0 || len(f.deps.SetupCmds) == 0 || len(f.deps.Env) == 0 {
		t.Fatalf("fixture produced thin deps, test would not discriminate: %+v", f.deps)
	}
	all := sb.snapshot()
	if len(all) == 0 || len(all[0].Cmd) == 0 || all[0].Cmd[0] != "go" {
		t.Fatalf("first Exec = %+v, want the dependency prefetch (go mod download) before any run_code", all)
	}
	prefetches := 0
	for _, sp := range all {
		if len(sp.Cmd) > 0 && sp.Cmd[0] == "go" {
			prefetches++
		}
	}
	if prefetches != 1 {
		t.Errorf("prefetch ran %d times, want exactly once", prefetches)
	}
	rc := sb.runCodeSpecs()
	if len(rc) < len(wiringLenses) {
		t.Fatalf("run_code specs = %d, want >= %d", len(rc), len(wiringLenses))
	}
	for _, sp := range rc {
		if sp.RepoDir != f.repo.Root() || sp.Workspace != "" {
			t.Errorf("RepoDir=%q Workspace=%q, want root %q and empty workspace", sp.RepoDir, sp.Workspace, f.repo.Root())
		}
		if sp.Network != sandbox.NetworkNone || sp.Timeout != fe.Timeout {
			t.Errorf("Network=%q Timeout=%v, want none/%v", sp.Network, sp.Timeout, fe.Timeout)
		}
		if fmt.Sprint(sp.ROMounts) != fmt.Sprint(f.deps.ROMounts) || fmt.Sprint(sp.Env) != fmt.Sprint(f.deps.Env) || fmt.Sprint(sp.SetupCmds) != fmt.Sprint(f.deps.SetupCmds) {
			t.Errorf("deps not carried: ROMounts=%v Env=%v SetupCmds=%v, want %v %v %v", sp.ROMounts, sp.Env, sp.SetupCmds, f.deps.ROMounts, f.deps.Env, f.deps.SetupCmds)
		}
		if string(sp.WriteFiles["probe.py"]) != "x = 1\n" {
			t.Errorf("WriteFiles = %v, want probe.py", sp.WriteFiles)
		}
	}
}

// with the real per-exec workspace copy, a probe that writes files
// leaves the scanned tree untouched.
func TestFinderExecWiring_ScannedTreeUnchanged(t *testing.T) {
	probe := `{"command":"echo scribble >> bug.go && echo new > created.txt && cat probe_input.txt","files":{"probe_input.txt":"seeded"}}`
	fin := &probeFinder{calls: []string{probe}}
	fe := FinderExecOpts{
		Sandbox: sandbox.NewHostExec(), Unsandboxed: true, Network: sandbox.NetworkHost, SkipProbe: true,
		MaxExecs: wiringMaxExecs, Timeout: 60 * time.Second, OutputCap: 4096,
	}
	st, repo, dir := openGoExecRepo(t)
	before := treeFingerprint(t, dir)
	f, err := New(RoleClients{Finder: fin, Verifier: newScriptedClient()}, st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: wiringLenses}, FinderExec: fe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	res, err := f.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after := treeFingerprint(t, dir); after != before {
		t.Errorf("scanned tree changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if res.Stats.SandboxExecs < len(wiringLenses) {
		t.Fatalf("SandboxExecs = %d; the probes never ran", res.Stats.SandboxExecs)
	}
	ran := false
	for _, s := range fin.snapshot() {
		for _, r := range s.results {
			if strings.Contains(r, "exit_code=0") && strings.Contains(r, "seeded") {
				ran = true
			}
		}
	}
	if !ran {
		t.Error("no run_code result showed the probe's files write and exit 0; the exec did not really run")
	}
}

// treeFingerprint is `git status --porcelain` plus sha256 of every file
// (outside .git) under dir.
func treeFingerprint(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	var lines []string
	lines = append(lines, "status:"+string(out))
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, fmt.Sprintf("%s %x", rel, sha256.Sum256(b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// run_code execs feed Stats.SandboxExecs / SandboxExecMillis.
func TestFinderExecWiring_StatsCountRunCodeExecs(t *testing.T) {
	sb := &execProbeSandbox{}
	fin := &probeFinder{calls: []string{`{"command":"true"}`, `{"command":"true"}`}}
	_, res, _ := sweepWithFinder(t, wiringExecOpts(sb), fin)
	n := len(sb.runCodeSpecs())
	if n < 2*len(wiringLenses) {
		t.Fatalf("run_code execs = %d, want >= %d", n, 2*len(wiringLenses))
	}
	if res.Stats.SandboxExecs != n {
		t.Errorf("Stats.SandboxExecs = %d, want %d", res.Stats.SandboxExecs, n)
	}
	if want := int64(n) * wiringExecDur.Milliseconds(); res.Stats.SandboxExecMillis != want {
		t.Errorf("Stats.SandboxExecMillis = %d, want %d", res.Stats.SandboxExecMillis, want)
	}
}
