package sx126x

import (
	"context"
	"errors"
	"testing"
	"time"
)

// frameScript is the SPI exchange of one collected frame: flags, buffer
// status, payload, quality, narrow clear.
func frameScript(payload []byte) []xfer {
	buf := make([]byte, 3+len(payload))
	buf[0], buf[1], buf[2] = stOK, stOK, stOK
	copy(buf[3:], payload)
	return []xfer{
		irqReadStep(IRQRxDone | IRQPreambleDetected | IRQSyncWordValid | IRQHeaderValid),
		{"rx buffer status", []byte{0x13, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, byte(len(payload)), 0x00}},
		{"read buffer", append([]byte{0x1E, 0x00, 0x00}, make([]byte, len(payload))...), buf},
		{"packet status", []byte{0x14, 0x00, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, 176, 49, 190}},
		// Raw 0xFFFE0: sign bit set, two's complement -32 → -1.9375 Hz
		// at 62.5 kHz.
		{"frequency error", []byte{0x1D, 0x07, 0x6B, 0x00, 0x00, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0F, 0xFF, 0xE0}},
		{"narrow clear of what was read", []byte{0x02, 0x00, 0x1E}, nil},
	}
}

// A latched IRQ whose edge was never delivered must still be collected,
// without any timer: the level check before sleeping sees the held line.
func TestReceiveWakesOnLevelNotEdge(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(), append(startReceiveScript(),
		append([]xfer{
			statusStep(0x50),  // ModeRx
			irqReadStep(0x00), // first Poll: nothing yet
		}, frameScript(payload)...)...)...))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	// The IRQ latches between the first Poll and the sleep; the edge is
	// lost, only the held level says so.
	c.dio1Level = true
	frame, err := r.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	c.done()
	if string(frame.Payload) != string(payload) {
		t.Errorf("payload = % X", frame.Payload)
	}
	if frame.FreqErr != -1.9375 {
		t.Errorf("FreqErr = %v, want -1.9375 (20-bit sign extension)", frame.FreqErr)
	}
}

// With the watchdog armed, even a transition missed while asleep — the
// level check long passed — is recovered at the next period.
func TestReceiveWatchdogRecoversWhileAsleep(t *testing.T) {
	payload := []byte{0xAA}
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true, Watchdog: 5 * time.Millisecond},
		append(configureScript(), append(startReceiveScript(),
			append([]xfer{
				statusStep(0x50),
				irqReadStep(0x00), // Poll before the sleep: nothing
			}, frameScript(payload)...)...)...))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	// Level reads low and no edge ever comes: only the watchdog polls.
	frame, err := r.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	c.done()
	if string(frame.Payload) != string(payload) {
		t.Errorf("payload = % X", frame.Payload)
	}
}

func TestChipStats(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(),
		xfer{"get stats", []byte{0x10, 0, 0, 0, 0, 0, 0, 0},
			[]byte{stOK, stOK, 0x01, 0x2C, 0x00, 0x05, 0x00, 0x02}},
		xfer{"reset stats", []byte{0x00, 0, 0, 0, 0, 0, 0}, nil},
	))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	s, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Received != 300 || s.CRCErrors != 5 || s.HeaderErrors != 2 {
		t.Errorf("stats = %+v, want 300/5/2", s)
	}
	if err := r.ResetStats(); err != nil {
		t.Fatalf("ResetStats: %v", err)
	}
	c.done()
}

// Without a watchdog the wait is purely edge-driven: no edge, low
// level, no wake-ups — the context deadline is the only way out.
func TestReceiveWithoutWatchdogSleepsOnEdgesAlone(t *testing.T) {
	r, c := openRig(t, Config{TCXO: TCXO1V8, UseDCDC: true}, append(configureScript(), append(startReceiveScript(),
		statusStep(0x50),
		irqReadStep(0x00),
	)...))
	if err := r.Configure(meshcoreEU()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := r.StartReceive(); err != nil {
		t.Fatalf("StartReceive: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive = %v, want the context deadline", err)
	}
	c.done()
}
