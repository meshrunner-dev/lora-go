package lora

import "io"

// SPI is the bus a transceiver hangs off. Transfer is full duplex: while
// tx goes out on MOSI, whatever the device drives on MISO fills rx. Both
// slices must have the same length; the caller owns both, so a driver
// polling in a loop can reuse its buffers and allocate nothing.
//
// Implementations need not be safe for concurrent use: a driver owns its
// bus and serialises access.
type SPI interface {
	Transfer(tx, rx []byte) error
	Close() error
}

// OutputPin drives a line the host controls, such as a reset line.
type OutputPin interface {
	Set(high bool) error
	Close() error
}

// InputPin reads a line the device drives, such as BUSY.
type InputPin interface {
	Get() (high bool, err error)
	Close() error
}

// InterruptPin is an input that also reports rising edges.
//
// Edges must be delivered without touching the bus: whatever watches the
// line does nothing but signal, and the owner reads the device's own
// interrupt flags to learn what happened. The channel is buffered and
// lossy on purpose — an edge is a hint to go look, never the payload,
// so a missed notification degrades to a poll rather than a lost event.
type InterruptPin interface {
	InputPin
	Edges() <-chan struct{}
}

// RFMode is the position of an RF switch.
type RFMode int

// RF switch positions. Off disconnects both paths where the hardware
// can express it; single-pin switches map it to the receive position.
const (
	RFOff RFMode = iota
	RFReceive
	RFTransmit
)

// RFSwitch steers an antenna switch the host controls. Boards that let
// the chip drive the switch itself (DIO2) need none.
type RFSwitch interface {
	Set(RFMode) error
	Close() error
}

// SinglePinRF adapts a one-line switch: one position for transmit, the
// other for everything else. txHigh says which level selects transmit.
func SinglePinRF(pin OutputPin, txHigh bool) RFSwitch {
	return &singlePinRF{pin: pin, txHigh: txHigh}
}

type singlePinRF struct {
	pin    OutputPin
	txHigh bool
}

func (s *singlePinRF) Set(m RFMode) error { return s.pin.Set((m == RFTransmit) == s.txHigh) }
func (s *singlePinRF) Close() error       { return s.pin.Close() }

// TwoPinRF adapts the common RXEN/TXEN pair, as found in front-end
// modules: exactly one line high per active path, both low when off.
// Driving both high is a hardware fault this type makes inexpressible.
func TwoPinRF(rxen, txen OutputPin) RFSwitch {
	return &twoPinRF{rxen: rxen, txen: txen}
}

type twoPinRF struct{ rxen, txen OutputPin }

func (s *twoPinRF) Set(m RFMode) error {
	// Break before make: never let both paths conduct, even for the
	// microseconds between two Set calls.
	if err := s.rxen.Set(false); err != nil {
		return err
	}
	if err := s.txen.Set(false); err != nil {
		return err
	}
	switch m {
	case RFReceive:
		return s.rxen.Set(true)
	case RFTransmit:
		return s.txen.Set(true)
	default:
		return nil
	}
}

func (s *twoPinRF) Close() error {
	err := s.rxen.Close()
	if cerr := s.txen.Close(); err == nil {
		err = cerr
	}
	return err
}

// Pins groups the lines an SX126x-class transceiver needs beyond SPI.
// RF is nil on boards where the chip drives the switch from DIO2, or
// where there is no switch at all.
type Pins struct {
	Reset OutputPin
	Busy  InputPin
	DIO1  InterruptPin
	RF    RFSwitch
}

// Close releases every pin that is present, returning the first error.
// "Present" means the interface value is non-nil; an interface holding
// a typed nil pointer counts as present and will make that pin's own
// Close panic — constructors in this module never produce that shape.
func (p Pins) Close() error {
	var first error
	for _, c := range []io.Closer{p.Reset, p.Busy, p.DIO1, p.RF} {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
