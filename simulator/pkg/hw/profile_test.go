package hw

import "testing"

func TestValidateProfile(t *testing.T) {
	base := Profile{
		BaseIdleW:   70,
		DefaultCapW: 500,
		PMaxW:       420,
		AlphaUtil:   1.1,
		BetaFreq:    1.2,
		FMinMHz:     1200,
		FMaxMHz:     3200,
		RaplCapMinW: 70,
		RaplCapMaxW: 600,
		DvfsRampMS:  500,
	}
	if err := ValidateProfile(base); err != nil {
		t.Fatalf("base profile should validate: %v", err)
	}

	bad := base
	bad.FMaxMHz = 1000
	if err := ValidateProfile(bad); err == nil {
		t.Fatalf("expected validation error when fMaxMHz < fMinMHz")
	}
}

func TestApplyOverrides(t *testing.T) {
	base := Profile{BaseIdleW: 80, PMaxW: 400, AlphaUtil: 1, BetaFreq: 1, FMinMHz: 1200, FMaxMHz: 3000, RaplCapMinW: 80, RaplCapMaxW: 500, DefaultCapW: 300}
	newIdle := 60.0
	newRamp := 200
	out := ApplyOverrides(base, Overrides{BaseIdleW: &newIdle, DvfsRampMS: &newRamp})
	if out.BaseIdleW != 60 {
		t.Fatalf("BaseIdleW override failed")
	}
	if out.DvfsRampMS != 200 {
		t.Fatalf("DvfsRampMS override failed")
	}
}
