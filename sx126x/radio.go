// Package sx126x drives Semtech SX1261/1262/1268 LoRa transceivers over
// SPI on a host that owns the reset, BUSY and DIO1 lines.
//
// A Radio is NOT safe for concurrent use, by design rather than by
// omission: the chip has one bus, one interrupt line and one set of
// latched flags, so exactly one goroutine must own it. Serialising
// access anywhere else — a lock inside a method, a callback on another
// thread — reintroduces the races this shape exists to prevent.
//
// The chip's own flags are the source of truth throughout, with one
// documented exception: the flags are latched, not timed, so telling "a
// frame is arriving" from "noise tripped the detector an hour ago"
// needs a clock. ReceiveInProgress keeps that one timestamp and expires
// stale detector state; everything else is asked of the chip.
package sx126x

import (
	"context"
	"errors"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// TCXOVoltage selects the supply the chip provides to a temperature-
// compensated oscillator through DIO3. The zero value means the module
// has a plain crystal and no TCXO — the only assumption safe to make
// by default.
type TCXOVoltage uint8

// TCXO supply voltages.
const (
	TCXONone TCXOVoltage = iota
	TCXO1V6
	TCXO1V7
	TCXO1V8
	TCXO2V2
	TCXO2V4
	TCXO2V7
	TCXO3V0
	TCXO3V3
)

// code maps the voltage to the chip's encoding (datasheet table 13-35).
func (v TCXOVoltage) code() (byte, bool) {
	switch v {
	case TCXO1V6:
		return 0x00, true
	case TCXO1V7:
		return 0x01, true
	case TCXO1V8:
		return 0x02, true
	case TCXO2V2:
		return 0x03, true
	case TCXO2V4:
		return 0x04, true
	case TCXO2V7:
		return 0x05, true
	case TCXO3V0:
		return 0x06, true
	case TCXO3V3:
		return 0x07, true
	default:
		return 0, false
	}
}

// Config describes the board, as opposed to the channel: the things
// that depend on how the chip is wired rather than on who we talk to.
type Config struct {
	// TCXO, when not TCXONone, is powered from DIO3. Getting the
	// voltage wrong is silent and total: the crystal never starts,
	// every RF operation fails, and only the device-error word says
	// why — which is why Open proves the crystal before returning.
	TCXO        TCXOVoltage
	TCXOTimeout time.Duration // crystal settling allowance; 10 ms is ample

	// DIO2AsRFSwitch lets the chip drive the antenna switch itself. On
	// boards that route the switch to the host instead, leave it false
	// and provide Pins.RF.
	DIO2AsRFSwitch bool

	// UseDCDC selects the DC-DC regulator over the LDO: less current at
	// the cost of needing the inductor the module may or may not have.
	UseDCDC bool

	// RXBoostedGain trades current for sensitivity in the low-noise
	// amplifier (datasheet 9.6). Worth it on a mains-powered repeater.
	RXBoostedGain bool

	// UndocumentedRXPatch sets bit 0 of register 0x08B5, a setting that
	// appears in no Semtech datasheet. It reached the mesh firmwares as
	// a Semtech recommendation relayed outside the documentation, and
	// both MeshCore and Meshtastic apply it — MeshCore notably on boards
	// with an SKY66122 front end.
	//
	// Nobody documents what it actually does. It is off by default here
	// for that reason: enable it deliberately, and ideally measure the
	// difference on your own hardware rather than take it on faith. On
	// one SKY66122 board it moved the idle noise floor by no measurable
	// amount, while the documented boosted gain moved it by 1 dB.
	UndocumentedRXPatch bool
}

// Radio is a configured transceiver.
type Radio struct {
	dev     device
	cfg     Config
	params  lora.Params
	ready   bool
	version string

	// The one piece of software state (see the package comment): when
	// the reception-progress detector first fired, and whether a valid
	// header has been seen since. Everything else is read off the chip.
	progAnchor time.Time
	progHeader bool

	// now is swappable so tests can drive the expiry clock.
	now func() time.Time
}

// Open takes ownership of the transports, resets the chip, applies the
// board configuration and leaves it in standby. It verifies that the
// crystal actually starts, so a TCXO mistake surfaces here rather than
// as an unexplained silence later; a bus with nothing on it fails on
// the first command instead of succeeding its way into a deaf Radio.
//
// From here on the Radio owns spi and pins: Close releases them.
func Open(spi lora.SPI, pins lora.Pins, cfg Config) (*Radio, error) {
	if pins.Reset == nil || pins.Busy == nil || pins.DIO1 == nil {
		return nil, fmt.Errorf("%w: Reset, Busy and DIO1 pins are required", ErrBadConfig)
	}
	if cfg.TCXO != TCXONone {
		if _, ok := cfg.TCXO.code(); !ok {
			return nil, fmt.Errorf("%w: unknown TCXO voltage %d", ErrBadConfig, cfg.TCXO)
		}
	}
	if cfg.TCXOTimeout <= 0 {
		cfg.TCXOTimeout = 10 * time.Millisecond
	}
	r := &Radio{dev: newDevice(spi, pins), cfg: cfg, now: time.Now}

	// First contact tolerates a marginal power-up: a chip still
	// stabilising can miss the first exchange, and a reset-and-retry
	// turns a capricious cold start into a reliable one (the reference
	// driver retries up to ten times for the same reason). Only the
	// nobody-home pattern is retried — a real failure, a TCXO verdict,
	// a calibration error all carry information and surface at once.
	const openAttempts = 3
	if err := r.initChip(); err != nil {
		for attempt := 1; ; attempt++ {
			if !errors.Is(err, ErrNoDevice) || attempt >= openAttempts {
				return nil, err
			}
			time.Sleep(10 * time.Millisecond)
			if err = r.initChip(); err == nil {
				break
			}
		}
	}

	// Read the version area — as a liveness probe, not an identity
	// (see Version for why it cannot be one). Only patterns a working
	// chip cannot produce are fatal.
	raw, err := r.dev.readRegister(regVersionString, 16)
	if err != nil {
		return nil, err
	}
	r.version = versionString(raw)

	return r, nil
}

// initChip is the board bring-up shared by Open and Reset: everything
// between a hardware reset and a chip proven ready in standby.
func (r *Radio) initChip() error {
	if err := r.dev.reset(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}

	if err := r.setupTCXO(); err != nil {
		return err
	}
	if r.cfg.DIO2AsRFSwitch {
		if _, err := r.dev.cmd(opSetDio2AsRfSwitch, 0x01); err != nil {
			return err
		}
	}
	reg := byte(0x00) // LDO
	if r.cfg.UseDCDC {
		reg = 0x01
	}
	if _, err := r.dev.cmd(opSetRegulatorMode, reg); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetPacketType, packetTypeLoRa); err != nil {
		return err
	}

	// Prove the crystal runs: force it on and read the verdict. BUSY
	// covers the oscillator start, so no fixed delay is needed. Without
	// this, a bad TCXO setting only shows up as every later operation
	// quietly doing nothing.
	if err := r.dev.clearDeviceErrors(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyXOSC); err != nil {
		return err
	}
	if err := r.dev.checkDeviceErrors("crystal start (check the TCXO voltage)"); err != nil {
		return err
	}
	_, err := r.dev.cmd(opSetStandby, standbyRC)
	return err
}

