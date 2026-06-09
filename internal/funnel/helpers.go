package funnel

import (
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/agent"
)

// readOnlyTools builds the read-only code tool set (read_file, list_dir, grep)
// rooted at the repository, shared by finder and refuter agents. The tools are
// stateless and safe for concurrent use across parallel agents, so they are
// constructed once per call and reused.
func (f *Funnel) readOnlyTools() ([]agent.Tool, error) {
	root := f.repo.Root()
	readFile, err := agent.NewReadFile(root)
	if err != nil {
		return nil, fmt.Errorf("funnel: read_file tool: %w", err)
	}
	listDir, err := agent.NewListDir(root)
	if err != nil {
		return nil, fmt.Errorf("funnel: list_dir tool: %w", err)
	}
	grep, err := agent.NewGrep(root)
	if err != nil {
		return nil, fmt.Errorf("funnel: grep tool: %w", err)
	}
	return []agent.Tool{readFile, listDir, grep}, nil
}

// transcriptOption returns a Runner option that persists transcripts when a
// TranscriptDir is configured, or a no-op option otherwise.
func (f *Funnel) transcriptOption() agent.Option {
	if f.opts.TranscriptDir == "" {
		return func(*agent.Runner) {}
	}
	return agent.WithTranscriptDir(f.opts.TranscriptDir)
}

// noteMu guards Result.Skipped, which parallel stages append to concurrently.
var noteMu sync.Mutex

// note appends a human-readable skip/degradation note to the Result. It is safe
// for concurrent use by parallel stage goroutines.
func (f *Funnel) note(result *Result, msg string) {
	noteMu.Lock()
	result.Skipped = append(result.Skipped, msg)
	noteMu.Unlock()
}

// normalizeSeverity coerces a model-supplied severity to one of the four valid
// values, defaulting unknown/empty to "medium" so ranking and reporting always
// have a usable value.
func normalizeSeverity(s string) string {
	switch s {
	case "critical", "high", "medium", "low":
		return s
	default:
		return "medium"
	}
}

// normalizeConfidence coerces a model-supplied confidence to one of the three
// valid values. Unknown/empty maps to "low" so that ambiguous output is treated
// conservatively (low-confidence candidates are dropped in triage).
func normalizeConfidence(s string) string {
	switch s {
	case "high", "medium", "low":
		return s
	default:
		return "low"
	}
}
