package sx126x

import (
	"context"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// rxMask is the full set of flags reception cares about. All of it is
// latched (the progress markers feed ReceiveInProgress), but only the
// terminal RxDone is routed to DIO1: the line must fire on "a frame is
// ready", not stand tall from the first preamble symbol — a level that
// rises early and stays up yields no edge for the event that matters.
const rxMask = irqOutcome | irqProgress

// pollFloor bounds how stale a lost DIO1 edge can leave us: the receive
// loop re-reads the chip's flags at least this often.
const pollFloor = 20 * time.Millisecond

// RxFrame is a received frame and what the radio measured about it.
type RxFrame struct {
	Payload []byte
	RSSI    float64       // dBm
	SNR     float64       // dB
	At      time.Time     // when the driver observed RxDone
	Airtime time.Duration // computed channel occupancy of this frame
}

// StartReceive arms continuous reception and leaves the radio there.
// Staying in RX is the default posture: a radio that is not
// transmitting should be listening.
//
// Arming resets reception state: leftover flags are cleared (narrowly —
// only the receive set, never a blanket sweep) and the IRQ routing is
// rewritten. StartReceive is the sole owner of that routing; nothing
// else may rewrite it without restoring it before returning.
func (r *Radio) StartReceive() error {
	if !r.ready {
		return ErrNotConfigured
	}
	if err := r.dev.clearIRQ(rxMask); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetDioIrqParams,
		byte(rxMask>>8), byte(rxMask&0xFF), // latch everything reception reads
		byte(IRQRxDone>>8), byte(IRQRxDone), // but only completion edges DIO1
		0, 0, 0, 0); err != nil {
		return err
	}
	if err := r.setRF(lora.RFReceive); err != nil {
		return err
	}
	r.progAnchor = time.Time{}
	r.progHeader = false
	// 0xFFFFFF selects continuous reception rather than a timeout.
	_, err := r.dev.cmd(opSetRx, 0xFF, 0xFF, 0xFF)
	return err
}

// Poll collects a finished reception if one is latched, without
// blocking: (nil, nil) means nothing has happened yet. This is the
// primitive an owner with several clocks builds its loop on; Receive
// wraps it for callers with only one thing to wait for.
//
// A frame failing its CRC surfaces as ErrCRC, a hopeless header as
// ErrHeader — both are expected traffic on a busy band, distinguishable
// with errors.Is, and worth counting: received-to-corrupt is the
// standard site-health ratio.
func (r *Radio) Poll() (*RxFrame, error) {
	if !r.ready {
		return nil, ErrNotConfigured
	}
	flags, err := r.dev.irqStatus()
	if err != nil {
		return nil, err
	}
	return r.collect(flags)
}

// collect interprets and consumes the outcome flags in the given word.
func (r *Radio) collect(flags IRQ) (*RxFrame, error) {
	switch {
	case flags&IRQCRCErr != 0:
		// Even with RxDone alongside: the payload is known-corrupt.
		return nil, r.finish(flags&rxMask, ErrCRC)

	case flags&IRQHeaderErr != 0 && flags&IRQHeaderValid == 0:
		// A HeaderErr next to a valid header is a leftover from an
		// earlier packet, not a verdict on this one (the reference
		// drivers guard this alias the same way) — that case falls
		// through to RxDone below or to "still arriving".
		return nil, r.finish(flags&(IRQHeaderErr|irqProgress), ErrHeader)

	case flags&IRQRxDone != 0:
		at := r.now()
		// Read everything about the frame BEFORE clearing: the buffer
		// status describes the last packet received, and clearing first
		// opens a window where a new arrival is misattributed.
		rx, err := r.dev.cmd(opGetRxBufferStatus, 0x00, 0x00, 0x00)
		if err != nil {
			return nil, err
		}
		length, offset := rx[2], rx[3]
		payload, err := r.readBuffer(offset, int(length))
		if err != nil {
			return nil, err
		}
		rssi, snr, err := r.packetStatus()
		if err != nil {
			return nil, err
		}
		if err := r.finish(flags&rxMask, nil); err != nil {
			return nil, err
		}
		return &RxFrame{
			Payload: payload,
			RSSI:    rssi,
			SNR:     snr,
			At:      at,
			Airtime: r.params.Airtime(len(payload)),
		}, nil

	case flags&IRQTimeout != 0:
		return nil, r.finish(flags&rxMask, ErrTimeout)

	case flags&IRQHeaderErr != 0:
		// Stale HeaderErr under a frame still arriving: shed the stale
		// bit alone and keep waiting for this frame's own outcome.
		if err := r.dev.clearIRQ(IRQHeaderErr); err != nil {
			return nil, err
		}
		return nil, nil //nolint:nilnil // Poll's documented "nothing yet"

	default:
		return nil, nil //nolint:nilnil // Poll's documented "nothing yet"
	}
}

