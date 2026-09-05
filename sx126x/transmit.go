package sx126x

import (
	"context"
	"errors"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// ChipVariant is the exact part on the board. It decides the power-
// amplifier configuration tables, where the SX1261's low-power PA and
// the SX1262/68's high-power PA differ enough that driving one with the
// other's settings is the classic way to destroy a front end.
//
// It must be declared by the integrator: the chip's version register
// cannot be trusted for this — genuine SX1262 silicon widely reports
// "SX1261" there (see Version).
type ChipVariant uint8

// Chip variants. The zero value is deliberately "undeclared": receive
// works without it, transmit refuses until the integrator commits.
const (
	ChipUnset ChipVariant = iota
	SX1261
	SX1262
	SX1268
)

// String implements fmt.Stringer.
func (v ChipVariant) String() string {
	switch v {
	case SX1261:
		return "SX1261"
	case SX1262:
		return "SX1262"
	case SX1268:
		return "SX1268"
	default:
		return "unset"
	}
}

// PowerRange is the chip-side output range in dBm: what this part can
// actually be programmed for, whatever ceiling an integrator declares
// on top of it. Exported so a caller can judge a configured ceiling —
// or a power resolved from one — before a transmission discovers it.
func (v ChipVariant) PowerRange() (minDBm, maxDBm int8) {
	if v == SX1261 {
		return -17, 15
	}
	return -9, 22
}

// MaxTxPowerZero expresses a transmit ceiling of exactly 0 dBm, since a
// zero Config.MaxTxPower means "transmit disabled".
const MaxTxPowerZero int8 = -128

// TxResult is one completed transmission.
//
// A non-nil result beside a non-nil error is a frame that reached the
// air and a radio that then failed to put itself away: the emission
// happened, and an integrator charging a regulatory budget owes it
// the airtime whatever the chip did afterwards. A nil result means
// nothing was radiated.
type TxResult struct {
	At       time.Time     // when TxDone was observed
	Airtime  time.Duration // computed channel occupancy
	Duration time.Duration // measured SetTx→TxDone; drifting from Airtime signals a sick radio
	// PowerDBm is the power the chip was actually programmed for —
	// what a regulatory ledger records, independent of caller
	// bookkeeping.
	PowerDBm int8
}

// paEntry is one operating point of the high-power PA: the PA duty
// cycle and HP max for SetPaConfig, and the value SetTxParams gets.
// The tables come from lab measurement (radiolib-org/power-tests, the
// optimised configurations the reference driver adopted) rather than
// the datasheet's three examples; index is target dBm + 9.
type paEntry struct {
	duty, hpMax byte
	val         int8
}

var paTableSX1262 = [32]paEntry{
	{2, 2, -5}, {2, 1, 0}, {1, 1, 3}, {1, 2, 0}, {1, 1, 6}, {1, 2, 3},
	{2, 2, 2}, {4, 1, 6}, {1, 1, 11}, {2, 1, 11}, {1, 1, 14}, {2, 1, 14},
	{1, 1, 20}, {1, 1, 22}, {2, 2, 11}, {3, 1, 21}, {1, 2, 17}, {4, 2, 13},
	{1, 2, 20}, {1, 2, 22}, {2, 2, 21}, {3, 2, 21}, {1, 4, 19}, {1, 4, 20},
	{3, 3, 20}, {2, 5, 19}, {1, 6, 22}, {2, 5, 22}, {3, 5, 22}, {3, 6, 22},
	{4, 6, 22}, {4, 7, 22},
}

var paTableSX1268 = [32]paEntry{
	{2, 1, -3}, {2, 1, -2}, {1, 1, 0}, {4, 1, -1}, {2, 1, 2}, {2, 2, 0},
	{2, 2, 1}, {1, 2, 3}, {1, 3, 3}, {1, 2, 5}, {1, 1, 9}, {4, 1, 8},
	{2, 2, 7}, {1, 1, 13}, {4, 1, 11}, {1, 1, 19}, {2, 1, 19}, {4, 1, 17},
	{1, 6, 12}, {1, 2, 16}, {4, 1, 22}, {2, 2, 18}, {1, 2, 21}, {2, 2, 21},
	{3, 2, 21}, {2, 3, 20}, {1, 6, 20}, {1, 5, 22}, {2, 5, 22}, {3, 5, 22},
	{3, 7, 22}, {4, 7, 22},
}

// paConfigFor maps a chip variant and target power to the PA operating
// point: SetPaConfig's duty/hpMax/deviceSel and SetTxParams' value.
func paConfigFor(chip ChipVariant, dBm int8) (duty, hpMax, deviceSel byte, val int8, err error) {
	minP, maxP := chip.PowerRange()
	if dBm < minP || dBm > maxP {
		return 0, 0, 0, 0, fmt.Errorf("%w: %d dBm is outside the %s range (%d..%d)",
			ErrBadConfig, dBm, chip, minP, maxP)
	}
	switch chip {
	case SX1261:
		// Low-power PA: fixed duty cycle, except the datasheet's 15 dBm
		// special case (raise the duty cycle, program 14).
		if dBm == 15 {
			return 0x06, 0x00, 0x01, 14, nil
		}
		return 0x04, 0x00, 0x01, dBm, nil
	case SX1262:
		e := paTableSX1262[int(dBm)+9]
		return e.duty, e.hpMax, 0x00, e.val, nil
	case SX1268:
		e := paTableSX1268[int(dBm)+9]
		return e.duty, e.hpMax, 0x00, e.val, nil
	default:
		return 0, 0, 0, 0, fmt.Errorf(
			"%w: transmit requires Config.Chip — the version register cannot identify the part", ErrBadConfig)
	}
}

// txPowerCap resolves the configured ceiling: (cap, enabled).
func (r *Radio) txPowerCap() (int8, bool) {
	switch r.cfg.MaxTxPower {
	case 0:
		return 0, false
	case MaxTxPowerZero:
		return 0, true
	default:
		return r.cfg.MaxTxPower, true
	}
}

// Transmit sends one frame at the given chip-side power and returns
// once the air is clear again. It blocks for the frame's airtime — a
// bounded wait, unlike Receive — and hands the radio back the way it
// found it: reception re-armed if it was armed, standby otherwise, the
// reception frame length restored, on every path out.
//
// It refuses rather than harms: power above Config.MaxTxPower is
// ErrPowerExceedsCap — never silently clamped, the integrator's ceiling
// is a promise, not a hint — and a frame arriving or unread is
// ErrReceiveInProgress/ErrUnreadFrame, because transmitting over the
// reception in progress is the exact collision listen-before-talk
// exists to avoid (run AssessChannel first for the neighbours'
// transmissions this radio is not receiving).
//
// Once TxDone is observed the result exists, and it is returned even
// when a later step fails: see TxResult.
func (r *Radio) Transmit(ctx context.Context, payload []byte, powerDBm int8) (res *TxResult, err error) {
	duty, hpMax, deviceSel, paVal, err := r.checkTransmit(payload, powerDBm)
	if err != nil {
		return nil, err
	}
	if err := r.guardDestructive(); err != nil {
		return nil, err
	}
	mode, _, err := r.dev.status()
	if err != nil {
		return nil, err
	}
	wasRX := mode == ModeRx

	// Whatever happens below — success, chip timeout, cancelled wait —
	// the radio is handed back consistent on the way out.
	defer func() {
		if herr := r.handBack(wasRX); herr != nil && err == nil {
			err = herr
		}
	}()

	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return nil, err
	}
	if err := r.applyTXPower(duty, hpMax, deviceSel, paVal); err != nil {
		return nil, err
	}
	// The chip demodulates TX framing from the same packet params as
	// RX: the length must be this frame's, and the deferred restore
	// puts the receive length back.
	if err := r.setPacketParams(r.params, byte(len(payload))); err != nil {
		return nil, err
	}
	if _, err := r.dev.cmd(opWriteBuffer, append([]byte{0x00}, payload...)...); err != nil {
		return nil, err
	}

	const txFlags = IRQTxDone | IRQTimeout
	if err := r.dev.clearIRQ(txFlags); err != nil {
		return nil, err
	}
	if _, err := r.dev.cmd(opSetDioIrqParams,
		byte(txFlags>>8), byte(txFlags&0xFF), byte(txFlags>>8), byte(txFlags&0xFF),
		0x00, 0x00, 0x00, 0x00); err != nil {
		return nil, err
	}
	if err := r.setRF(lora.RFTransmit); err != nil {
		return nil, err
	}

	// The chip-side timeout is a safeguard scaled from airtime — a
	// fixed constant either truncates slow presets mid-frame (the
	// datasheet stops TX when the timer fires) or under-protects fast
	// ones. Clamped to the 24-bit field, which at 15.625 us per tick
	// caps out far beyond any real frame.
	air := r.params.Airtime(len(payload))
	ticks := min(uint64((air+air/4+500*time.Millisecond)/(15625*time.Nanosecond)), 0xFFFFFF)

	start := r.now()
	if _, err := r.dev.cmd(opSetTx,
		byte(ticks>>16), byte(ticks>>8), byte(ticks)); err != nil {
		return nil, err
	}
	res, err = r.awaitTxDone(ctx, start, air)
	if res != nil {
		res.PowerDBm = powerDBm
	}
	return res, err
}