// setupTCXO powers the oscillator from DIO3 and calibrates against it.
func (r *Radio) setupTCXO() error {
	code, ok := r.cfg.TCXO.code()
	if !ok {
		return nil // bare crystal: nothing to power
	}
	// Timeout counts in 15.625 us ticks, 24 bits wide.
	ticks := min(uint64(r.cfg.TCXOTimeout/(15625*time.Nanosecond)), 0xFFFFFF)
	if _, err := r.dev.cmd(opSetDio3AsTcxoCtrl, code,
		byte(ticks>>16), byte(ticks>>8), byte(ticks)); err != nil {
		return err
	}
	// A reset leaves the crystal error latched from before the TCXO
	// was configured; clear it so the verdicts below are fresh.
	if err := r.dev.clearDeviceErrors(); err != nil {
		return err
	}
	// Calibration reports failure only through the error word — the
	// command itself "succeeds" on the bus whatever happens. BUSY stays
	// high for the duration, so the next command's handshake is the
	// wait.
	if _, err := r.dev.cmd(opCalibrate, 0x7F); err != nil {
		return err
	}
	return r.dev.checkDeviceErrors("calibration")
}

// versionString extracts the printable prefix of the version area.
func versionString(raw []byte) string {
	end := 0
	for end < len(raw) && raw[end] >= 0x20 && raw[end] < 0x7F {
		end++
	}
	return string(raw[:end])
}