// finish clears exactly the flags a collected outcome consumed and
// resets the progress clock, then returns outcome unchanged.
func (r *Radio) finish(flags IRQ, outcome error) error {
	if err := r.dev.clearIRQ(flags); err != nil {
		return err
	}
	r.progAnchor = time.Time{}
	r.progHeader = false
	return outcome
}

// Events exposes the DIO1 edge hint, for owners that select across
// several clocks: an edge says "call Poll", nothing more. It can be
// lossy — pair it with a periodic Poll so a lost edge costs latency,
// never an event.
func (r *Radio) Events() <-chan struct{} { return r.dev.pins.DIO1.Edges() }

// Receive waits for the next frame: Poll wrapped in edge-or-timer
// waiting, for callers with nothing else to schedule. The radio must be
// receiving (see StartReceive) and stays so afterwards, so frames can
// be read back to back.
func (r *Radio) Receive(ctx context.Context) (*RxFrame, error) {
	if !r.ready {
		return nil, ErrNotConfigured
	}
	// A quiet channel and a disarmed radio produce the same silence;
	// tell them apart up front rather than let the caller time out on a
	// chip that was never listening.
	mode, _, err := r.dev.status()
	if err != nil {
		return nil, err
	}
	if mode != ModeRx {
		// Latched outcomes are still collectable even out of RX.
		if frame, err := r.Poll(); frame != nil || err != nil {
			return frame, err
		}
		return nil, fmt.Errorf("%w: mode is %s", ErrNotReceiving, mode)
	}
	edges := r.Events()
	for {
		frame, err := r.Poll()
		if frame != nil || err != nil {
			return frame, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-edges:
		case <-time.After(pollFloor):
		}
	}
}

// ReceiveInProgress reports whether a frame is arriving: preamble means
// something tripped the detector, header means a frame is committed and
// worth protecting.
//
// The chip latches these markers but never ages them, so noise that
// trips the detector with no frame behind it would otherwise read as
// "busy" forever — and wedge every guarded operation, ResetAGC first.
// Stale state is therefore expired against the channel's own timing
// (preamble duration, then maximum frame airtime once a header is
// seen) and cleared, which also lets the DIO1 line fall.
func (r *Radio) ReceiveInProgress() (preamble, header bool, err error) {
	flags, err := r.dev.irqStatus()
	if err != nil {
		return false, false, err
	}
	return r.receiveProgress(flags)
}

// receiveProgress is ReceiveInProgress on an already-read flag word.
func (r *Radio) receiveProgress(flags IRQ) (preamble, header bool, err error) {
	prog := flags & irqProgress
	if prog == 0 {
		r.progAnchor = time.Time{}
		r.progHeader = false
		return false, false, nil
	}
	now := r.now()
	header = flags&IRQHeaderValid != 0
	if r.progAnchor.IsZero() || (header && !r.progHeader) {
		// First sighting, or the frame just committed to a header:
		// (re)anchor the clock for the next stage.
		r.progAnchor = now
		r.progHeader = header
	}
	window := r.params.PreambleDuration() + 12*r.params.SymbolDuration()
	if r.progHeader {
		window = r.worstCaseFrameWindow()
	}
	if now.Sub(r.progAnchor) > window {
		// No frame can still legitimately be in the air: the detector
		// was tripped by noise. Shed exactly the stale markers.
		if err := r.dev.clearIRQ(prog); err != nil {
			return false, false, err
		}
		r.progAnchor = time.Time{}
		r.progHeader = false
		return false, false, nil
	}
	return flags&(IRQPreambleDetected|IRQSyncWordValid) != 0, header, nil
}

