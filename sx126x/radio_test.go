package sx126x

import "testing"

func TestValidateParamsIsConfigureEntireJudgement(t *testing.T) {
	// The synthesiser bound lived only inside Configure, so a dry run
	// stopping at lora.Params.Validate accepted 100 MHz and 1.2 GHz
	// and the relay discovered them after opening its hardware.
	base := meshcoreEU()
	for _, c := range []struct {
		hz uint32
		ok bool
	}{
		{149_999_999, false}, {150_000_000, true},
		{960_000_000, true}, {960_000_001, false},
		{100_000_000, false}, {1_200_000_000, false},
		{869_618_000, true},
	} {
		p := base
		p.Frequency = c.hz
		err := ValidateParams(p)
		if (err == nil) != c.ok {
			t.Errorf("%d Hz: %v, want ok=%v", c.hz, err, c.ok)
		}
	}
	// And the modulation half still judges: a bad SF fails here too.
	bad := base
	bad.SF = 0
	if err := ValidateParams(bad); err == nil {
		t.Error("SF 0 passed the pure judgement")
	}
}
