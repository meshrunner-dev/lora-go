package sx126x

import (
	"context"
	"errors"
	"testing"

	"meshrunner.dev/pkg/lora"
)

// txConfig is the bench board with transmit enabled: chip declared,
// ceiling at -5 dBm chip-side.
func txConfig() Config {
	return Config{TCXO: TCXO1V8, UseDCDC: true, Chip: SX1262, MaxTxPower: -5}
}

// The full transmission, pinned byte by byte: errata §15.2 clamping,
// OCP preserved across SetPaConfig, the measured PA operating point
// for -5 dBm, the per-frame payload length, the airtime-scaled chip
// timeout — and the radio handed back exactly as found, reception
// length restored and RX re-armed.
func TestTransmitTranscript(t *testing.T) {
	payload := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0x42}
	steps := append(configureScript(), startReceiveScript()...)
	steps = append(steps,
		irqReadStep(0),   // guard: nothing latched
		statusStep(stRX), // radio was receiving
		xfer{"standby for TX", []byte{0x80, 0x00}, nil},
		// applyTXPower: §15.2 clamping (read 0x08, write |0x1E), OCP
		// saved and restored around SetPaConfig, then the measured
		// -5 dBm point of the SX1262 table: duty 1, hpMax 1, value 6.
		xfer{"read TX clamp config", []byte{0x1D, 0x08, 0xD8, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x08}},
		xfer{"errata 15.2: clamp |= 0x1E", []byte{0x0D, 0x08, 0xD8, 0x1E}, nil},
		xfer{"save OCP", []byte{0x1D, 0x08, 0xE7, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x38}},
		xfer{"PA config -5 dBm (measured point)", []byte{0x95, 0x01, 0x01, 0x00, 0x01}, nil},
		xfer{"restore OCP", []byte{0x0D, 0x08, 0xE7, 0x38}, nil},
		xfer{"TX params: value 6, 200us ramp", []byte{0x8E, 0x06, 0x04}, nil},
		// Per-frame packet params: length 5, not the RX 0xFF.
		xfer{"packet params for this frame", []byte{0x8C, 0x00, 0x20, 0x00, 0x05, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
		xfer{"stage payload", append([]byte{0x0E, 0x00}, payload...), nil},
		xfer{"clear TX flags", []byte{0x02, 0x02, 0x01}, nil},
		xfer{"TX IRQ routing", []byte{0x08, 0x02, 0x01, 0x02, 0x01, 0x00, 0x00, 0x00, 0x00}, nil},
		// SetTx timeout: airtime(5 B) = 246.784 ms, +25% +500 ms at
		// 15.625 us/tick = 51742 ticks.
		xfer{"start TX, airtime-scaled timeout", []byte{0x83, 0x00, 0xCA, 0x1E}, nil},
		irqReadStep(IRQTxDone),
		xfer{"clear TxDone", []byte{0x02, 0x00, 0x01}, nil},
		// The deferred hand-back: abort-to-standby, RX frame length
		// restored, reception re-armed.
		xfer{"standby (hand-back)", []byte{0x80, 0x00}, nil},
		xfer{"packet params back to RX", []byte{0x8C, 0x00, 0x20, 0x00, 0xFF, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
	)
	steps = append(steps, startReceiveScript()...)
	r, c := openRig(t, txConfig(), steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	res, err := r.Transmit(context.Background(), payload, -5)
	if err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	c.done()
	if res.Airtime != meshcoreEU().Airtime(len(payload)) {
		t.Errorf("airtime = %v", res.Airtime)
	}
}

// Every refusal, in order of the checks: no payload, over-length,
// ceiling exceeded (never clamped), transmit not enabled, chip not
// declared, and reception state protected.
func TestTransmitRefusals(t *testing.T) {
	r, c := openRig(t, txConfig(), append(configureScript(),
		irqReadStep(IRQRxDone), // for the unread-frame refusal
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	ctx := context.Background()

	if _, err := r.Transmit(ctx, nil, -5); !errors.Is(err, ErrBadConfig) {
		t.Errorf("empty payload: %v", err)
	}
	if _, err := r.Transmit(ctx, make([]byte, 256), -5); !errors.Is(err, ErrBadConfig) {
		t.Errorf("256 bytes: %v", err)
	}
	if _, err := r.Transmit(ctx, []byte{1}, -4); !errors.Is(err, ErrPowerExceedsCap) {
		t.Errorf("-4 dBm over a -5 ceiling: %v", err)
	}
	if _, err := r.Transmit(ctx, []byte{1}, -128); !errors.Is(err, ErrBadConfig) {
		t.Errorf("below chip minimum: %v", err)
	}
	if _, err := r.Transmit(ctx, []byte{1}, -5); !errors.Is(err, ErrUnreadFrame) {
		t.Errorf("unread frame: %v", err)
	}
	c.done()

	// A config without a ceiling refuses regardless of the request.
	r2, _ := rig(t, nil)
	r2.ready = true
	r2.cfg.Chip = SX1262
	if _, err := r2.Transmit(ctx, []byte{1}, -30); !errors.Is(err, ErrBadConfig) {
		t.Errorf("no ceiling configured: %v", err)
	}
	// And a ceiling without a chip variant refuses too.
	r3, _ := rig(t, nil)
	r3.ready = true
	r3.cfg.MaxTxPower = -5
	if _, err := r3.Transmit(ctx, []byte{1}, -5); !errors.Is(err, ErrBadConfig) {
		t.Errorf("chip undeclared: %v", err)
	}
}

// The PA operating points are load-bearing safety data: pin the pure
// mapping for each variant, including the SX1261's 15 dBm special case
// and the destructive-confusion guard (deviceSel differs).
func TestPAConfigFor(t *testing.T) {
	duty, hpMax, dev, val, err := paConfigFor(SX1262, -5)
	if err != nil || duty != 1 || hpMax != 1 || dev != 0x00 || val != 6 {
		t.Errorf("SX1262 -5 dBm: %d/%d/%d/%d %v", duty, hpMax, dev, val, err)
	}
	duty, hpMax, dev, val, err = paConfigFor(SX1262, 22)
	if err != nil || duty != 4 || hpMax != 7 || dev != 0x00 || val != 22 {
		t.Errorf("SX1262 22 dBm: %d/%d/%d/%d %v", duty, hpMax, dev, val, err)
	}
	duty, hpMax, dev, val, err = paConfigFor(SX1261, 15)
	if err != nil || duty != 0x06 || hpMax != 0 || dev != 0x01 || val != 14 {
		t.Errorf("SX1261 15 dBm special case: %d/%d/%d/%d %v", duty, hpMax, dev, val, err)
	}
	duty, hpMax, dev, val, err = paConfigFor(SX1261, -17)
	if err != nil || duty != 0x04 || dev != 0x01 || val != -17 {
		t.Errorf("SX1261 floor: %d/%d/%d/%d %v", duty, hpMax, dev, val, err)
	}
	if _, _, _, _, err := paConfigFor(SX1261, 16); !errors.Is(err, ErrBadConfig) {
		t.Errorf("SX1261 above range accepted: %v", err)
	}
	if _, _, _, _, err := paConfigFor(SX1262, -10); !errors.Is(err, ErrBadConfig) {
		t.Errorf("SX1262 below range accepted: %v", err)
	}
	if _, _, _, _, err := paConfigFor(ChipUnset, 0); !errors.Is(err, ErrBadConfig) {
		t.Errorf("unset variant accepted: %v", err)
	}
}

// MaxTxPowerZero expresses the 0 dBm ceiling the zero value cannot.
func TestTxPowerCapSemantics(t *testing.T) {
	r, _ := rig(t, nil)
	if _, enabled := r.txPowerCap(); enabled {
		t.Error("zero MaxTxPower must disable transmit")
	}
	r.cfg.MaxTxPower = MaxTxPowerZero
	if capDBm, enabled := r.txPowerCap(); !enabled || capDBm != 0 {
		t.Errorf("MaxTxPowerZero: cap=%d enabled=%v", capDBm, enabled)
	}
	r.cfg.MaxTxPower = -5
	if capDBm, enabled := r.txPowerCap(); !enabled || capDBm != -5 {
		t.Errorf("-5: cap=%d enabled=%v", capDBm, enabled)
	}
}

// A chip-side timeout surfaces as ErrTimeout — and the hand-back still
// runs: the radio ends re-armed, not wedged in TX.
func TestTransmitChipTimeout(t *testing.T) {
	payload := []byte{0x01}
	steps := append(configureScript(), startReceiveScript()...)
	steps = append(steps,
		irqReadStep(0),
		statusStep(stRX),
		xfer{"standby for TX", []byte{0x80, 0x00}, nil},
		xfer{"read TX clamp config", []byte{0x1D, 0x08, 0xD8, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x1E}}, // already fixed: no write
		xfer{"save OCP", []byte{0x1D, 0x08, 0xE7, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x38}},
		xfer{"PA config", []byte{0x95, 0x01, 0x01, 0x00, 0x01}, nil},
		xfer{"restore OCP", []byte{0x0D, 0x08, 0xE7, 0x38}, nil},
		xfer{"TX params", []byte{0x8E, 0x06, 0x04}, nil},
		xfer{"packet params len 1", []byte{0x8C, 0x00, 0x20, 0x00, 0x01, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
		xfer{"stage payload", []byte{0x0E, 0x00, 0x01}, nil},
		xfer{"clear TX flags", []byte{0x02, 0x02, 0x01}, nil},
		xfer{"TX IRQ routing", []byte{0x08, 0x02, 0x01, 0x02, 0x01, 0x00, 0x00, 0x00, 0x00}, nil},
		xfer{"start TX", []byte{0x83, 0x00, 0xBF, 0xE1}, nil},
		irqReadStep(IRQTimeout), // the chip stopped the transmission
		xfer{"clear Timeout", []byte{0x02, 0x02, 0x00}, nil},
		xfer{"transmit verdict", []byte{0x17, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, 0x01, 0x00}}, // PA ramp error latched
	)
	// Hand-back still runs after the failure.
	steps = append(steps,
		xfer{"standby (hand-back)", []byte{0x80, 0x00}, nil},
		xfer{"packet params back to RX", []byte{0x8C, 0x00, 0x20, 0x00, 0xFF, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
	)
	steps = append(steps, startReceiveScript()...)
	r, c := openRig(t, txConfig(), steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	_, err := r.Transmit(context.Background(), payload, -5)
	if !errors.Is(err, ErrTimeout) && !errors.Is(err, ErrDeviceError) {
		t.Fatalf("err = %v, want the chip's timeout surfaced", err)
	}
	c.done()
}

var _ = lora.RFTransmit // the RF switch path is exercised on boards that have one

// The SX1261 transmit path differs from the SX1262/68 one in two ways,
// both pinned here end to end: it skips the errata §15.2 PA clamping
// (that guards the high-power PA only), and SetPaConfig selects the
// low-power PA with deviceSel=1. At 15 dBm the datasheet's special case
// also raises the duty cycle to 6 and programs 14 — so this single
// transcript exercises both SX1261-specific behaviours.
func TestTransmitSX1261(t *testing.T) {
	cfg := Config{TCXO: TCXO1V8, UseDCDC: true, Chip: SX1261, MaxTxPower: 15}
	payload := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0x42}
	steps := append(configureScript(), startReceiveScript()...)
	steps = append(steps,
		irqReadStep(0),
		statusStep(stRX),
		xfer{"standby for TX", []byte{0x80, 0x00}, nil},
		// No clamp read/write: §15.2 is SX1262/68 only.
		xfer{"save OCP", []byte{0x1D, 0x08, 0xE7, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x18}},
		// 15 dBm special case: duty 6, hpMax 0, deviceSel 1 (SX1261 PA).
		xfer{"PA config 15 dBm SX1261", []byte{0x95, 0x06, 0x00, 0x01, 0x01}, nil},
		xfer{"restore OCP", []byte{0x0D, 0x08, 0xE7, 0x18}, nil},
		xfer{"TX params: value 14, 200us ramp", []byte{0x8E, 0x0E, 0x04}, nil},
		xfer{"packet params for this frame", []byte{0x8C, 0x00, 0x20, 0x00, 0x05, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
		xfer{"stage payload", append([]byte{0x0E, 0x00}, payload...), nil},
		xfer{"clear TX flags", []byte{0x02, 0x02, 0x01}, nil},
		xfer{"TX IRQ routing", []byte{0x08, 0x02, 0x01, 0x02, 0x01, 0x00, 0x00, 0x00, 0x00}, nil},
		xfer{"start TX, airtime-scaled timeout", []byte{0x83, 0x00, 0xCA, 0x1E}, nil},
		irqReadStep(IRQTxDone),
		xfer{"clear TxDone", []byte{0x02, 0x00, 0x01}, nil},
		xfer{"standby (hand-back)", []byte{0x80, 0x00}, nil},
		xfer{"packet params back to RX", []byte{0x8C, 0x00, 0x20, 0x00, 0xFF, 0x01, 0x00}, nil},
		xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
	)
	steps = append(steps, startReceiveScript()...)
	r, c := openRig(t, cfg, steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	if _, err := r.Transmit(context.Background(), payload, 15); err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	c.done()
}