// Version returns whatever the chip's version register holds. It
// proves something answered the bus and nothing more: genuine SX1262
// silicon widely reports "SX1261 V2D 2D02" here — a known hardware
// quirk (RadioLib issue #683) the reference driver also works around —
// and clones may leave the area blank. Never derive the part variant
// from it: transmit-path decisions like the PA configuration tables
// must come from the integrator's declaration, because on that choice
// the two parts differ enough to destroy a front end.
func (r *Radio) Version() string { return r.version }

// Configure applies a channel: frequency, modulation and framing. It
// may be called again to retune.
//
// Post-condition: the radio is in standby — retuning always disarms
// reception, so call StartReceive after. While a frame is arriving or
// unread, Configure refuses rather than destroy it.
func (r *Radio) Configure(p lora.Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Frequency < 150_000_000 || p.Frequency > 960_000_000 {
		return fmt.Errorf("%w: %d Hz is outside the SX126x synthesiser range (150-960 MHz)",
			ErrBadConfig, p.Frequency)
	}
	if r.ready {
		if err := r.guardDestructive(); err != nil {
			return err
		}
	}
	if err := r.applyChannel(p); err != nil {
		return err
	}
	r.params = p
	r.ready = true
	return nil
}

// applyChannel programs a channel onto the chip. It is separate from
// Configure so a reset can replay it verbatim: after a calibration the
// safe assumption is that everything is gone, not just the parts the
// datasheet mentions.
func (r *Radio) applyChannel(p lora.Params) error {
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetPacketType, packetTypeLoRa); err != nil {
		return err
	}

	// Image calibration is per band and must precede the first use of a
	// frequency; skipping it costs sensitivity that is hard to trace.
	if err := r.dev.clearDeviceErrors(); err != nil {
		return err
	}
	lo, hi := imageCalibrationBand(p.Frequency)
	if _, err := r.dev.cmd(opCalibrateImage, lo, hi); err != nil {
		return err
	}

	frf := uint32(uint64(p.Frequency) * (1 << 25) / xtalHz)
	if _, err := r.dev.cmd(opSetRfFrequency,
		byte(frf>>24), byte(frf>>16), byte(frf>>8), byte(frf)); err != nil {
		return err
	}
	// Image calibration and PLL programming judge themselves only
	// through the error word; read the verdict for both.
	if err := r.dev.checkDeviceErrors("channel calibration"); err != nil {
		return err
	}
	if err := r.applyAGCBandCal(p.Frequency); err != nil {
		return err
	}

	ldro := byte(0)
	if p.LowDataRateOptimize() {
		ldro = 1
	}
	if _, err := r.dev.cmd(opSetModulationParams,
		byte(p.SF), bandwidthCode(p.BW), byte(p.CR)-4, ldro); err != nil {
		return err
	}

	if err := r.applySensitivityFix(p.BW); err != nil {
		return err
	}
	if err := r.setPacketParams(p, rxPayloadLen(p)); err != nil {
		return err
	}

	if err := r.dev.writeRegister(regLoRaSyncWordMSB,
		p.SyncWord&0xF0|0x04, p.SyncWord<<4|0x04); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetBufferBaseAddress, 0x00, 0x00); err != nil {
		return err
	}

	// Any calibration can clear the receiver tuning, and an image
	// calibration ran above, so re-apply rather than assume.
	return r.applyRXTuning(p.Frequency)
}

// agcBandCal is one column of DS §6.1.6 Table 6-4: the RSSI/AGC
// calibration for a frequency band.
type agcBandCal struct {
	rssiMeasCalH byte // regAgcRssiMeasCalH, bits 4:0
	rssiMeasCalL byte
	gforstPowThr byte
	sensiAdjust  byte // regRxGain bits 7:2
	gainTune     [7]byte
}

// The chip ships calibrated for 868-915 MHz, which is why the high-band
// gain tunes are zero: writing them is a no-op on fresh silicon and
// exists so a runtime band change restores them after the low-band set
// was installed.
var agcCalHigh = agcBandCal{
	rssiMeasCalH: 0x01, rssiMeasCalL: 0x53, gforstPowThr: 0x0A,
	sensiAdjust: 0x25,
}

// The low-band column is the datasheet's 470-490 MHz calibration —
// Semtech publishes no 433 MHz column. Below 600 MHz it is the nearest
// published data and beats leaving the 868-915 tuning installed, but a
// true 433 MHz figure would need the DS §6.1.6 generator sweep on real
// hardware; do not read these numbers as characterised for 433.
var agcCalLow = agcBandCal{
	rssiMeasCalH: 0x01, rssiMeasCalL: 0x27, gforstPowThr: 0x04,
	sensiAdjust: 0x22,
	gainTune:    [7]byte{0xDE, 0xE2, 0x32, 0x44, 0x33, 0x34, 0x04},
}

