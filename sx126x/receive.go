package sx126x

import (
	"context"
	"fmt"
	"time"

	"meshrunner.dev/pkg/lora"
)

// RxFrame is a received frame and what the radio measured about it.
type RxFrame struct {
	Payload []byte
	RSSI    float64   // dBm
	SNR     float64   // dB
	At      time.Time // when the driver observed RxDone
}

// AssessChannel performs a channel activity detection: a short listen
// that answers "is a LoRa transmission under way right now?".
//
// It is far cheaper than a full reception — tens of milliseconds — which
// is what makes listen-before-talk affordable. It reports true when the
// channel is busy.
func (r *Radio) AssessChannel(ctx context.Context, symbols CADSymbols) (bool, error) {
	if !r.ready {
		return false, ErrNotConfigured
	}
	peak, min := cadDetection(r.params.SF)
	if _, err := r.dev.cmd(opSetCadParams,
		byte(symbols), peak, min, 0x00 /* exit to standby */, 0x00, 0x00, 0x00); err != nil {
		return false, err
	}
	if err := r.dev.clearIRQ(irqAll); err != nil {
		return false, err
	}
	if _, err := r.dev.cmd(opSetDioIrqParams,
		0x01, 0x80, 0x01, 0x80, 0x00, 0x00, 0x00, 0x00); err != nil { // CadDone|CadDetected on DIO1
		return false, err
	}
	if _, err := r.dev.cmd(opSetCad); err != nil {
		return false, err
	}

	// A CAD is bounded by construction; the ceiling only guards against
	// a chip that never answers.
	deadline := r.params.SymbolDuration()*time.Duration(symbolCount(symbols)+8) + 100*time.Millisecond
	cadCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	flags, err := r.waitIRQ(cadCtx, irqCadDone, 2*time.Millisecond)
	if err != nil {
		return false, fmt.Errorf("sx126x: CAD: %w", err)
	}
	if err := r.dev.clearIRQ(flags); err != nil {
		return false, err
	}
	return flags&irqCadDetected != 0, nil
}

// StartReceive puts the radio into continuous reception and leaves it
// there. Staying in RX is the default posture: a radio that is not
// transmitting should be listening, including while it waits for a free
// channel.
func (r *Radio) StartReceive() error {
	if !r.ready {
		return ErrNotConfigured
	}
	if err := r.dev.clearIRQ(irqAll); err != nil {
		return err
	}
	// Ask for the progress markers too. Whoever arms reception owns the
	// whole mask: a later writer that omits them makes any
	// reception-in-progress check silently blind.
	mask := uint16(irqRxDone | irqTimeout | irqCRCErr | irqHeaderErr |
		irqPreambleDetect | irqHeaderValid)
	if _, err := r.dev.cmd(opSetDioIrqParams,
		byte(mask>>8), byte(mask), byte(mask>>8), byte(mask), 0, 0, 0, 0); err != nil {
		return err
	}
	if err := r.setAntenna(false); err != nil {
		return err
	}
	// 0xFFFFFF selects continuous reception rather than a timeout.
	_, err := r.dev.cmd(opSetRx, 0xFF, 0xFF, 0xFF)
	return err
}

// Receive waits for the next frame. The radio must already be in
// reception (see StartReceive); it stays there afterwards, so frames can
// be read back to back.
//
// A frame failing its CRC is reported as an error rather than returned:
// the bytes are known-corrupt, and the flags are cleared either way so
// reception continues.
func (r *Radio) Receive(ctx context.Context) (*RxFrame, error) {
	if !r.ready {
		return nil, ErrNotConfigured
	}
	flags, err := r.waitIRQ(ctx, irqRxDone|irqTimeout|irqCRCErr|irqHeaderErr, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	at := time.Now()
	if err := r.dev.clearIRQ(flags); err != nil {
		return nil, err
	}
	switch {
	case flags&irqCRCErr != 0:
		return nil, fmt.Errorf("sx126x: %w", errCRC)
	case flags&irqHeaderErr != 0:
		return nil, fmt.Errorf("sx126x: %w", errHeader)
	case flags&irqTimeout != 0:
		return nil, ErrTimeout
	}

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
	return &RxFrame{Payload: payload, RSSI: rssi, SNR: snr, At: at}, nil
}

// ReceiveInProgress reports whether the radio is currently demodulating,
// by reading the chip's live flags rather than any software memory of
// them. Preamble seen means something is arriving; header seen means a
// frame is committed and worth protecting.
func (r *Radio) ReceiveInProgress() (preamble, header bool, err error) {
	flags, err := r.dev.irqStatus()
	if err != nil {
		return false, false, err
	}
	return flags&irqPreambleDetect != 0, flags&irqHeaderValid != 0, nil
}

func (r *Radio) readBuffer(offset byte, n int) ([]byte, error) {
	tx := make([]byte, 3+n)
	tx[0], tx[1] = opReadBuffer, offset
	rx, err := r.dev.cmd(tx[0], tx[1:]...)
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

// setAntenna steers an external RF switch, when the board routes it to
// the host instead of letting the chip drive it from DIO2.
func (r *Radio) setAntenna(transmit bool) error {
	if r.dev.pins.AntennaSwitch == nil {
		return nil
	}
	return r.dev.pins.AntennaSwitch.Set(transmit)
}

// CADSymbols is how many symbols a channel assessment listens for. More
// symbols means a more reliable answer and a longer listen.
type CADSymbols byte

// Symbol counts the chip supports.
const (
	CAD1Symbol   CADSymbols = 0x00
	CAD2Symbols  CADSymbols = 0x01
	CAD4Symbols  CADSymbols = 0x02
	CAD8Symbols  CADSymbols = 0x03
	CAD16Symbols CADSymbols = 0x04
)

func symbolCount(s CADSymbols) int {
	switch s {
	case CAD1Symbol:
		return 1
	case CAD2Symbols:
		return 2
	case CAD4Symbols:
		return 4
	case CAD8Symbols:
		return 8
	default:
		return 16
	}
}

// cadDetection returns the peak and minimum detection thresholds
// Semtech recommends for a spreading factor (application note AN1200.48).
func cadDetection(sf lora.SpreadingFactor) (peak, min byte) {
	switch sf {
	case lora.SF5:
		return 18, 10
	case lora.SF6:
		return 19, 10
	case lora.SF7:
		return 20, 10
	case lora.SF8:
		return 21, 10
	case lora.SF9:
		return 22, 10
	case lora.SF10:
		return 23, 10
	case lora.SF11:
		return 24, 10
	default:
		return 25, 10
	}
}

// RSSI reads the instantaneous signal strength on the channel. Taken
// while receiving, it measures the noise floor — the number that tells a
// deaf radio (no antenna, silent front end) from a quiet channel.
func (r *Radio) RSSI() (float64, error) {
	rx, err := r.dev.cmd(opGetRssiInst, 0x00, 0x00)
	if err != nil {
		return 0, err
	}
	return -float64(rx[2]) / 2, nil
}
