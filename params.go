package lora

import (
	"errors"
	"fmt"
	"time"
)

// ErrBadParams reports modulation parameters that do not describe a
// usable channel.
var ErrBadParams = errors.New("lora: invalid modulation parameters")

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
//
// There are no defaults and no silent substitutions: Validate rejects
// an unset preamble or sync word rather than picking one, because both
// are network-level agreements — the preamble length in particular is
// the window during which neighbours can detect a transmission, so a
// node must declare it, not inherit it.
type Params struct {
	Frequency uint32 // carrier, in hertz
	SF        SpreadingFactor
	BW        Bandwidth
	CR        CodingRate

	// Preamble is the number of preamble symbols. Receivers need a few
	// to lock on, and the whole preamble doubles as the carrier-sense
	// window. Required; 8 is the classic minimum.
	Preamble uint16

	// SyncWord distinguishes networks sharing a channel. 0x12 is the
	// conventional private value, 0x34 the public/LoRaWAN one.
	// Required.
	SyncWord uint8

	// ImplicitHeader omits the LoRa header, in which case both ends
	// must agree on PayloadLength and coding rate beforehand.
	ImplicitHeader bool

	// PayloadLength is the agreed frame size, in bytes. Required with
	// an implicit header — it is the only thing telling the demodulator
	// where the frame ends — and ignored otherwise.
	PayloadLength uint8

	// CRC appends the payload checksum the receiver verifies. Off in
	// the zero value; most networks, MeshCore included, run with it on.
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
	if p.Preamble == 0 {
		return fmt.Errorf("%w: no preamble length — it is a network agreement, declare it", ErrBadParams)
	}
	if p.SyncWord == 0 {
		return fmt.Errorf("%w: no sync word (0x12 private, 0x34 public)", ErrBadParams)
	}
	if p.ImplicitHeader && p.PayloadLength == 0 {
		return fmt.Errorf("%w: implicit header requires PayloadLength", ErrBadParams)
	}
	return nil
}

// SymbolDuration is the time one chirp occupies the channel: 2^SF / BW.
// It underpins every other timing in the modulation. Out-of-range
// parameters yield 0; Validate first when the input is not trusted.
func (p Params) SymbolDuration() time.Duration {
	if p.SF < SF5 || p.SF > SF12 || p.BW == 0 {
		return 0
	}
	return time.Duration(float64(int(1)<<p.SF) / float64(p.BW) * float64(time.Second))
}

// LowDataRateOptimize reports whether the low-data-rate optimisation
// must be enabled, which the modulation requires once a symbol lasts
// longer than 16 ms — otherwise clock drift breaks demodulation.
func (p Params) LowDataRateOptimize() bool {
	return p.SymbolDuration() > 16*time.Millisecond
}

// PreambleDuration is how long the preamble occupies the air, including
// the mandatory tail the modem appends (4.25 symbols, 6.25 at SF5/SF6).
// It bounds how long "preamble detected, header pending" can honestly
// last, which makes it the natural expiry for carrier-sense state.
func (p Params) PreambleDuration() time.Duration {
	tail := 4.25
	if p.SF == SF5 || p.SF == SF6 {
		tail = 6.25
	}
	return time.Duration((float64(p.Preamble) + tail) * float64(p.SymbolDuration()))
}

// MaxFrameDuration is the airtime of the longest frame the channel can
// carry with maxPayload bytes — the honest upper bound on "a reception
// is in progress".
func (p Params) MaxFrameDuration(maxPayload int) time.Duration {
	return p.Airtime(maxPayload)
}
