package sx126x

import "errors"

// Errors the driver reports. The reception outcomes ErrCRC and
// ErrHeader are expected traffic on a busy band, not faults: the
// received-to-corrupt ratio is a standard site-health metric, which is
// why both are exported and distinguishable with errors.Is.
var (
	// ErrNoDevice means the bus answered with all-zeros or all-ones:
	// electrically, nobody home. Check wiring and the spidev path.
	ErrNoDevice = errors.New("sx126x: no device on the bus")

	// ErrCommandFailed reports a command the chip explicitly rejected
	// (timeout, processing error or execution failure in its status
	// byte) — usually a command issued in a mode that forbids it.
	ErrCommandFailed = errors.New("sx126x: command failed")

	// ErrBusyTimeout means BUSY never fell: the chip has stopped
	// responding (stuck calibration, dead TCXO, or it is asleep — see
	// Wake). The recovery is Reset.
	ErrBusyTimeout = errors.New("sx126x: BUSY stayed high")

	// ErrDeviceError carries a non-zero DeviceError word after an
	// operation that must complete cleanly.
	ErrDeviceError = errors.New("sx126x: device error latched")

	// ErrNotConfigured: Configure has not succeeded yet.
	ErrNotConfigured = errors.New("sx126x: no channel configured")

	// ErrNotReceiving: the operation only makes sense with the radio
	// armed in receive mode (see StartReceive).
	ErrNotReceiving = errors.New("sx126x: radio is not in receive mode")

	// ErrReceiveInProgress: a destructive operation was refused because
	// a frame is arriving. Retry once the air is quiet.
	ErrReceiveInProgress = errors.New("sx126x: reception in progress")

	// ErrUnreadFrame: a destructive operation was refused because a
	// finished reception has not been collected. Poll first.
	ErrUnreadFrame = errors.New("sx126x: unread frame pending")

	// ErrCRC: the frame arrived but failed its checksum. Expected
	// traffic, not a fault.
	ErrCRC = errors.New("sx126x: frame failed CRC")

	// ErrHeader: a frame header that never made sense. Expected
	// traffic, not a fault.
	ErrHeader = errors.New("sx126x: malformed frame header")

	// ErrTimeout: the chip reported an RX/TX timeout.
	ErrTimeout = errors.New("sx126x: timed out")
)