// handBack restores the radio's posture after a transmission attempt:
// aborted to standby, RF switch off transmit, the reception frame
// length restored, and reception re-armed if that is where the radio
// was taken from.
func (r *Radio) handBack(wasRX bool) error {
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}
	if err := r.setRF(lora.RFReceive); err != nil {
		return err
	}
	if err := r.setPacketParams(r.params, rxPayloadLen(r.params)); err != nil {
		return err
	}
	if wasRX {
		return r.StartReceive()
	}
	return nil
}

// checkTransmit validates everything about a transmission that can be
// judged without touching the chip, and resolves the PA operating
// point.
func (r *Radio) checkTransmit(payload []byte, powerDBm int8) (duty, hpMax, deviceSel byte, paVal int8, err error) {
	if !r.ready {
		return 0, 0, 0, 0, ErrNotConfigured
	}
	if len(payload) == 0 || len(payload) > 255 {
		return 0, 0, 0, 0, fmt.Errorf("%w: payload of %d bytes", ErrBadConfig, len(payload))
	}
	if r.params.ImplicitHeader && len(payload) != int(r.params.PayloadLength) {
		return 0, 0, 0, 0, fmt.Errorf("%w: implicit-header frames are fixed at %d bytes, got %d",
			ErrBadConfig, r.params.PayloadLength, len(payload))
	}
	capDBm, enabled := r.txPowerCap()
	if !enabled {
		return 0, 0, 0, 0, fmt.Errorf(
			"%w: transmit requires Config.MaxTxPower — the ceiling is the integrator's to set", ErrBadConfig)
	}
	if powerDBm > capDBm {
		return 0, 0, 0, 0, fmt.Errorf("%w: %d dBm requested, ceiling is %d dBm",
			ErrPowerExceedsCap, powerDBm, capDBm)
	}
	return paConfigFor(r.cfg.Chip, powerDBm)
}

