package domain

import "testing"

func TestTierLabel(t *testing.T) {
	cases := []struct {
		tier Tier
		want string
	}{
		{TierFixWitnessed, "T0 Fix-witnessed"},
		{TierReproduced, "T1 Reproduced"},
		{TierVerified, "T2 Verified"},
		{TierSuspected, "T3 Suspected"},
		{Tier(9), "T? Unknown"},
	}
	for _, c := range cases {
		if got := c.tier.Label(); got != c.want {
			t.Errorf("Tier(%d).Label() = %q, want %q", c.tier, got, c.want)
		}
	}
}

func TestTierLevel(t *testing.T) {
	cases := []struct {
		tier Tier
		want string
	}{
		// TierFixWitnessed is now the strongest tier: Level() == "error".
		{TierFixWitnessed, "error"},
		{TierReproduced, "error"},
		{TierVerified, "warning"},
		{TierSuspected, "note"},
		{Tier(9), "note"},
	}
	for _, c := range cases {
		if got := c.tier.Level(); got != c.want {
			t.Errorf("Tier(%d).Level() = %q, want %q", c.tier, got, c.want)
		}
	}
}

func TestTierBaseConfidence(t *testing.T) {
	cases := []struct {
		tier Tier
		want float64
	}{
		// TierFixWitnessed is the strongest tier: highest BaseConfidence.
		{TierFixWitnessed, 0.90},
		{TierReproduced, 0.80},
		{TierVerified, 0.50},
		{TierSuspected, 0.20},
		{Tier(9), 0.20},
	}
	for _, c := range cases {
		if got := c.tier.BaseConfidence(); got != c.want {
			t.Errorf("Tier(%d).BaseConfidence() = %v, want %v", c.tier, got, c.want)
		}
	}
}

func TestTierBaseConfidenceOrdering(t *testing.T) {
	if !(TierFixWitnessed.BaseConfidence() > TierReproduced.BaseConfidence()) {
		t.Error("TierFixWitnessed.BaseConfidence() should be > TierReproduced.BaseConfidence()")
	}
	if !(TierReproduced.BaseConfidence() > TierVerified.BaseConfidence()) {
		t.Error("TierReproduced.BaseConfidence() should be > TierVerified.BaseConfidence()")
	}
	if !(TierVerified.BaseConfidence() > TierSuspected.BaseConfidence()) {
		t.Error("TierVerified.BaseConfidence() should be > TierSuspected.BaseConfidence()")
	}
}

func TestTierLevelFixWitnessed(t *testing.T) {
	if got := TierFixWitnessed.Level(); got != "error" {
		t.Errorf("TierFixWitnessed.Level() = %q, want %q", got, "error")
	}
}

func TestParseSeverity(t *testing.T) {
	cases := []struct {
		in     string
		want   Severity
		wantOK bool
	}{
		{"critical", SeverityCritical, true},
		{"high", SeverityHigh, true},
		{"medium", SeverityMedium, true},
		{"low", SeverityLow, true},
		{"  HIGH  ", SeverityHigh, true}, // case-insensitive + trimmed
		{"Critical", SeverityCritical, true},
		{"", "", false},
		{"blocker", "", false},
		{"none", "", false},
	}
	for _, c := range cases {
		got, ok := ParseSeverity(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseSeverity(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSeverityRankAndAtLeast(t *testing.T) {
	// Higher rank = more severe; unknown sorts last.
	if SeverityCritical.Rank() <= SeverityHigh.Rank() ||
		SeverityHigh.Rank() <= SeverityMedium.Rank() ||
		SeverityMedium.Rank() <= SeverityLow.Rank() ||
		SeverityLow.Rank() <= Severity("").Rank() {
		t.Fatalf("severity rank ordering broken: crit=%d high=%d med=%d low=%d unknown=%d",
			SeverityCritical.Rank(), SeverityHigh.Rank(), SeverityMedium.Rank(),
			SeverityLow.Rank(), Severity("").Rank())
	}
	if !SeverityHigh.AtLeast(SeverityHigh) {
		t.Error("High should be AtLeast High (inclusive)")
	}
	if !SeverityCritical.AtLeast(SeverityLow) {
		t.Error("Critical should be AtLeast Low")
	}
	if SeverityLow.AtLeast(SeverityHigh) {
		t.Error("Low should not be AtLeast High")
	}
}

func TestParseConfidence(t *testing.T) {
	cases := []struct {
		in     string
		want   Confidence
		wantOK bool
	}{
		{"high", ConfidenceHigh, true},
		{"medium", ConfidenceMedium, true},
		{"low", ConfidenceLow, true},
		{" Low ", ConfidenceLow, true},
		{"", "", false},
		{"certain", "", false},
	}
	for _, c := range cases {
		got, ok := ParseConfidence(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseConfidence(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestConfidenceRankAndAtLeast(t *testing.T) {
	if ConfidenceHigh.Rank() <= ConfidenceMedium.Rank() ||
		ConfidenceMedium.Rank() <= ConfidenceLow.Rank() ||
		ConfidenceLow.Rank() <= Confidence("").Rank() {
		t.Fatalf("confidence rank ordering broken")
	}
	if ConfidenceLow.AtLeast(ConfidenceMedium) {
		t.Error("Low should not be AtLeast Medium")
	}
	if !ConfidenceMedium.AtLeast(ConfidenceMedium) {
		t.Error("Medium should be AtLeast Medium (inclusive)")
	}
}
