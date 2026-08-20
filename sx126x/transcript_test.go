package sx126x

import (
	"context"
	"errors"
	"testing"
	"time"

	"meshrunner.dev/pkg/lora"
)

func TestOpenTranscript(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, nil)
	c.done()
	if got := r.Version(); got != "SX1262" {
		t.Errorf("Version = %q, want SX1262", got)
	}
}

// A bus with nothing on it must fail on the very first command, not
// succeed its way into a Radio that will never hear anything.
func TestOpenNoDevice(t *testing.T) {
	c := &chip{t: t, steps: []xfer{
		{"post-reset flush, dead bus", []byte{0x80, 0x00}, []byte{0x00, 0x00}},
	}}
	pins := lora.Pins{Reset: &fakePin{c}, Busy: &fakeBusy{c}, DIO1: &fakeDIO1{c: c, edges: make(chan struct{}, 1)}}
	_, err := Open(&fakeSPI{c}, pins, Config{})
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("err = %v, want ErrNoDevice", err)
	}
}

// A TCXO misconfiguration must fail Open with the crystal verdict, not
// return a deaf Radio.
func TestOpenCrystalFailure(t *testing.T) {
	steps := openScript()[:11] // up to the crystal verdict
	steps[10] = xfer{"crystal verdict: XOSC_START_ERR", []byte{0x17, 0x00, 0x00, 0x00},
		[]byte{stOK, stOK, 0x00, 0x20}}
	c := &chip{t: t, steps: steps}
	pins := lora.Pins{Reset: &fakePin{c}, Busy: &fakeBusy{c}, DIO1: &fakeDIO1{c: c, edges: make(chan struct{}, 1)}}
	_, err := Open(&fakeSPI{c}, pins, Config{TCXO: TCXO1V8, UseDCDC: true})
	if !errors.Is(err, ErrDeviceError) {
		t.Fatalf("err = %v, want ErrDeviceError", err)
	}
	c.done()
}

func TestConfigureTranscript(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, configureScript())
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.done()
	if got := r.Params(); got != meshcoreEU() {
		t.Errorf("Params() does not round-trip: %+v", got)
	}
}

// Inverted IQ requires the errata §15.4 register fix alongside the
// packet-params flag; a driver that sets the flag alone degrades
// reception silently.
func TestInvertIQErrata(t *testing.T) {
	p := meshcoreEU()
	p.InvertIQ = true
	script := configureScript()
	script[7] = xfer{"packet params inverted IQ",
		[]byte{0x8C, 0x00, 0x20, 0x00, 0xFF, 0x01, 0x01}, nil}
	script[8] = xfer{"read IQ polarity", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
		[]byte{stOK, stOK, stOK, stOK, 0x0D}}
	// bit 2 must be cleared for inverted IQ: 0x0D -> 0x09, so a write
	// follows the read.
	rest := make([]xfer, 0, len(script)+1)
	rest = append(rest, script[:9]...)
	rest = append(rest, xfer{"errata 15.4 write", []byte{0x0D, 0x07, 0x36, 0x09}, nil})
	rest = append(rest, script[9:]...)
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, rest)
	if err := r.Configure(p); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.done()
}

// The full happy path of a reception: everything about the frame is
// read BEFORE the narrow clear, and the clear names exactly the flags
// that were latched.
func TestPollReceivesFrame(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x42}
	buf := make([]byte, 3+len(payload))
	buf[0], buf[1], buf[2] = stOK, stOK, stOK
	copy(buf[3:], payload)
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(), append(startReceiveScript(),
		irqReadStep(IRQRxDone|IRQPreambleDetected|IRQSyncWordValid|IRQHeaderValid),
		xfer{"rx buffer status", []byte{0x13, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, byte(len(payload)), 0x00}},
		xfer{"read buffer", append([]byte{0x1E, 0x00, 0x00}, make([]byte, len(payload))...), buf},
		xfer{"packet status", []byte{0x14, 0x00, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, 176, 49}},
		xfer{"narrow clear of what was read", []byte{0x02, 0x00, 0x1E}, nil},
	)...))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	frame, err := r.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	c.done()
	if string(frame.Payload) != string(payload) {
		t.Errorf("payload = % X", frame.Payload)
	}
	if frame.RSSI != -88 || frame.SNR != 12.25 {
		t.Errorf("RSSI/SNR = %v/%v, want -88/12.25", frame.RSSI, frame.SNR)
	}
	if frame.Airtime != meshcoreEU().Airtime(len(payload)) {
		t.Errorf("airtime not derived from params")
	}
}

