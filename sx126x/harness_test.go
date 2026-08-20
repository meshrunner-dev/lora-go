package sx126x

// The test harness: a scripted chip. Every fact this driver validated
// on real hardware is a property of the SPI byte stream, so the fakes
// replay pinned transcripts and fail on any drift — an unexpected
// transfer, a missing one, or a single wrong byte.
//
// The BUSY fake models the one piece of analogue behaviour that has
// already caused a real bug: asleep, the chip holds BUSY high until an
// SPI transfer pulls NSS low, which deadlocks any code that waits for
// BUSY before transferring.

import (
	"bytes"
	"testing"
	"time"

	"meshrunner.dev/pkg/lora"
)

// stOK is the status byte a healthy chip in STDBY_RC clocks back; stRX
// the same in receive mode.
const (
	stOK byte = 0x24 // mode STDBY_RC, command status "data available"
	stRX byte = 0x54 // mode RX
)

type xfer struct {
	desc string
	tx   []byte
	rx   []byte // nil = all-stOK filler of the right length
}

// chip is the shared state behind the fake SPI and pins.
type chip struct {
	t        *testing.T
	steps    []xfer
	i        int
	sleeping bool // BUSY held high until the next transfer
}

func (c *chip) transfer(tx, rx []byte) error {
	c.t.Helper()
	c.sleeping = false // NSS activity is what wakes the chip
	if c.i >= len(c.steps) {
		c.t.Fatalf("unexpected transfer #%d: % X", c.i, tx)
	}
	st := c.steps[c.i]
	c.i++
	if !bytes.Equal(tx, st.tx) {
		c.t.Fatalf("step %d (%s):\n  got  % X\n  want % X", c.i-1, st.desc, tx, st.tx)
	}
	// Doctrine check, everywhere: a blanket IRQ clear discards events.
	if tx[0] == opClearIrqStatus && len(tx) >= 3 {
		if w := uint16(tx[1])<<8 | uint16(tx[2]); w == 0x03FF {
			c.t.Errorf("step %d (%s): blanket ClearIrqStatus(0x03FF)", c.i-1, st.desc)
		}
	}
	if tx[0] == opSetSleep {
		c.sleeping = true
	}
	for j := range rx {
		rx[j] = stOK
	}
	if st.rx != nil {
		copy(rx, st.rx)
	}
	return nil
}

func (c *chip) done() {
	c.t.Helper()
	if c.i != len(c.steps) {
		c.t.Fatalf("script not exhausted: %d of %d steps ran; next would be %q",
			c.i, len(c.steps), c.steps[c.i].desc)
	}
}

type fakeSPI struct{ c *chip }

func (s *fakeSPI) Transfer(tx, rx []byte) error { return s.c.transfer(tx, rx) }
func (s *fakeSPI) Close() error                 { return nil }

type fakePin struct{ c *chip }

func (p *fakePin) Set(bool) error { return nil }
func (p *fakePin) Close() error   { return nil }

type fakeBusy struct{ c *chip }

func (p *fakeBusy) Get() (bool, error) { return p.c.sleeping, nil }
func (p *fakeBusy) Close() error       { return nil }

type fakeDIO1 struct {
	c     *chip
	edges chan struct{}
}

func (p *fakeDIO1) Get() (bool, error)     { return false, nil }
func (p *fakeDIO1) Edges() <-chan struct{} { return p.edges }
func (p *fakeDIO1) Close() error           { return nil }

// rig builds a Radio wired to a scripted chip, bypassing Open so each
// test scripts exactly the calls it makes. The busy timeout is shrunk
// so timeout paths run in milliseconds.
func rig(t *testing.T, steps []xfer) (*Radio, *chip) {
	t.Helper()
	c := &chip{t: t, steps: steps}
	pins := lora.Pins{
		Reset: &fakePin{c},
		Busy:  &fakeBusy{c},
		DIO1:  &fakeDIO1{c: c, edges: make(chan struct{}, 1)},
	}
	r := &Radio{dev: newDevice(&fakeSPI{c}, pins), now: time.Now}
	r.dev.busyTimeout = 5 * time.Millisecond
	return r, c
}

