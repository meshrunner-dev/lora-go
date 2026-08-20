package sx126x

import (
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// busyPollInterval paces the BUSY wait. The chip clears BUSY within tens
// of microseconds for most commands and a few milliseconds for
// calibration, so polling this often costs little and reacts quickly.
const busyPollInterval = 50 * time.Microsecond

// device is the command layer: it turns opcodes into SPI transfers,
// enforces the BUSY handshake the chip requires before every command,
// and rejects transfers the chip itself flagged as failed.
//
// It is deliberately not safe for concurrent use. A single owner
// serialises access — the whole point of the design — so no lock hides
// here to give a false sense of safety.
type device struct {
	spi  lora.SPI
	pins lora.Pins

	// busyTimeout bounds every BUSY wait. One second in production;
	// tests shrink it so timeout paths run fast.
	busyTimeout time.Duration

	// lastOp is the previous opcode sent: the status byte the chip
	// clocks back during a command describes the command BEFORE it —
	// the chip cannot know the outcome of bytes it is still receiving —
	// so failures must be attributed one step back.
	lastOp byte

	// tx/rx are reused across transfers so a polling loop allocates
	// nothing. Safe only because of the single-owner rule.
	tx, rx []byte
}

func newDevice(spi lora.SPI, pins lora.Pins) device {
	return device{spi: spi, pins: pins, busyTimeout: time.Second}
}

// waitBusy blocks until the chip is ready to accept a command.
//
// This is a hardware handshake, not a delay: skipping it drops commands
// silently, and the chip gives no feedback that it did. It also covers
// slow internal work — calibration, TCXO start — because BUSY stays
// high for their whole duration.
func (d *device) waitBusy() error {
	deadline := time.Now().Add(d.busyTimeout)
	for {
		high, err := d.pins.Busy.Get()
		if err != nil {
			return fmt.Errorf("sx126x: read BUSY: %w", err)
		}
		if !high {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s", ErrBusyTimeout, d.busyTimeout)
		}
		time.Sleep(busyPollInterval)
	}
}

// xfer runs one raw full-duplex transfer of n bytes; tx must already be
// staged in d.tx[:n]. The response lands in d.rx[:n].
func (d *device) xfer(n int) error {
	if cap(d.rx) < n {
		d.rx = make([]byte, n)
	}
	d.rx = d.rx[:n]
	return d.spi.Transfer(d.tx[:n], d.rx)
}

// cmd issues a command and returns the full-duplex response, valid only
// until the next call. Response format: the byte clocked back during
// the opcode is RFU; the chip's status rides on every byte after it,
// and the driver checks the first one — a command the chip rejected
// (wrong mode, timeout, execution failure) fails here instead of
// silently doing nothing, and a bus with nobody on it is recognised
// instead of reading as an endless success.
func (d *device) cmd(op byte, args ...byte) ([]byte, error) {
	if err := d.waitBusy(); err != nil {
		return nil, err
	}
	n := 1 + len(args)
	if cap(d.tx) < n {
		d.tx = make([]byte, n)
	}
	d.tx = d.tx[:n]
	d.tx[0] = op
	copy(d.tx[1:], args)
	if err := d.xfer(n); err != nil {
		return nil, fmt.Errorf("sx126x: command 0x%02X: %w", op, err)
	}
	prev := d.lastOp
	d.lastOp = op
	if n >= 2 {
		if err := parseStatus(d.rx[1]); err != nil {
			return nil, fmt.Errorf("sx126x: command 0x%02X (reported while sending 0x%02X): %w",
				prev, op, err)
		}
	}
	return d.rx, nil
}

// parseStatus screens the status byte every command clocks back. The
// verdict applies to the PREVIOUS command (see device.lastOp).
// All-zeros and all-ones cannot be produced by a working chip driving
// MISO; they are what a floating or grounded line reads as.
func parseStatus(st byte) error {
	if st == 0x00 || st == 0xFF {
		return ErrNoDevice
	}
	switch CommandStatus((st >> 1) & 0x7) {
	case CmdTimeout, CmdProcessingErr, CmdExecFailure:
		return fmt.Errorf("%w: %s", ErrCommandFailed, CommandStatus((st>>1)&0x7))
	default:
		return nil
	}
}

// wake brings the chip out of sleep.
//
// Sleep inverts the usual handshake: BUSY stays high until an SPI
// transaction pulls NSS low, so waiting for BUSY first — as every other
// command does — deadlocks. The transfer must come before the wait.
func (d *device) wake() error {
	var wtx, wrx [2]byte
	wtx[0] = opGetStatus
	if err := d.spi.Transfer(wtx[:], wrx[:]); err != nil {
		return fmt.Errorf("sx126x: wake: %w", err)
	}
	return d.waitBusy()
}

// readRegister reads n bytes starting at addr.
func (d *device) readRegister(addr uint16, n int) ([]byte, error) {
	// opcode, addr MSB, addr LSB, one status byte, then the data.
	args := make([]byte, 3+n)
	args[0], args[1] = byte(addr>>8), byte(addr)
	rx, err := d.cmd(opReadRegister, args...)
	if err != nil {
		return nil, err
	}
	return rx[4:], nil
}

func (d *device) writeRegister(addr uint16, data ...byte) error {
	args := append([]byte{byte(addr >> 8), byte(addr)}, data...)
	_, err := d.cmd(opWriteRegister, args...)
	return err
}

// status reports the chip mode and the outcome of the last command.
func (d *device) status() (mode ChipMode, cmd CommandStatus, err error) {
	rx, err := d.cmd(opGetStatus, 0x00)
	if err != nil {
		return 0, 0, err
	}
	s := rx[1]
	return ChipMode((s >> 4) & 0x7), CommandStatus((s >> 1) & 0x7), nil
}

// irqStatus reads the latched interrupt flags.
func (d *device) irqStatus() (IRQ, error) {
	rx, err := d.cmd(opGetIrqStatus, 0x00, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return IRQ(rx[2])<<8 | IRQ(rx[3]), nil
}

// clearIRQ clears exactly the flags passed in — see Radio.ClearIRQ for
// why never more.
func (d *device) clearIRQ(flags IRQ) error {
	_, err := d.cmd(opClearIrqStatus, byte(flags>>8), byte(flags))
	return err
}

// deviceErrors reads the latched error flags without clearing them.
func (d *device) deviceErrors() (DeviceError, error) {
	rx, err := d.cmd(opGetDeviceErrors, 0x00, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return DeviceError(rx[2])<<8 | DeviceError(rx[3]), nil
}

func (d *device) clearDeviceErrors() error {
	_, err := d.cmd(opClearDeviceErrors, 0x00, 0x00)
	return err
}

// checkDeviceErrors reads the latched error word and fails on anything
// non-zero. Calibration and PLL commands "succeed" on the bus whatever
// happens inside the chip; this word is their only verdict.
func (d *device) checkDeviceErrors(during string) error {
	errs, err := d.deviceErrors()
	if err != nil {
		return err
	}
	if errs != 0 {
		return fmt.Errorf("%w during %s: %s", ErrDeviceError, during, errs)
	}
	return nil
}

// reset drives NRESET low then releases it, and waits for the chip to
// come back. Everything configured before is lost, including the TCXO
// setup, so callers must reconfigure from scratch afterwards.
func (d *device) reset() error {
	if err := d.pins.Reset.Set(false); err != nil {
		return fmt.Errorf("sx126x: assert NRESET: %w", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := d.pins.Reset.Set(true); err != nil {
		return fmt.Errorf("sx126x: release NRESET: %w", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := d.waitBusy(); err != nil {
		return err
	}
	// Flush the in-band status: right after power-on it holds garbage
	// that would otherwise be blamed on the first real command, and the
	// field only refreshes when a command actually executes — GetStatus
	// is not enough — so the flush is a harmless standby, sent raw. Its
	// response also proves someone is on the bus at all.
	var ftx, frx [2]byte
	ftx[0] = opSetStandby
	if err := d.spi.Transfer(ftx[:], frx[:]); err != nil {
		return fmt.Errorf("sx126x: post-reset flush: %w", err)
	}
	if frx[1] == 0x00 || frx[1] == 0xFF {
		return ErrNoDevice
	}
	d.lastOp = opSetStandby
	return nil
}
