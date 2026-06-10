package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

// Stable substrings used to classify a saved transcript by the funnel role that
// produced it.
//
// The agent transcript records each request's MESSAGES but NOT the request's
// System field (see agent.Runner.Run: it records `messages`, while the system
// prompt rides on llm.Request.System separately). So we cannot classify by the
// finder/verifier SYSTEM prompt — it is not persisted. We classify instead by
// the first USER message, which IS recorded: the funnel seeds every finder run
// with finderTask(...) and every verifier run with verifierTask(...) (see
// internal/funnel/prompt.go and hypothesize.go/verify.go), and these task
// preambles carry stable, role-distinct opening sentences:
//
//   - finderRoleMarker is finderTask's opening sentence.
//   - verifierRoleMarker is verifierTask's opening sentence.
//
// RunJSON appends its JSON instruction AFTER the task, so the marker is always
// present at the head of the first user message regardless of schema. If a
// transcript matches neither (or both), classification is a hard error: a
// misclassified session would silently corrupt the recorded corpus.
const (
	finderRoleMarker   = "Audit these target files for bugs in your assigned focus area"
	verifierRoleMarker = "Try to refute this bug report"
)

// transcriptRole names the funnel role a recorded session belongs to.
type transcriptRole int

const (
	roleUnknown transcriptRole = iota
	roleFinder
	roleVerifier
)

// classifyTranscript inspects a transcript's first user message and decides
// whether it was produced by a finder or a verifier run, using the stable task
// markers above. It errors (rather than guessing) on an unclassifiable or
// ambiguous transcript so a bad recording fails loudly at corpus-build time.
func classifyTranscript(tr *agent.Transcript) (transcriptRole, error) {
	task, ok := firstUserMessage(tr)
	if !ok {
		return roleUnknown, fmt.Errorf("transcript has no request event with a user message to classify by")
	}
	isFinder := strings.Contains(task, finderRoleMarker)
	isVerifier := strings.Contains(task, verifierRoleMarker)
	switch {
	case isFinder && isVerifier:
		return roleUnknown, fmt.Errorf("transcript first user message matched BOTH finder and verifier markers; cannot classify")
	case isFinder:
		return roleFinder, nil
	case isVerifier:
		return roleVerifier, nil
	default:
		return roleUnknown, fmt.Errorf("transcript first user message matched neither finder marker %q nor verifier marker %q", finderRoleMarker, verifierRoleMarker)
	}
}

// firstUserMessage returns the content of the first user message in the
// transcript's first request event. A fresh agent run always opens with exactly
// one user message (the task), which carries the role-distinct task preamble.
// Returns ("", false) if no request event with a user message is present.
func firstUserMessage(tr *agent.Transcript) (string, bool) {
	for _, ev := range tr.Events {
		if ev.Kind != agent.EventRequest {
			continue
		}
		for _, m := range ev.Messages {
			if m.Role == llm.RoleUser {
				return m.Content, true
			}
		}
	}
	return "", false
}

// loadTranscriptDir loads every *.jsonl transcript under dir, sorted by
// filename. The autosave names files "<UTC-timestamp>-<slug>.jsonl" with a
// lexicographically-sortable timestamp prefix (see agent.Runner.autosave), so a
// filename sort is a chronological sort — the order the funnel emitted the runs
// under MaxParallel=1.
func loadTranscriptDir(dir string) ([]string, []*agent.Transcript, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read transcript dir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	trs := make([]*agent.Transcript, 0, len(names))
	for _, n := range names {
		tr, err := loadTranscriptFile(filepath.Join(dir, n))
		if err != nil {
			return nil, nil, fmt.Errorf("load transcript %q: %w", n, err)
		}
		trs = append(trs, tr)
	}
	return names, trs, nil
}

// loadTranscriptFile reads one JSONL transcript file.
func loadTranscriptFile(path string) (*agent.Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return agent.LoadJSONL(f)
}

// splitByRole partitions transcripts (already in chronological order) into
// finder and verifier sessions by inspecting each one's first request. It
// errors on the first unclassifiable transcript, naming it, so a corrupt
// recording is debuggable rather than silently dropped or misfiled.
func splitByRole(names []string, trs []*agent.Transcript) (finder, verifier []*agent.Transcript, err error) {
	for i, tr := range trs {
		role, cerr := classifyTranscript(tr)
		if cerr != nil {
			return nil, nil, fmt.Errorf("classify transcript %q: %w", names[i], cerr)
		}
		switch role {
		case roleFinder:
			finder = append(finder, tr)
		case roleVerifier:
			verifier = append(verifier, tr)
		default:
			return nil, nil, fmt.Errorf("transcript %q classified as unknown role", names[i])
		}
	}
	return finder, verifier, nil
}