// CAD describes one channel-activity scan. The zero value is the
// baseline: 4 symbols, Semtech's recommended detection thresholds for
// the configured spreading factor.
//
// Every field is per call on purpose: adaptive listen-before-talk
// schemes calibrate DetectPeak against the site — probing neighbouring
// values around an operating point and stepping toward the false-
// positive knee — and that requires overriding the threshold scan by
// scan. The driver therefore imposes no clamp and no policy; it
// executes the scan it is given. What the integrator should know about
// the scale: the register is 8 bits but the useful SX126x range is
// roughly 18-32, values much above 40 leave CAD effectively blind
// (listen-before-talk silently off), values well below the base make
// every scan read busy, and thresholds quoted for the LR11xx family
// live on a different chip's correlation scale entirely.
type CAD struct {
	// Symbols is how many symbols the scan listens for: 1, 2, 4, 8 or
	// 16. More symbols, more reliable, longer. 0 means 4 — better
	// mid-payload detection than shorter scans, and most of a frame's
	// airtime is payload.
	Symbols uint8

	// DetectPeak is the correlator's peak-to-noise threshold — a
	// despreader quantity, not a dBm level. 0 means CADBasePeak of the
	// configured spreading factor. Higher = hears only strong signals.
	DetectPeak byte

	// DetectMin is the floor under the peak search. 0 means 10,
	// Semtech's universal recommendation; adaptive schemes leave it
	// alone.
	DetectMin byte
}

// CADBasePeak is Semtech's recommended detection peak for a spreading
// factor (AN1200.48): the reference point adaptive schemes offset from.
func CADBasePeak(sf lora.SpreadingFactor) byte { return byte(sf) + 13 }

// resolve fills the defaults in and maps the symbol count to the
// chip's encoding.
func (c CAD) resolve(sf lora.SpreadingFactor) (symbols, peak, minimum byte, err error) {
	switch c.Symbols {
	case 0, 4:
		symbols = 0x02
	case 1:
		symbols = 0x00
	case 2:
		symbols = 0x01
	case 8:
		symbols = 0x03
	case 16:
		symbols = 0x04
	default:
		return 0, 0, 0, fmt.Errorf("%w: CAD over %d symbols (1, 2, 4, 8 or 16)",
			ErrBadConfig, c.Symbols)
	}
	peak = c.DetectPeak
	if peak == 0 {
		peak = CADBasePeak(sf)
	}
	minimum = c.DetectMin
	if minimum == 0 {
		minimum = 10
	}
	return symbols, peak, minimum, nil
}

// symbolCount recovers the listen length from the resolved encoding.
func (c CAD) symbolCount() int {
	if c.Symbols == 0 {
		return 4
	}
	return int(c.Symbols)
}

// worstCaseFrameWindow bounds how long a committed frame can still
// legitimately be in the air. With an explicit header the coding rate
// travels IN the header, per frame: a channel configured at 4/5 can
// carry a 4/8 frame, so the bound must assume the slowest rate and the
// longest payload, whatever Configure said. Deliberately generous — a
// margin on a safety net costs nothing, while expiring early would let
// a destructive operation start on top of a frame that is still
// arriving.
func (r *Radio) worstCaseFrameWindow() time.Duration {
	p := r.params
	p.CR = lora.CR8
	p.CRC = true
	w := p.MaxFrameDuration(255)
	return w + w/4 + 100*time.Millisecond
}

