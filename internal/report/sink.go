package report

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/config"
)

// A Sink delivers a rendered Report to some destination. Implementations must
// be safe to call with a cancelable context and should respect cancellation
// where it is meaningful (filesystem writes are fast and treated as atomic at
// this scale). Write should be idempotent enough to re-run a scan's emit.
type Sink interface {
	// Write delivers the report. Name-identifying errors should wrap a clear
	// message so the CLI can report which sink failed.
	Write(ctx context.Context, r Report) error
	// Name returns the registry name of the sink, for logging.
	Name() string
}

// FSSink writes both report.md and report.sarif into a directory, creating it
// if necessary. Each emit writes a timestamped pair plus stable latest.md /
// latest.sarif copies, so tooling can always read "latest.*" while history is
// retained. The timestamp is taken from Report.Meta.GeneratedAt (falling back
// to now only when unset) so emits are reproducible in tests.
type FSSink struct {
	Dir string
}

// Name implements Sink.
func (s *FSSink) Name() string { return "fs" }

// Write renders Markdown and SARIF and writes the four files. It returns the
// first error encountered. Files are written 0o644 within a 0o755 directory.
func (s *FSSink) Write(ctx context.Context, r Report) error {
	if s.Dir == "" {
		return fmt.Errorf("report: FSSink requires a non-empty Dir")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("report: create dir %q: %w", s.Dir, err)
	}

	md := []byte(Markdown(r))
	sarif, err := SARIF(r)
	if err != nil {
		return fmt.Errorf("report: render sarif: %w", err)
	}

	stamp := r.Meta.GeneratedAt
	if stamp.IsZero() {
		stamp = nowFunc()
	}
	// Compact, sortable, filesystem-safe timestamp.
	ts := stamp.UTC().Format("20060102T150405Z")

	files := []struct {
		name string
		data []byte
	}{
		{fmt.Sprintf("report-%s.md", ts), md},
		{fmt.Sprintf("report-%s.sarif", ts), sarif},
		{"latest.md", md},
		{"latest.sarif", sarif},
	}
	for _, f := range files {
		path := filepath.Join(s.Dir, f.name)
		if err := os.WriteFile(path, f.data, 0o644); err != nil {
			return fmt.Errorf("report: write %q: %w", path, err)
		}
	}
	return nil
}

// nowFunc is the time source for the filename fallback, indirected for tests.
// Reports SHOULD set Meta.GeneratedAt, so this is only used when it is unset.
var nowFunc = func() time.Time { return time.Now().UTC() }

// StdoutSink writes the Markdown rendering to an io.Writer (stdout by default).
// It is the default human-facing sink and ignores SARIF, which is intended for
// machine consumption via the filesystem.
type StdoutSink struct {
	// W is the destination; nil means os.Stdout.
	W io.Writer
}

// Name implements Sink.
func (s *StdoutSink) Name() string { return "stdout" }

// Write renders Markdown and writes it to W (or os.Stdout).
func (s *StdoutSink) Write(ctx context.Context, r Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w := s.W
	if w == nil {
		w = os.Stdout
	}
	if _, err := io.WriteString(w, Markdown(r)); err != nil {
		return fmt.Errorf("report: write stdout: %w", err)
	}
	return nil
}

// SinksFromConfig resolves the sink names in cfg.Report.Sinks into Sink
// implementations. Recognized names:
//
//	fs       -> *FSSink writing into cfg.Report.Dir
//	stdout   -> *StdoutSink to os.Stdout
//
// For backwards compatibility with the existing config template/defaults, the
// legacy names "markdown" and "sarif" are accepted as aliases for the
// filesystem sink (which emits BOTH formats); they collapse to a single FSSink
// so duplicate output is avoided. An unknown name returns an error listing the
// valid choices. An empty sink list defaults to a single StdoutSink.
func SinksFromConfig(cfg config.Config) ([]Sink, error) {
	names := cfg.Report.Sinks
	if len(names) == 0 {
		return []Sink{&StdoutSink{}}, nil
	}

	var sinks []Sink
	fsAdded := false
	stdoutAdded := false
	for _, raw := range names {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "fs", "markdown", "sarif":
			if !fsAdded {
				sinks = append(sinks, &FSSink{Dir: cfg.Report.Dir})
				fsAdded = true
			}
		case "stdout":
			if !stdoutAdded {
				sinks = append(sinks, &StdoutSink{})
				stdoutAdded = true
			}
		default:
			return nil, fmt.Errorf("report: unknown sink %q (valid: %s)", raw, strings.Join(validSinkNames(), ", "))
		}
	}
	return sinks, nil
}

// validSinkNames returns the accepted sink names, sorted, for error messages.
func validSinkNames() []string {
	names := []string{"fs", "stdout"}
	sort.Strings(names)
	return names
}
