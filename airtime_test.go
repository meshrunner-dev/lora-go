package lora

import (
	"testing"
	"time"
)

// The SF7/125 kHz/CR4-5 case is the textbook one: a 10-byte payload with
// an 8-symbol preamble and CRC takes 41.216 ms. Anchoring on a published
// figure catches a wrong constant that a self-consistent test would not.
func TestAirtimeKnownReference(t *testing.T) {
	p := Params{Frequency: 868e6, SF: SF7, BW: BW125000, CR: CR5, Preamble: 8, CRC: true}
	got := p.Airtime(10)
	want := 41216 * time.Microsecond
	if d := got - want; d < -50*time.Microsecond || d > 50*time.Microsecond {
		t.Fatalf("airtime = %v, want %v", got, want)
	}
}

// Symbol duration doubles with each spreading factor and halves with each
// doubling of bandwidth — the two levers the modulation offers.
func TestSymbolDuration(t *testing.T) {
	base := Params{SF: SF7, BW: BW125000}
	if got, want := base.SymbolDuration(), 1024*time.Microsecond; got != want {
		t.Fatalf("SF7/125k symbol = %v, want %v", got, want)
	}
	slower := Params{SF: SF8, BW: BW125000}
	if slower.SymbolDuration() != 2*base.SymbolDuration() {
		t.Fatal("SF8 symbol is not twice SF7")
	}
	narrower := Params{SF: SF7, BW: BW62500}
	if narrower.SymbolDuration() != 2*base.SymbolDuration() {
		t.Fatal("halving bandwidth did not double the symbol")
	}
}

// Airtime must grow monotonically with payload, spreading factor and
// coding rate, and shrink with bandwidth.
func TestAirtimeMonotonic(t *testing.T) {
	base := Params{Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, Preamble: 8, CRC: true}

	if base.Airtime(1) >= base.Airtime(200) {
		t.Error("airtime does not grow with payload")
	}
	slower := base
	slower.SF = SF9
	if slower.Airtime(50) <= base.Airtime(50) {
		t.Error("airtime does not grow with spreading factor")
	}
	lessFEC := base
	lessFEC.CR = CR5
	if lessFEC.Airtime(50) >= base.Airtime(50) {
		t.Error("a lighter coding rate should be quicker")
	}
	wider := base
	wider.BW = BW125000
	if wider.Airtime(50) >= base.Airtime(50) {
		t.Error("a wider channel should be quicker")
	}
}

// Past SF11 at narrow bandwidths a symbol exceeds 16 ms and the
// modulation mandates the low-data-rate optimisation.
func TestLowDataRateOptimize(t *testing.T) {
	if (Params{SF: SF8, BW: BW62500}).LowDataRateOptimize() {
		t.Error("SF8/62.5k (4.1 ms symbol) should not need LDRO")
	}
	if !(Params{SF: SF12, BW: BW62500}).LowDataRateOptimize() {
		t.Error("SF12/62.5k (65 ms symbol) must enable LDRO")
	}
}

func TestValidate(t *testing.T) {
	ok := Params{Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, Preamble: 32, SyncWord: 0x12}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	for name, bad := range map[string]Params{
		"no frequency":       {SF: SF8, BW: BW62500, CR: CR8, Preamble: 32, SyncWord: 0x12},
		"SF too low":         {Frequency: 868e6, SF: 4, BW: BW62500, CR: CR8, Preamble: 32, SyncWord: 0x12},
		"SF too high":        {Frequency: 868e6, SF: 13, BW: BW62500, CR: CR8, Preamble: 32, SyncWord: 0x12},
		"odd BW":             {Frequency: 868e6, SF: SF8, BW: 100000, CR: CR8, Preamble: 32, SyncWord: 0x12},
		"odd CR":             {Frequency: 868e6, SF: SF8, BW: BW62500, CR: 9, Preamble: 32, SyncWord: 0x12},
		"no preamble":        {Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, SyncWord: 0x12},
		"no sync word":       {Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, Preamble: 32},
		"implicit no length": {Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, Preamble: 32, SyncWord: 0x12, ImplicitHeader: true},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// SF5 uses the SX126x variant of the formula: RadioLib computes
// 12.096 ms for SF5/125 kHz/CR4-5, 10 bytes, CRC on, 8-symbol preamble.
// Keeping the SF7+ constant there gives 13.376 ms — one ceil step off.
func TestAirtimeSF5Variant(t *testing.T) {
	p := Params{Frequency: 868e6, SF: SF5, BW: BW125000, CR: CR5, Preamble: 8, CRC: true}
	got := p.Airtime(10)
	want := 12096 * time.Microsecond
	if d := got - want; d < -50*time.Microsecond || d > 50*time.Microsecond {
		t.Fatalf("SF5 airtime = %v, want %v (RadioLib reference)", got, want)
	}
}

// Out-of-domain inputs must yield zero, not garbage: SymbolDuration on
// an invalid bandwidth previously returned a negative astronomical
// duration through a float infinity.
func TestDurationGuards(t *testing.T) {
	if d := (Params{SF: SF8}).SymbolDuration(); d != 0 {
		t.Errorf("SymbolDuration with BW=0 = %v, want 0", d)
	}
	if d := (Params{SF: 255, BW: BW62500}).SymbolDuration(); d != 0 {
		t.Errorf("SymbolDuration with SF=255 = %v, want 0", d)
	}
	p := Params{Frequency: 868e6, SF: SF8, BW: BW62500, CR: CR8, Preamble: 32, CRC: true}
	if d := p.Airtime(-5); d != 0 {
		t.Errorf("Airtime(-5) = %v, want 0", d)
	}
}

// PreambleDuration is the carrier-sense window: 32 symbols at
// SF8/62.5 kHz is the MeshCore EU narrow figure of ~148 ms with the
// 4.25-symbol tail.
func TestPreambleDuration(t *testing.T) {
	p := Params{SF: SF8, BW: BW62500, Preamble: 32}
	got := p.PreambleDuration()
	want := time.Duration((32 + 4.25) * 4096 * float64(time.Microsecond))
	if got != want {
		t.Fatalf("PreambleDuration = %v, want %v", got, want)
	}
}
