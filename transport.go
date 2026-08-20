package lora

// SPI is the bus a transceiver hangs off. Transfer is full duplex: the
// returned slice holds what the device drove on MISO while the caller's
// bytes went out on MOSI, so it always has the same length as tx.
//
// Implementations need not be safe for concurrent use: a driver owns its
// bus and serialises access.
type SPI interface {
	Transfer(tx []byte) ([]byte, error)
	Close() error
}

// OutputPin drives a line the host controls, such as a reset or an
// antenna switch.
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

// Pins groups the lines an SX126x-class transceiver needs beyond SPI.
// AntennaSwitch is nil on modules that let the chip drive the RF switch
// from DIO2 instead of exposing it to the host.
type Pins struct {
	Reset         OutputPin
	Busy          InputPin
	DIO1          InterruptPin
	AntennaSwitch OutputPin
}

// Close releases every pin that is present, returning the first error.
func (p Pins) Close() error {
	var first error
	for _, c := range []interface{ Close() error }{p.Reset, p.Busy, p.DIO1, p.AntennaSwitch} {
		if c == nil {
			continue
		}
		// A nil interface holding a nil pointer would panic on Close.
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
