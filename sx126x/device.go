package sx126x

import (
	"errors"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// Errors a device operation can report.
var (
	ErrBusyTimeout = errors.New("sx126x: BUSY stayed high")
	ErrDeviceError = errors.New("sx126x: device error latched")
)

// busyPollInterval paces the BUSY wait. The chip clears BUSY within tens
// of microseconds for most commands and a few milliseconds for
// calibration, so polling this often costs little and reacts quickly.
const busyPollInterval = 50 * time.Microsecond

// device is the command layer: it turns opcodes into SPI transfers and
// enforces the BUSY handshake the chip requires before every command.
//
// It is deliberately not safe for concurrent use. A single owner
// serialises access — the whole point of the design — so no lock hides
// here to give a false sense of safety.
type device struct {
	spi  lora.SPI
	pins lora.Pins
}

// waitBusy blocks until the chip is ready to accept a command.
//
// This is a hardware handshake, not a delay: skipping it drops commands
// silently, and the chip gives no feedback that it did.
func (d *device) waitBusy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		high, err := d.pins.Busy.Get()
		if err != nil {
			return fmt.Errorf("sx126x: read BUSY: %w", err)
		}
		if !high {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s", ErrBusyTimeout, timeout)
		}
		time.Sleep(busyPollInterval)
	}
}

// wake brings the chip out of sleep.
//
// Sleep inverts the usual handshake: BUSY stays high until an SPI
// transaction pulls NSS low, so waiting for BUSY first — as every other
// command does — deadlocks. The transfer must come before the wait.
func (d *device) wake() error {
	if _, err := d.spi.Transfer([]byte{opGetStatus, 0x00}); err != nil {
		return fmt.Errorf("sx126x: wake: %w", err)
	}
	return d.waitBusy(time.Second)
}

// cmd issues a command and returns the full-duplex response. The first
// returned byte is what the chip drove while receiving the opcode, which
// for most commands is the device status.
func (d *device) cmd(op byte, args ...byte) ([]byte, error) {
	if err := d.waitBusy(time.Second); err != nil {
		return nil, err
	}
	tx := make([]byte, 0, 1+len(args))
	tx = append(tx, op)
	tx = append(tx, args...)
	rx, err := d.spi.Transfer(tx)
	if err != nil {
		return nil, fmt.Errorf("sx126x: command 0x%02X: %w", op, err)
	}
	return rx, nil
}

// readRegister reads n bytes starting at addr.
func (d *device) readRegister(addr uint16, n int) ([]byte, error) {
	// opcode, addr MSB, addr LSB, one NOP, then the data.
	tx := make([]byte, 4+n)
	tx[0], tx[1], tx[2] = opReadRegister, byte(addr>>8), byte(addr)
	rx, err := d.cmd(tx[0], tx[1:]...)
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

// irqStatus reads the latched interrupt flags. These flags — not any
// software copy of them — are the source of truth for what happened.
func (d *device) irqStatus() (uint16, error) {
	rx, err := d.cmd(opGetIrqStatus, 0x00, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return uint16(rx[2])<<8 | uint16(rx[3]), nil
}

// clearIRQ clears exactly the flags passed in, never a blanket sweep:
// clearing more than was read discards events that arrived in between,
// and the interrupt line falls with no edge left to announce them.
func (d *device) clearIRQ(flags uint16) error {
	_, err := d.cmd(opClearIrqStatus, byte(flags>>8), byte(flags))
	return err
}

// deviceErrors reads the latched error flags. They persist until
// cleared, so a stale error will misdirect a diagnosis if not reset.
func (d *device) deviceErrors() (uint16, error) {
	rx, err := d.cmd(opGetDeviceErrors, 0x00, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return uint16(rx[2])<<8 | uint16(rx[3]), nil
}

func (d *device) clearDeviceErrors() error {
	_, err := d.cmd(opClearDeviceErrors, 0x00, 0x00)
	return err
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
	return d.waitBusy(time.Second)
}

// Reception failure causes, distinguished because they mean different
// things: a CRC error is a frame that arrived corrupt, a header error is
// one that never made sense at all.
var (
	errCRC    = errors.New("frame failed CRC")
	errHeader = errors.New("malformed frame header")
)