// awaitTxDone waits out the transmission and interprets its outcome.
func (r *Radio) awaitTxDone(ctx context.Context, start time.Time, air time.Duration) (*TxResult, error) {
	// Bound the wait independently of the caller's context: TxDone or
	// the chip's own timeout must arrive within the airtime, and a
	// radio that reports neither needs Reset, not patience.
	waitCtx, cancel := context.WithTimeout(ctx, air*2+time.Second)
	defer cancel()
	const txFlags = IRQTxDone | IRQTimeout
	flags, err := r.waitIRQ(waitCtx, txFlags, 5*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("sx126x: transmit: %w", err)
	}
	at := r.now()
	// TxDone is the proof, and it is built the moment it is observed.
	// Everything after this line is the chip's bookkeeping — clearing
	// the interrupt, reading the error word — and none of it can
	// un-transmit a frame that is already on the air and finished. An
	// integrator counting airtime against a regulatory budget must be
	// told about that emission even when the radio then falls over,
	// so the result travels out beside the error rather than instead
	// of it.
	var res *TxResult
	if flags&IRQTxDone != 0 {
		res = &TxResult{At: at, Airtime: air, Duration: at.Sub(start)}
	}
	if err := r.dev.clearIRQ(flags & txFlags); err != nil {
		return res, err
	}
	if flags&IRQTimeout != 0 {
		// The transmission was stopped by the chip. The error word says
		// whether the PA even ramped.
		if derr := r.dev.checkDeviceErrors("transmit"); derr != nil {
			return res, fmt.Errorf("%w: %w", ErrTimeout, errors.Unwrap(derr))
		}
		return res, fmt.Errorf("sx126x: transmit: %w", ErrTimeout)
	}
	return res, nil
}

// applyTXPower programs the amplifier for one operating point. Order
// matters: the errata §15.2 clamping fix comes first, and only for
// the high-power variants (antenna-mismatch resistance, SX1262/68),
// then the PA operating point, then the ramp.
//
// The over-current ceiling is SetPaConfig's to set. It re-arms the
// part's own default on every call — 140 mA on the SX1262 and
// SX1268, 60 mA on the SX1261 (datasheet table 5-2) — which is the
// ceiling each part chose for its own amplifier, so nothing here
// writes over it.
func (r *Radio) applyTXPower(duty, hpMax, deviceSel byte, paVal int8) error {
	if r.cfg.Chip == SX1262 || r.cfg.Chip == SX1268 {
		clamp, err := r.dev.readRegister(regTxClampConfig, 1)
		if err != nil {
			return err
		}
		if fixed := clamp[0] | 0x1E; fixed != clamp[0] {
			if err := r.dev.writeRegister(regTxClampConfig, fixed); err != nil {
				return err
			}
		}
	}
	if _, err := r.dev.cmd(opSetPaConfig, duty, hpMax, deviceSel, 0x01); err != nil {
		return err
	}
	// 200 us ramp: the reference value, and a spectral-mask parameter —
	// not a place for creativity.
	_, err := r.dev.cmd(opSetTxParams, byte(paVal), 0x04)
	return err
}