// AssessChannel performs a channel activity detection: a short listen
// answering "is a LoRa transmission under way right now?". It reports
// true when the channel is busy. Far cheaper than a full reception —
// tens of milliseconds — which is what makes listen-before-talk
// affordable. The zero CAD scans with the recommended defaults; see
// CAD for per-call tuning.
//
// CAD borrows the radio: the chip leaves RX for the scan and the IRQ
// routing is temporarily CAD's. Both are restored before returning, on
// every path — if the radio was receiving it is receiving again
// afterwards, re-armed by StartReceive; otherwise it ends in standby.
// While a frame is arriving (ErrReceiveInProgress) or unread
// (ErrUnreadFrame) the scan refuses instead of destroying it: the scan
// that kills the reception it was protecting is the classic
// listen-before-talk self-sabotage.
func (r *Radio) AssessChannel(ctx context.Context, cad CAD) (busy bool, err error) {
	if !r.ready {
		return false, ErrNotConfigured
	}
	symbols, peak, minimum, err := cad.resolve(r.params.SF)
	if err != nil {
		return false, err
	}
	if err := r.guardDestructive(); err != nil {
		return false, err
	}
	mode, _, err := r.dev.status()
	if err != nil {
		return false, err
	}
	wasRX := mode == ModeRx

	// Whatever happens below, hand the radio back the way we found it.
	defer func() {
		if wasRX {
			if rerr := r.StartReceive(); rerr != nil && err == nil {
				err = rerr
			}
		}
	}()

	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return false, err
	}
	if _, err := r.dev.cmd(opSetCadParams,
		symbols, peak, minimum,
		0x00 /* exit to standby */, 0x00, 0x00, 0x00); err != nil {
		return false, err
	}
	const cadFlags = IRQCadDone | IRQCadDetected
	if err := r.dev.clearIRQ(cadFlags); err != nil {
		return false, err
	}
	if _, err := r.dev.cmd(opSetDioIrqParams,
		byte(cadFlags>>8), byte(cadFlags&0xFF), byte(cadFlags>>8), byte(cadFlags&0xFF),
		0x00, 0x00, 0x00, 0x00); err != nil {
		return false, err
	}
	if err := r.setRF(lora.RFReceive); err != nil {
		return false, err
	}
	if _, err := r.dev.cmd(opSetCad); err != nil {
		return false, err
	}

	// A CAD is bounded by construction; the ceiling only guards against
	// a chip that never answers.
	deadline := r.params.SymbolDuration()*time.Duration(cad.symbolCount()+8) + 100*time.Millisecond
	cadCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	flags, err := r.waitIRQ(cadCtx, IRQCadDone, 2*time.Millisecond)
	if err != nil {
		return false, fmt.Errorf("sx126x: CAD: %w", err)
	}
	if err := r.dev.clearIRQ(flags & cadFlags); err != nil {
		return false, err
	}
	return flags&IRQCadDetected != 0, nil
}

// RSSI reads the instantaneous signal strength on the channel. It is
// only meaningful while the radio is receiving — and, for a noise-floor
// reading, only when no frame is arriving; sample around
// ReceiveInProgress if that distinction matters.
func (r *Radio) RSSI() (float64, error) {
	if !r.ready {
		return 0, ErrNotConfigured
	}
	mode, _, err := r.dev.status()
	if err != nil {
		return 0, err
	}
	if mode != ModeRx {
		return 0, fmt.Errorf("%w: mode is %s", ErrNotReceiving, mode)
	}
	rx, err := r.dev.cmd(opGetRssiInst, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return -float64(rx[2]) / 2, nil
}

func (r *Radio) readBuffer(offset byte, n int) ([]byte, error) {
	args := make([]byte, 2+n)
	args[0] = offset
	rx, err := r.dev.cmd(opReadBuffer, args...)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, rx[3:])
	return out, nil
}

// packetStatus reads the quality of the last frame: RSSI in half-dBm
// steps, SNR in quarter-dB steps and signed.
func (r *Radio) packetStatus() (rssi, snr float64, err error) {
	rx, err := r.dev.cmd(opGetPacketStatus, 0x00, 0x00, 0x00, 0x00)
	if err != nil {
		return 0, 0, err
	}
	return -float64(rx[2]) / 2, float64(int8(rx[3])) / 4, nil
}
