package domain

import (
	"errors"
	"testing"
)

func TestValidateFindingState_IllegalTransitions(t *testing.T) {
	base := func() Finding {
		return Finding{
			Fingerprint: "fp",
			Tier:        2,
			Status:      StatusOpen,
		}
	}

	cases := []struct {
		name    string
		mutate  func(*Finding)
		wantErr bool
	}{
		{
			name:    "valid T2 open",
			mutate:  func(f *Finding) {},
			wantErr: false,
		},
		{
			name: "valid T1 with ReproPath",
			mutate: func(f *Finding) {
				f.Tier = 1
				f.ReproPath = "/artifacts/repro"
			},
			wantErr: false,
		},
		{
			name: "valid T0 with ReproPath",
			mutate: func(f *Finding) {
				f.Tier = 0
				f.ReproPath = "/artifacts/fix"
			},
			wantErr: false,
		},
		{
			name: "valid NeedsHuman BelowQuorum",
			mutate: func(f *Finding) {
				f.NeedsHuman = true
				f.NeedsHumanReason = NeedsHumanReasonBelowQuorum
			},
			wantErr: false,
		},
		{
			name: "valid NeedsHuman ProverExhausted with ReproPath",
			mutate: func(f *Finding) {
				f.Tier = 1
				f.ReproPath = "/artifacts/repro"
				f.NeedsHuman = true
				f.NeedsHumanReason = NeedsHumanReasonProverExhausted
			},
			wantErr: false,
		},
		{
			name: "valid ReproWitness with NeedsHuman",
			mutate: func(f *Finding) {
				f.NeedsHuman = true
				f.NeedsHumanReason = NeedsHumanReasonBelowQuorum
				f.ReproWitness = "/artifacts/witness"
			},
			wantErr: false,
		},
		// --- illegal ---
		{
			name: "ILLEGAL: T0 without ReproPath",
			mutate: func(f *Finding) {
				f.Tier = 0
			},
			wantErr: true,
		},
		{
			name: "ILLEGAL: T1 without ReproPath",
			mutate: func(f *Finding) {
				f.Tier = 1
			},
			wantErr: true,
		},
		{
			name: "ILLEGAL: T0 + NeedsHuman (Witnessed+Promoted conflict)",
			mutate: func(f *Finding) {
				f.Tier = 0
				f.ReproPath = "/artifacts/fix"
				f.NeedsHuman = true
				f.NeedsHumanReason = NeedsHumanReasonBelowQuorum
			},
			wantErr: true,
		},
		{
			name: "valid ReproWitness without NeedsHuman (witness-only ecosystem, bugbot-qb4r layer b)",
			mutate: func(f *Finding) {
				f.ReproWitness = "/artifacts/witness"
			},
			wantErr: false,
		},
		{
			name: "ILLEGAL: NeedsHuman without reason",
			mutate: func(f *Finding) {
				f.NeedsHuman = true
				// NeedsHumanReason stays None
			},
			wantErr: true,
		},
		{
			name: "ILLEGAL: reason set but NeedsHuman false",
			mutate: func(f *Finding) {
				f.NeedsHumanReason = NeedsHumanReasonBelowQuorum
			},
			wantErr: true,
		},
		{
			name: "ILLEGAL: ProverExhausted without ReproPath",
			mutate: func(f *Finding) {
				f.NeedsHuman = true
				f.NeedsHumanReason = NeedsHumanReasonProverExhausted
				// ReproPath empty
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.mutate(&f)
			err := ValidateFindingState(f)
			if tc.wantErr && err == nil {
				t.Errorf("expected illegal-transition error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected nil error, got %v", err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("expected ErrIllegalTransition, got %v", err)
			}
		})
	}
}