// agcCalFor picks the calibration column. The datasheet characterises
// only 470-490 and 868-915; 600 MHz splits the 433/470 group from the
// 779/868/915 group cleanly.
func agcCalFor(freq uint32) *agcBandCal {
	if freq < 600_000_000 {
		return &agcCalLow
	}
	return &agcCalHigh
}

// applyAGCBandCal installs the band's RSSI/AGC calibration — everything
// except the shared 0x08AC byte, which rxGainByte folds into the gain
// writes.
func (r *Radio) applyAGCBandCal(freq uint32) error {
	cal := agcCalFor(freq)
	// Bits 4:0 only — preserve the rest of the register.
	cur, err := r.dev.readRegister(regAgcRssiMeasCalH, 1)
	if err != nil {
		return err
	}
	if err := r.dev.writeRegister(regAgcRssiMeasCalH,
		cur[0]&^0x1F|cal.rssiMeasCalH&0x1F); err != nil {
		return err
	}
	if err := r.dev.writeRegister(regAgcRssiMeasCalL, cal.rssiMeasCalL); err != nil {
		return err
	}
	if err := r.dev.writeRegister(regAgcGforstPowThr, cal.gforstPowThr); err != nil {
		return err
	}
	return r.dev.writeRegister(regAgcGainTune, cal.gainTune[:]...)
}

// rxGainByte builds the shared 0x08AC byte: the band's AgcSensiAdjust
// in bits 7:2, the gain mode in bits 1:0.
func rxGainByte(freq uint32, boosted bool) byte {
	b := agcCalFor(freq).sensiAdjust << 2
	if boosted {
		b |= 0x02
	}
	return b
}

// applySensitivityFix applies errata §15.1: bit 2 of the sensitivity
// register must be cleared for LoRa at 500 kHz and set for every other
// bandwidth — a channel that once ran at 500 kHz would otherwise leave
// the workaround stuck on. Written only when the value changes.
func (r *Radio) applySensitivityFix(bw lora.Bandwidth) error {
	cur, err := r.dev.readRegister(regSensitivityConfig, 1)
	if err != nil {
		return err
	}
	sens := cur[0] | 0x04
	if bw == lora.BW500000 {
		sens = cur[0] &^ 0x04
	}
	if sens == cur[0] {
		return nil
	}
	return r.dev.writeRegister(regSensitivityConfig, sens)
}

// rxPayloadLen is the length programmed for reception: the agreed frame
// size with an implicit header, "accept anything" with an explicit one.
// The transmit path must override this per frame.
func rxPayloadLen(p lora.Params) byte {
	if p.ImplicitHeader {
		return p.PayloadLength
	}
	return 0xFF
}

// applyRXTuning writes the receiver settings that are not part of the
// modulation: the shared gain/AgcSensiAdjust byte and the undocumented
// patch. Silently cleared by calibration, so this runs after every one
// — and the gain byte is rewritten again after each SetRx, which
// resets it (see StartReceive).
func (r *Radio) applyRXTuning(freq uint32) error {
	if err := r.dev.writeRegister(regRxGain, rxGainByte(freq, r.cfg.RXBoostedGain)); err != nil {
		return err
	}
	// Point the warm-sleep retention list at the gain register so the
	// byte survives chip-internal sleep transitions (datasheet 9.6);
	// without this a wake-up silently costs sensitivity.
	if err := r.dev.writeRegister(regRetention0, 0x01); err != nil {
		return err
	}
	if err := r.dev.writeRegister(regRetention1, byte(regRxGain>>8)); err != nil {
		return err
	}
	if err := r.dev.writeRegister(regRetention2, byte(regRxGain&0xFF)); err != nil {
		return err
	}
	if r.cfg.UndocumentedRXPatch {
		// Read-modify-write: only bit 0 is ours to touch, and whatever
		// else lives in this register is unknown territory.
		cur, err := r.dev.readRegister(regRXPatch, 1)
		if err != nil {
			return err
		}
		if err := r.dev.writeRegister(regRXPatch, cur[0]|0x01); err != nil {
			return err
		}
	}
	return nil
}