// openRig scripts a full Open against the fake chip and returns the
// resulting Radio with the remaining steps loaded.
func openRig(t *testing.T, cfg Config, rest []xfer) (*Radio, *chip) {
	t.Helper()
	c := &chip{t: t, steps: append(openScript(), rest...)}
	pins := lora.Pins{
		Reset: &fakePin{c},
		Busy:  &fakeBusy{c},
		DIO1:  &fakeDIO1{c: c, edges: make(chan struct{}, 1)},
	}
	r, err := Open(&fakeSPI{c}, pins, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.dev.busyTimeout = 5 * time.Millisecond
	return r, c
}

// openScript is the pinned bring-up transcript for a TCXO 1.8 V DC-DC
// board — the exact sequence validated on the air.
func openScript() []xfer {
	version := make([]byte, 20)
	for i := range 4 {
		version[i] = stOK // status bytes precede the register data
	}
	copy(version[4:], "SX1262")
	s := []xfer{
		{"post-reset flush: raw standby", []byte{0x80, 0x00}, nil},
		{"standby RC", []byte{0x80, 0x00}, nil},
		{"TCXO 1.8V, 640 ticks", []byte{0x97, 0x02, 0x00, 0x02, 0x80}, nil},
		{"clear device errors", []byte{0x07, 0x00, 0x00}, nil},
		{"calibrate all", []byte{0x89, 0x7F}, nil},
		{"calibration verdict", []byte{0x17, 0x00, 0x00, 0x00}, []byte{stOK, stOK, 0x00, 0x00}},
		{"regulator DC-DC", []byte{0x96, 0x01}, nil},
		{"packet type LoRa", []byte{0x8A, 0x01}, nil},
		{"fallback mode STDBY_RC", []byte{0x93, 0x20}, nil},
		{"clear device errors", []byte{0x07, 0x00, 0x00}, nil},
		{"standby XOSC (crystal proof)", []byte{0x80, 0x01}, nil},
		{"crystal verdict", []byte{0x17, 0x00, 0x00, 0x00}, []byte{stOK, stOK, 0x00, 0x00}},
		{"standby RC", []byte{0x80, 0x00}, nil},
		{"read version", append([]byte{0x1D, 0x03, 0x20}, make([]byte, 17)...), version},
	}
	return s
}

// meshcoreEU is the live-validated channel: MeshCore EU narrow.
func meshcoreEU() lora.Params {
	return lora.Params{
		Frequency: 869_618_000,
		SF:        lora.SF8,
		BW:        lora.BW62500,
		CR:        lora.CR8,
		Preamble:  32,
		SyncWord:  0x12,
		CRC:       true,
	}
}

// configureScript is the pinned transcript for Configure(meshcoreEU).
// The FRF bytes and the 0x14/0x24 sync-word encoding are the two facts
// most worth pinning: both were validated against the real mesh.
func configureScript() []xfer {
	return []xfer{
		{"standby RC", []byte{0x80, 0x00}, nil},
		{"packet type LoRa", []byte{0x8A, 0x01}, nil},
		{"clear device errors", []byte{0x07, 0x00, 0x00}, nil},
		{"calibrate image 863-870", []byte{0x98, 0xD7, 0xDB}, nil},
		{"frequency 869.618 MHz", []byte{0x86, 0x36, 0x59, 0xE3, 0x53}, nil},
		{"channel calibration verdict", []byte{0x17, 0x00, 0x00, 0x00}, []byte{stOK, stOK, 0x00, 0x00}},
		// The DS §6.1.6 band calibration, high-band column: the RMW on
		// 0x089C must touch bits 4:0 only.
		{"read RSSI meas cal H", []byte{0x1D, 0x08, 0x9C, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x21}},
		{"RSSI meas cal H (bits 4:0)", []byte{0x0D, 0x08, 0x9C, 0x21}, nil},
		{"RSSI meas cal L", []byte{0x0D, 0x08, 0x9D, 0x53}, nil},
		{"GFO/RST power threshold", []byte{0x0D, 0x08, 0xB9, 0x0A}, nil},
		{"AGC gain tune (high band: zeros)",
			[]byte{0x0D, 0x08, 0xF5, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, nil},
		{"modulation SF8/62.5k/CR4-8", []byte{0x8B, 0x08, 0x03, 0x04, 0x00}, nil},
		// Errata §15.1: bit 2 read back set — correct for every
		// bandwidth but 500 kHz, so no write follows.
		{"sensitivity config (errata 15.1)", []byte{0x1D, 0x08, 0x89, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x04}},
		{"packet params pre=32 explicit CRC", []byte{0x8C, 0x00, 0x20, 0x00, 0xFF, 0x01, 0x00}, nil},
		{"read IQ polarity (errata 15.4)", []byte{0x1D, 0x07, 0x36, 0x00, 0x00},
			[]byte{stOK, stOK, stOK, stOK, 0x0D}},
		// 0x0D already has bit 2 set: standard IQ needs no write.
		{"sync word 0x12 -> 14 24", []byte{0x0D, 0x07, 0x40, 0x14, 0x24}, nil},
		{"buffer base addresses", []byte{0x8F, 0x00, 0x00}, nil},
		// The shared gain byte carries the band's AgcSensiAdjust even
		// unboosted: 0x25<<2 = 0x94 on the high band.
		{"RX gain byte (high band, unboosted)", []byte{0x0D, 0x08, 0xAC, 0x94}, nil},
		{"retention slot count", []byte{0x0D, 0x02, 0x9F, 0x01}, nil},
		{"retention addr MSB", []byte{0x0D, 0x02, 0xA0, 0x08}, nil},
		{"retention addr LSB", []byte{0x0D, 0x02, 0xA1, 0xAC}, nil},
	}
}

// stepIndex finds a step by description; the scripts are data, and
// tests that vary one step should name it, not count offsets.
func stepIndex(steps []xfer, desc string) int {
	for i, st := range steps {
		if st.desc == desc {
			return i
		}
	}
	return -1
}

// startReceiveScript pins StartReceive: the wide latch mask with only
// RxDone routed to DIO1 — the mask/routing distinction that gives the
// line a real edge per frame.
func startReceiveScript() []xfer {
	return []xfer{
		{"clear RX flags (narrow)", []byte{0x02, 0x02, 0x7E}, nil},
		{"IRQ mask wide, DIO1=RxDone", []byte{0x08, 0x02, 0x7E, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00}, nil},
		{"RX continuous", []byte{0x82, 0xFF, 0xFF, 0xFF}, nil},
		// SetRx resets the shared gain byte; the driver rewrites it
		// after entering RX, every time.
		{"RX gain byte rewritten after SetRx", []byte{0x0D, 0x08, 0xAC, 0x94}, nil},
	}
}

func irqReadStep(flags IRQ) xfer {
	return xfer{"read IRQ", []byte{0x12, 0x00, 0x00, 0x00},
		[]byte{stOK, stOK, byte(flags >> 8), byte(flags & 0xFF)}}
}

func statusStep(st byte) xfer {
	return xfer{"get status", []byte{0xC0, 0x00}, []byte{st, st}}
}
