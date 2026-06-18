package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// TestPrintEstimate_Priors covers the no-history rendering: the basis is flagged
// as priors, the duration is reported as unknown, and the unit breakdown spells
// out chunk + diff-intent + seam contributions.
func TestPrintEstimate_Priors(t *testing.T) {
	var buf bytes.Buffer
	e := &funnel.Estimate{
		Kind:                 store.ScanTargeted,
		Commit:               "abcdef0",
		Files:                40,
		Packages:             6,
		Chunks:               5,
		Lenses:               7,
		FinderUnits:          12,
		Seams:                2,
		DiffIntent:           true,
		CartographerEnabled:  true,
		CartographerPackages: 6,
		CartographerUncached: 4,
		Calibrated:           false,
		TokensPerUnit:        100_000,
		EstTokens:            1_200_000,
		EstTokensLow:         600_000,
		EstTokensHigh:        2_400_000,
		// ThroughputTokPerSec zero → duration unknown.
	}
	printEstimate(&buf, e)
	out := buf.String()

	for _, want := range []string{
		"no LLM calls made",
		"in-scope files",
		"finder units",
		"(9 chunk + 1 diff-intent + 2 seam)", // 12 - 2 seams - 1 diff-intent = 9
		"cartographer",
		"4 need fresh summaries",
		"Projected spend: ~1.2M tokens",
		"Projected time:  unknown",
		"built-in priors",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("priors estimate output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "calibrated from") {
		t.Errorf("priors estimate must not claim calibration:\n%s", out)
	}
}

// TestPrintEstimate_Calibrated covers the calibrated rendering: a finite
// duration is shown and the basis cites the sample.
func TestPrintEstimate_Calibrated(t *testing.T) {
	var buf bytes.Buffer
	e := &funnel.Estimate{
		Kind:                store.ScanOneshot,
		Commit:              "abcdef0",
		Files:               40,
		FinderUnits:         10,
		Calibrated:          true,
		SampleRuns:          4,
		SampleMatched:       true,
		TokensPerUnit:       50_000,
		ThroughputTokPerSec: 2_000,
		EstTokens:           500_000,
		EstTokensLow:        250_000,
		EstTokensHigh:       1_000_000,
		EstDuration:         250 * time.Second,
		EstDurationLow:      125 * time.Second,
		EstDurationHigh:     500 * time.Second,
	}
	printEstimate(&buf, e)
	out := buf.String()

	for _, want := range []string{
		"Projected spend: ~500k tokens",
		"Projected time:  ~4m10s",
		"calibrated from 4 matching-kind runs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("calibrated estimate output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "built-in priors") {
		t.Errorf("calibrated estimate must not claim priors:\n%s", out)
	}
	if strings.Contains(out, "unknown") {
		t.Errorf("calibrated estimate has throughput, must not report unknown time:\n%s", out)
	}
}

func TestHumanCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{850, "850"},
		{12_345, "12k"},
		{1_500_000, "1.5M"},
		{-5, "0"},
	}
	for _, c := range cases {
		if got := humanCount(c.in); got != c.want {
			t.Errorf("humanCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRoundDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "<1s"},
		{0, "<1s"},
		{90 * time.Second, "1m30s"},
		{2 * time.Hour, "2h0m0s"},
	}
	for _, c := range cases {
		if got := roundDuration(c.in); got != c.want {
			t.Errorf("roundDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
