package cli

import (
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

// TestBuildFunnelOptions_HeatOrdering verifies that buildFunnelOptions maps
// cfg.Scan.HeatOrdering to funnel.Options.DisableHeatOrdering with the correct
// inversion: HeatOrdering true → DisableHeatOrdering false, and vice-versa.
func TestBuildFunnelOptions_HeatOrdering(t *testing.T) {
	tests := []struct {
		name            string
		heatOrdering    bool
		wantDisableHeat bool
	}{
		{
			name:            "heat ordering enabled (default) → DisableHeatOrdering false",
			heatOrdering:    true,
			wantDisableHeat: false,
		},
		{
			name:            "heat ordering disabled → DisableHeatOrdering true",
			heatOrdering:    false,
			wantDisableHeat: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Scan.HeatOrdering = tt.heatOrdering
			opts, _, err := buildFunnelOptions(cfg, FunnelOptionOverrides{})
			if err != nil {
				t.Fatalf("buildFunnelOptions() error = %v", err)
			}
			if opts.DisableHeatOrdering != tt.wantDisableHeat {
				t.Errorf("DisableHeatOrdering = %v, want %v", opts.DisableHeatOrdering, tt.wantDisableHeat)
			}
		})
	}
}