// guardDestructive refuses an operation that would destroy reception
// state: an unread frame (collect it with Poll first) or a frame
// arriving right now (try again shortly). Stale detector state is
// expired rather than honoured, so a noise-tripped latch cannot wedge
// the caller forever.
func (r *Radio) guardDestructive() error {
	flags, err := r.dev.irqStatus()
	if err != nil {
		return err
	}
	if flags&irqOutcome != 0 {
		return ErrUnreadFrame
	}
	pre, hdr, err := r.receiveProgress(flags)
	if err != nil {
		return err
	}
	if pre || hdr {
		return ErrReceiveInProgress
	}
	return nil
}

// ResetAGC restarts the receiver's analogue front end.
//
// Some sites need this periodically: the automatic gain control can
// settle after a strong local burst and never recover on its own,
// leaving the node deaf with no error to show for it. Repeaters
// commonly schedule it every few seconds.
//
// It refuses while a frame is arriving (ErrReceiveInProgress) or
// unread (ErrUnreadFrame) — try again later rather than sacrifice the
// reception. If the radio was armed for receive it is re-armed before
// returning, so a periodic caller needs no follow-up; otherwise it is
// left in standby.
//
// Calibration silently clears settings that have nothing to do with
// calibration — the image band reverts to 902-928 MHz, the receiver
// tuning is wiped — so the whole channel is replayed from the stored
// configuration rather than guessing at what survived.
func (r *Radio) ResetAGC() error {
	if !r.ready {
		return ErrNotConfigured
	}
	if err := r.guardDestructive(); err != nil {
		return err
	}
	mode, _, err := r.dev.status()
	if err != nil {
		return err
	}
	wasRX := mode == ModeRx

	if _, err := r.dev.cmd(opSetSleep, 0x04); err != nil { // warm sleep
		return err
	}
	time.Sleep(time.Millisecond) // let the analogue front end actually drop
	if err := r.dev.wake(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}
	if err := r.dev.clearDeviceErrors(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opCalibrate, 0x7F); err != nil {
		return err
	}
	if err := r.dev.checkDeviceErrors("AGC reset calibration"); err != nil {
		return err
	}

	if r.cfg.DIO2AsRFSwitch {
		if _, err := r.dev.cmd(opSetDio2AsRfSwitch, 0x01); err != nil {
			return err
		}
	}
	// Replay the entire channel. Working out exactly which settings a
	// calibration clears is a losing game — the datasheet is silent, and
	// a single forgotten one leaves a radio that looks configured and
	// hears nothing. Reprogramming costs a handful of SPI commands.
	if err := r.applyChannel(r.params); err != nil {
		return err
	}
	if wasRX {
		return r.StartReceive()
	}
	return nil
}

// setPacketParams programs the framing. payloadLen is the reception
// length here; the future transmit path rewrites it per frame, as the
// chip requires.
func (r *Radio) setPacketParams(p lora.Params, payloadLen byte) error {
	header := byte(0x00) // variable length (explicit header)
	if p.ImplicitHeader {
		header = 0x01
	}
	crc := byte(0x00)
	if p.CRC {
		crc = 0x01
	}
	iq := byte(0x00) // standard
	if p.InvertIQ {
		iq = 0x01
	}
	if _, err := r.dev.cmd(opSetPacketParams,
		byte(p.Preamble>>8), byte(p.Preamble), header, payloadLen, crc, iq); err != nil {
		return err
	}

	// Errata §15.4: bit 2 of the IQ polarity register must be cleared
	// for inverted IQ and set for standard, or inverted-IQ reception is
	// quietly degraded. The flag alone is not enough.
	cur, err := r.dev.readRegister(regIQPolarity, 1)
	if err != nil {
		return err
	}
	fixed := cur[0] | 0x04
	if p.InvertIQ {
		fixed = cur[0] &^ 0x04
	}
	if fixed != cur[0] {
		return r.dev.writeRegister(regIQPolarity, fixed)
	}
	return nil
}

// Params returns the configured channel, exactly as programmed — there
// are no silent substitutions between Configure and the chip.
func (r *Radio) Params() lora.Params { return r.params }

// Status reports the chip's current mode and the outcome of the last
// command — a direct read, never a cached value.
func (r *Radio) Status() (ChipMode, CommandStatus, error) { return r.dev.status() }

// Sleep puts the chip in its lowest-power warm-start state.
//
// Asleep, the chip inverts the BUSY handshake, so every ordinary
// command — Close included — would time out against it: the ONLY valid
// next call on this Radio is Wake (or Close, which wakes internally).
func (r *Radio) Sleep() error {
	_, err := r.dev.cmd(opSetSleep, 0x04) // warm start, no RTC wake
	return err
}