// A HeaderErr sitting next to a valid header belongs to an earlier
// packet; treating it as this frame's verdict throws away a good frame.
func TestStaleHeaderErrDoesNotKillArrivingFrame(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQHeaderErr|IRQHeaderValid|IRQPreambleDetected),
		xfer{"shed only the stale bit", []byte{0x02, 0x00, 0x20}, nil},
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	frame, err := r.Poll()
	if frame != nil || err != nil {
		t.Fatalf("Poll = %v, %v; want nil, nil (frame still arriving)", frame, err)
	}
	c.done()
}

// A hopeless header (no valid header alongside) is expected traffic,
// reported as ErrHeader and cleared narrowly.
func TestHeaderError(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQHeaderErr|IRQPreambleDetected),
		xfer{"clear header error + progress", []byte{0x02, 0x00, 0x24}, nil},
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_, err := r.Poll()
	if !errors.Is(err, ErrHeader) {
		t.Fatalf("err = %v, want ErrHeader", err)
	}
	c.done()
}

func TestCRCError(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQRxDone|IRQCRCErr|IRQPreambleDetected|IRQHeaderValid),
		xfer{"clear corrupt frame's flags", []byte{0x02, 0x00, 0x56}, nil},
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_, err := r.Poll()
	if !errors.Is(err, ErrCRC) {
		t.Fatalf("err = %v, want ErrCRC", err)
	}
	c.done()
}

// The CAD transcript: standby first, CAD's own narrow clears, and —
// because the radio was receiving — reception re-armed at the end, on
// the way out. The scan must hand the radio back the way it found it.
func TestAssessChannelRestoresReception(t *testing.T) {
	steps := append(configureScript(), startReceiveScript()...)
	steps = append(steps,
		irqReadStep(0),   // guard: nothing latched
		statusStep(stRX), // radio is receiving
		xfer{"standby for CAD", []byte{0x80, 0x00}, nil},
		xfer{"CAD params SF8", []byte{0x88, 0x02, 0x15, 0x0A, 0x00, 0x00, 0x00, 0x00}, nil},
		xfer{"clear CAD flags", []byte{0x02, 0x01, 0x80}, nil},
		xfer{"CAD IRQ routing", []byte{0x08, 0x01, 0x80, 0x01, 0x80, 0x00, 0x00, 0x00, 0x00}, nil},
		xfer{"start CAD", []byte{0xC5}, nil},
		irqReadStep(IRQCadDone|IRQCadDetected),
		xfer{"clear CAD outcome", []byte{0x02, 0x01, 0x80}, nil},
	)
	steps = append(steps, startReceiveScript()...) // the restore
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	busy, err := r.AssessChannel(context.Background(), CAD4Symbols)
	if err != nil {
		t.Fatalf("AssessChannel: %v", err)
	}
	if !busy {
		t.Error("CadDetected latched but AssessChannel said free")
	}
	c.done()
}

// The scan must refuse rather than destroy: an unread frame and a frame
// in the air are both grounds for refusal, with distinct errors.
func TestAssessChannelRefusals(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQRxDone),
		irqReadStep(IRQPreambleDetected),
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := r.AssessChannel(context.Background(), CAD4Symbols); !errors.Is(err, ErrUnreadFrame) {
		t.Fatalf("with RxDone latched: err = %v, want ErrUnreadFrame", err)
	}
	if _, err := r.AssessChannel(context.Background(), CAD4Symbols); !errors.Is(err, ErrReceiveInProgress) {
		t.Fatalf("mid-frame: err = %v, want ErrReceiveInProgress", err)
	}
	c.done()
}

// The sleep/wake handshake inversion, proven: after Sleep every plain
// command times out on BUSY, and Wake — a transfer before any wait —
// is what recovers, replaying the channel behind it.
func TestSleepInvertsBusyAndWakeRecovers(t *testing.T) {
	steps := append(configureScript(),
		xfer{"sleep", []byte{0x84, 0x04}, nil},
		// Wake's bare status transfer is what pulls NSS low:
		xfer{"wake transfer", []byte{0xC0, 0x00}, nil},
		xfer{"standby RC", []byte{0x80, 0x00}, nil},
	)
	steps = append(steps, configureScript()...) // Wake replays the channel
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.Sleep(); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	// Any ordinary command must now hit the inverted handshake.
	if _, err := r.IRQ(); !errors.Is(err, ErrBusyTimeout) {
		t.Fatalf("command while asleep: err = %v, want ErrBusyTimeout", err)
	}
	if err := r.Wake(); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	c.done()
}

