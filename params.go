package lora

import (
	"errors"
	"fmt"
	"time"
)

// Parameter errors.
var (
	ErrBadParams = errors.New("lora: invalid modulation parameters")
	ErrBadPower  = errors.New("lora: transmit power out of range")
)

// SpreadingFactor is the LoRa spreading factor, SF5 to SF12. Higher
// spreads further and slower: each step doubles airtime.
type SpreadingFactor uint8

// Spreading factors defined by the modulation.
const (
	SF5  SpreadingFactor = 5
	SF6  SpreadingFactor = 6
	SF7  SpreadingFactor = 7
	SF8  SpreadingFactor = 8
	SF9  SpreadingFactor = 9
	SF10 SpreadingFactor = 10
	SF11 SpreadingFactor = 11
	SF12 SpreadingFactor = 12
)

// Bandwidth is the channel bandwidth in hertz.
type Bandwidth uint32

// The bandwidths a LoRa transceiver offers.
const (
	BW7810   Bandwidth = 7810
	BW10420  Bandwidth = 10420
	BW15630  Bandwidth = 15630
	BW20830  Bandwidth = 20830
	BW31250  Bandwidth = 31250
	BW41670  Bandwidth = 41670
	BW62500  Bandwidth = 62500
	BW125000 Bandwidth = 125000
	BW250000 Bandwidth = 250000
	BW500000 Bandwidth = 500000
)

// CodingRate is the forward-error-correction rate, expressed as the
// denominator of 4/n: CR5 means 4/5, CR8 means 4/8.
type CodingRate uint8

// Coding rates defined by the modulation.
const (
	CR5 CodingRate = 5
	CR6 CodingRate = 6
	CR7 CodingRate = 7
	CR8 CodingRate = 8
)

// Params is a complete description of a LoRa channel: everything two
// radios must agree on to hear each other.
type Params struct {
	Frequency uint32 // carrier, in hertz
	SF        SpreadingFactor
	BW        Bandwidth
	CR        CodingRate

	// Preamble is the number of preamble symbols; receivers need a few
	// to lock on, and it doubles as the carrier-sense window.
	Preamble uint16

	// SyncWord distinguishes networks sharing a channel. 0x12 is the
	// conventional private value, 0x34 the public/LoRaWAN one.
	SyncWord uint8

	// ImplicitHeader omits the LoRa header, in which case both ends must
	// already agree on the payload length and coding rate.
	ImplicitHeader bool

	// CRC appends the payload checksum the receiver verifies.
	CRC bool

	// InvertIQ swaps I and Q, the trick gateways use so that downlinks
	// are not heard by other gateways.
	InvertIQ bool
}

// Validate reports whether the parameters describe a usable channel.
func (p Params) Validate() error {
	if p.SF < SF5 || p.SF > SF12 {
		return fmt.Errorf("%w: spreading factor %d", ErrBadParams, p.SF)
	}
	if p.CR < CR5 || p.CR > CR8 {
		return fmt.Errorf("%w: coding rate 4/%d", ErrBadParams, p.CR)
	}
	switch p.BW {
	case BW7810, BW10420, BW15630, BW20830, BW31250, BW41670, BW62500,
		BW125000, BW250000, BW500000:
	default:
		return fmt.Errorf("%w: bandwidth %d Hz", ErrBadParams, p.BW)
	}
	if p.Frequency == 0 {
		return fmt.Errorf("%w: no frequency", ErrBadParams)
	}
	return nil
}

// SymbolDuration is the time one chirp occupies the channel: 2^SF / BW.
// It underpins every other timing in the modulation.
func (p Params) SymbolDuration() time.Duration {
	return time.Duration(float64(int(1)<<p.SF) / float64(p.BW) * float64(time.Second))
}

// LowDataRateOptimize reports whether the low-data-rate optimisation
// must be enabled, which the modulation requires once a symbol lasts
// longer than 16 ms — otherwise clock drift breaks demodulation.
func (p Params) LowDataRateOptimize() bool {
	return p.SymbolDuration() > 16*time.Millisecond
}