// Wake brings the chip out of Sleep and replays the channel: warm-start
// retention preserves some configuration, and guessing which part is
// the classic way to get a radio that looks configured and hears
// nothing. Post-condition: standby; call StartReceive to listen again.
func (r *Radio) Wake() error {
	if err := r.dev.wake(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}
	if r.ready {
		return r.applyChannel(r.params)
	}
	return nil
}

// Standby aborts whatever the chip is doing and parks it in standby.
//
// This is deliberately unguarded — it is the abort button, and an abort
// that can refuse is not one. A reception in progress dies here;
// callers that want to protect one should consult ReceiveInProgress
// (never the BUSY line, which is the command handshake and reads idle
// throughout a reception) and Poll for unread frames first.
func (r *Radio) Standby() error {
	_, err := r.dev.cmd(opSetStandby, standbyRC)
	return err
}

// Reset pulls NRESET, brings the chip back up and replays the channel
// if one was configured. This is the recovery path ErrBusyTimeout
// calls for: it assumes nothing about the chip's state, because a chip
// that stopped answering has none worth trusting.
// Post-condition: standby; call StartReceive to listen again.
func (r *Radio) Reset() error {
	if err := r.initChip(); err != nil {
		return err
	}
	if r.ready {
		return r.applyChannel(r.params)
	}
	return nil
}

// Close puts the chip to sleep and releases the SPI bus and pins the
// Radio took ownership of at Open. It wakes the chip first so a Radio
// left sleeping still shuts down cleanly.
func (r *Radio) Close() error {
	// Wake is a bare transfer: harmless when awake, and the only thing
	// that unblocks the handshake when asleep.
	err := r.dev.wake()
	if serr := r.Sleep(); serr != nil {
		err = errors.Join(err, serr)
	}
	if cerr := r.dev.spi.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if perr := r.dev.pins.Close(); perr != nil {
		err = errors.Join(err, perr)
	}
	return err
}

// waitIRQ blocks until one of want's flags is latched, using the DIO1
// edge as a hint and the chip's flags as the answer. The poll floor
// means a missed edge costs latency, never the event. Flags are read
// before the context is consulted, so an event that already happened is
// delivered even to a cancelled caller.
func (r *Radio) waitIRQ(ctx context.Context, want IRQ, poll time.Duration) (IRQ, error) {
	edges := r.dev.pins.DIO1.Edges()
	for {
		flags, err := r.dev.irqStatus()
		if err != nil {
			return 0, err
		}
		if flags&want != 0 {
			return flags, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-edges:
		case <-time.After(poll):
		}
	}
}

// setRF steers the antenna switch when the host owns one; boards where
// the chip drives it from DIO2 need nothing here.
func (r *Radio) setRF(m lora.RFMode) error {
	if r.dev.pins.RF == nil {
		return nil
	}
	return r.dev.pins.RF.Set(m)
}

// imageCalibrationBand returns the calibration window covering freq:
// the published pair when the frequency falls in one of the datasheet's
// characterised bands (table 9-2), otherwise a window computed around
// the frequency itself — the encoding is 4 MHz per step, and any
// frequency the synthesiser accepts deserves a calibrated image, not
// the nearest band's leftovers.
func imageCalibrationBand(freq uint32) (lo, hi byte) {
	switch {
	case freq >= 902_000_000 && freq <= 928_000_000:
		return 0xE1, 0xE9
	case freq >= 863_000_000 && freq <= 870_000_000:
		return 0xD7, 0xDB
	case freq >= 779_000_000 && freq <= 787_000_000:
		return 0xC1, 0xC5
	case freq >= 470_000_000 && freq <= 510_000_000:
		return 0x75, 0x81
	case freq >= 430_000_000 && freq <= 440_000_000:
		return 0x6B, 0x6F
	default:
		mhz := freq / 1_000_000
		return byte((mhz - 4) / 4), byte((mhz + 4) / 4)
	}
}

// bandwidthCode maps a bandwidth to the chip's encoding (table 13-47).
func bandwidthCode(bw lora.Bandwidth) byte {
	switch bw {
	case lora.BW7810:
		return 0x00
	case lora.BW10420:
		return 0x08
	case lora.BW15630:
		return 0x01
	case lora.BW20830:
		return 0x09
	case lora.BW31250:
		return 0x02
	case lora.BW41670:
		return 0x0A
	case lora.BW62500:
		return 0x03
	case lora.BW125000:
		return 0x04
	case lora.BW250000:
		return 0x05
	default:
		return 0x06 // 500 kHz
	}
}