// ResetAGC: the full cycle — guard, warm sleep, wake transfer,
// recalibration with its verdict read, and the complete channel replay.
// Nothing may be assumed to have survived the calibration.
func TestResetAGCReplaysEverything(t *testing.T) {
	steps := append(configureScript(),
		irqReadStep(0),   // guard
		statusStep(stOK), // in standby: no re-arm at the end
		xfer{"warm sleep", []byte{0x84, 0x04}, nil},
		xfer{"wake transfer", []byte{0xC0, 0x00}, nil},
		xfer{"standby RC", []byte{0x80, 0x00}, nil},
		xfer{"clear device errors", []byte{0x07, 0x00, 0x00}, nil},
		xfer{"calibrate all", []byte{0x89, 0x7F}, nil},
		xfer{"calibration verdict", []byte{0x17, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, 0x00, 0x00}},
	)
	steps = append(steps, configureScript()...) // the replay
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, steps)
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.ResetAGC(); err != nil {
		t.Fatalf("ResetAGC: %v", err)
	}
	c.done()
}

// The stale-latch expiry: a preamble with no frame behind it must stop
// reading as "busy" once the channel's own timing rules it out, and the
// stale markers must be cleared so DIO1 can fall.
func TestReceiveProgressExpiry(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQPreambleDetected),
		irqReadStep(IRQPreambleDetected),
		xfer{"expire stale preamble", []byte{0x02, 0x00, 0x04}, nil},
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }

	pre, _, err := r.ReceiveInProgress()
	if err != nil || !pre {
		t.Fatalf("fresh preamble: pre=%v err=%v, want true", pre, err)
	}
	// Window at SF8/62.5k, 32-symbol preamble: ~148 + ~49 ms. Beyond
	// it no header can still legitimately arrive.
	clock = clock.Add(300 * time.Millisecond)
	pre, hdr, err := r.ReceiveInProgress()
	if err != nil {
		t.Fatalf("expiry pass: %v", err)
	}
	if pre || hdr {
		t.Fatal("stale preamble still reads as busy after expiry")
	}
	c.done()
}

// A committed header re-anchors the clock with the full-frame window:
// a long legitimate frame must NOT be expired mid-payload.
func TestReceiveProgressHeaderReanchors(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		irqReadStep(IRQPreambleDetected),
		irqReadStep(IRQPreambleDetected|IRQSyncWordValid|IRQHeaderValid),
		irqReadStep(IRQPreambleDetected|IRQSyncWordValid|IRQHeaderValid),
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }

	if _, _, err := r.ReceiveInProgress(); err != nil {
		t.Fatal(err)
	}
	// 150 ms in, the header lands: the frame is committed.
	clock = clock.Add(150 * time.Millisecond)
	if _, hdr, err := r.ReceiveInProgress(); err != nil || !hdr {
		t.Fatalf("header not seen: hdr=%v err=%v", hdr, err)
	}
	// 1 s later — well past the preamble window but within the maximum
	// frame airtime (~2.4 s for 255 bytes on this channel) — the frame
	// must still be protected.
	clock = clock.Add(1 * time.Second)
	if _, hdr, err := r.ReceiveInProgress(); err != nil || !hdr {
		t.Fatalf("legitimate long frame expired mid-payload: hdr=%v err=%v", hdr, err)
	}
	c.done()
}

// Commands the chip explicitly rejects must fail loudly.
func TestCommandFailureSurfaces(t *testing.T) {
	r, c := rig(t, []xfer{
		{"rejected command", []byte{0x82, 0xFF, 0xFF, 0xFF},
			[]byte{stOK, 0x28}}, // status 0x4: processing error
	})
	r.ready = true
	_, err := r.dev.cmd(opSetRx, 0xFF, 0xFF, 0xFF)
	if !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("err = %v, want ErrCommandFailed", err)
	}
	c.done()
}
