// Package sx126x drives Semtech SX1261/1262/1268 LoRa transceivers over
// SPI on a host that owns the reset, BUSY and DIO1 lines.
//
// A Radio is NOT safe for concurrent use, by design rather than by
// omission: the chip has one bus, one interrupt line and one set of
// latched flags, so exactly one goroutine must own it. Serialising
// access anywhere else — a lock inside a method, a callback on another
// thread — reintroduces the races this shape exists to prevent.
//
// The chip's own flags are the source of truth throughout. The driver
// keeps no shadow copy of "a reception is in progress": it asks.
package sx126x

import (
	"context"
	"errors"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// Operational errors.
var (
	ErrNotConfigured = errors.New("sx126x: no channel configured")
	ErrTimeout       = errors.New("sx126x: timed out")
)

// TCXOVoltage is the supply the chip provides to a temperature-
// compensated oscillator through DIO3. Modules with a plain crystal use
// TCXONone.
type TCXOVoltage uint8

// TCXO supply voltages (datasheet table 13-35).
const (
	TCXONone TCXOVoltage = 0xFF // no TCXO: the module has a bare crystal
	TCXO1V6  TCXOVoltage = 0x00
	TCXO1V7  TCXOVoltage = 0x01
	TCXO1V8  TCXOVoltage = 0x02
	TCXO2V2  TCXOVoltage = 0x03
	TCXO2V4  TCXOVoltage = 0x04
	TCXO2V7  TCXOVoltage = 0x05
	TCXO3V0  TCXOVoltage = 0x06
	TCXO3V3  TCXOVoltage = 0x07
)

// Config describes the board, as opposed to the channel: the things
// that depend on how the chip is wired rather than on who we talk to.
type Config struct {
	// TCXO, when not TCXONone, is powered from DIO3. Getting this wrong
	// is silent and total: the crystal never starts, every RF operation
	// fails, and only GetDeviceErrors says why.
	TCXO        TCXOVoltage
	TCXOTimeout time.Duration // crystal settling allowance; 10 ms is ample

	// DIO2AsRFSwitch lets the chip drive the antenna switch itself. On
	// boards that route the switch to a host pin instead, leave it false
	// and provide Pins.AntennaSwitch.
	DIO2AsRFSwitch bool

	// UseDCDC selects the DC-DC regulator over the LDO: less current at
	// the cost of needing the inductor the module may or may not have.
	UseDCDC bool
}

// Radio is a configured transceiver.
type Radio struct {
	dev    device
	cfg    Config
	params lora.Params
	ready  bool
}

// Open resets the chip, applies the board configuration and leaves it in
// standby. It verifies the crystal actually started, so a TCXO mistake
// surfaces here rather than as an unexplained silence later.
func Open(spi lora.SPI, pins lora.Pins, cfg Config) (*Radio, error) {
	if pins.Reset == nil || pins.Busy == nil || pins.DIO1 == nil {
		return nil, errors.New("sx126x: Reset, Busy and DIO1 pins are required")
	}
	if cfg.TCXOTimeout <= 0 {
		cfg.TCXOTimeout = 10 * time.Millisecond
	}
	r := &Radio{dev: device{spi: spi, pins: pins}, cfg: cfg}

	if err := r.dev.reset(); err != nil {
		return nil, err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return nil, err
	}

	if cfg.TCXO != TCXONone {
		// Timeout counts in 15.625 us ticks.
		ticks := uint32(cfg.TCXOTimeout / (15625 * time.Nanosecond))
		if _, err := r.dev.cmd(opSetDio3AsTcxoCtrl, byte(cfg.TCXO),
			byte(ticks>>16), byte(ticks>>8), byte(ticks)); err != nil {
			return nil, err
		}
		// A reset leaves the crystal error latched from before the TCXO
		// was configured; clear it so the check below sees fresh state.
		if err := r.dev.clearDeviceErrors(); err != nil {
			return nil, err
		}
		if _, err := r.dev.cmd(opCalibrate, 0x7F); err != nil { // all blocks
			return nil, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cfg.DIO2AsRFSwitch {
		if _, err := r.dev.cmd(opSetDio2AsRfSwitch, 0x01); err != nil {
			return nil, err
		}
	}
	reg := byte(0x00) // LDO
	if cfg.UseDCDC {
		reg = 0x01
	}
	if _, err := r.dev.cmd(opSetRegulatorMode, reg); err != nil {
		return nil, err
	}
	if _, err := r.dev.cmd(opSetPacketType, packetTypeLoRa); err != nil {
		return nil, err
	}

	// Prove the crystal runs: force it on and see whether the chip
	// complains. Without this, a bad TCXO setting only shows up as every
	// later operation quietly doing nothing.
	if err := r.dev.clearDeviceErrors(); err != nil {
		return nil, err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyXOSC); err != nil {
		return nil, err
	}
	time.Sleep(5 * time.Millisecond)
	if errs, err := r.dev.deviceErrors(); err != nil {
		return nil, err
	} else if errs&errXOSCStart != 0 {
		return nil, fmt.Errorf("%w: %s (check the TCXO voltage)", ErrDeviceError,
			describeDeviceErrors(errs))
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return nil, err
	}
	return r, nil
}

// Configure applies a channel: frequency, modulation and framing. It may
// be called again to retune.
func (r *Radio) Configure(p lora.Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetStandby, standbyRC); err != nil {
		return err
	}

	// Image calibration is per band and must precede the first use of a
	// frequency; skipping it costs sensitivity that is hard to trace.
	lo, hi := imageCalibrationBand(p.Frequency)
	if _, err := r.dev.cmd(opCalibrateImage, lo, hi); err != nil {
		return err
	}

	frf := uint32(uint64(p.Frequency) * (1 << 25) / xtalHz)
	if _, err := r.dev.cmd(opSetRfFrequency,
		byte(frf>>24), byte(frf>>16), byte(frf>>8), byte(frf)); err != nil {
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
	if err := r.setPacketParams(p, 0xFF); err != nil {
		return err
	}

	sync := p.SyncWord
	if sync == 0 {
		sync = 0x12 // the conventional private-network value
	}
	if err := r.dev.writeRegister(regLoRaSyncWordMSB,
		byte(sync&0xF0)|0x04, byte(sync<<4)|0x04); err != nil {
		return err
	}
	if _, err := r.dev.cmd(opSetBufferBaseAddress, 0x00, 0x00); err != nil {
		return err
	}

	r.params = p
	r.ready = true
	return nil
}

// setPacketParams programs the framing. payloadLen matters only with an
// implicit header, but the chip wants it either way.
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
	preamble := p.Preamble
	if preamble == 0 {
		preamble = 8
	}
	_, err := r.dev.cmd(opSetPacketParams,
		byte(preamble>>8), byte(preamble), header, payloadLen, crc, iq)
	return err
}

// Params returns the configured channel.
func (r *Radio) Params() lora.Params { return r.params }

// Status reports the chip's current mode and the outcome of the last
// command — a direct read, never a cached value.
func (r *Radio) Status() (ChipMode, CommandStatus, error) { return r.dev.status() }

// DeviceErrors returns the latched error causes in words, and clears
// them so the next read reflects fresh state.
func (r *Radio) DeviceErrors() (string, error) {
	errs, err := r.dev.deviceErrors()
	if err != nil {
		return "", err
	}
	if errs != 0 {
		if err := r.dev.clearDeviceErrors(); err != nil {
			return "", err
		}
	}
	return describeDeviceErrors(errs), nil
}

// Sleep puts the chip in its lowest-power state. Configuration is kept
// in warm-start mode but the caller should reconfigure after waking.
func (r *Radio) Sleep() error {
	_, err := r.dev.cmd(opSetSleep, 0x04) // warm start, no RTC wake
	return err
}

// Standby returns the chip to standby, aborting whatever it was doing.
//
// This is destructive: a reception in progress dies here. Callers that
// care should check Busy or the interrupt flags first.
func (r *Radio) Standby() error {
	_, err := r.dev.cmd(opSetStandby, standbyRC)
	return err
}

// Close puts the chip to sleep and releases the bus and pins.
func (r *Radio) Close() error {
	_ = r.Sleep()
	err := r.dev.spi.Close()
	if perr := r.dev.pins.Close(); err == nil {
		err = perr
	}
	return err
}

// waitIRQ blocks until one of want's flags is latched, using the DIO1
// edge as a hint and the chip's flags as the answer. The poll floor
// means a missed edge costs latency, never the event.
func (r *Radio) waitIRQ(ctx context.Context, want uint16, poll time.Duration) (uint16, error) {
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

const xtalHz = 32_000_000

// imageCalibrationBand returns the calibration window covering freq
// (datasheet table 9-2).
func imageCalibrationBand(freq uint32) (lo, hi byte) {
	switch {
	case freq >= 902_000_000:
		return 0xE1, 0xE9 // 902-928 MHz
	case freq >= 863_000_000:
		return 0xD7, 0xDB // 863-870 MHz
	case freq >= 779_000_000:
		return 0xC1, 0xC5 // 779-787 MHz
	case freq >= 470_000_000:
		return 0x75, 0x81 // 470-510 MHz
	default:
		return 0x6B, 0x6F // 430-440 MHz
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
