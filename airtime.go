package lora

import (
	"math"
	"time"
)

// Airtime returns how long a frame of payloadLen bytes occupies the
// channel under these parameters — preamble included. Invalid
// parameters or a negative length yield 0; Validate first when the
// input is not trusted.
//
// This is the number every higher layer leans on: duty-cycle budgets,
// listen-before-talk deadlines and retransmission spacing are all
// expressed in airtime, not in bytes.
//
// The symbol count follows Semtech's formula, with the SF5/SF6 variant
// the SX126x family introduced: those two spreading factors use a
// longer preamble tail (6.25 symbols instead of 4.25), skip the
// low-data-rate term, and drop the 8-bit sync contribution from the
// payload term (the +28 becomes +20).
func (p Params) Airtime(payloadLen int) time.Duration {
	sym := p.SymbolDuration()
	if sym == 0 || payloadLen < 0 {
		return 0
	}

	crc, ih, de := 0, 0, 0
	if p.CRC {
		crc = 1
	}
	if p.ImplicitHeader {
		ih = 1
	}
	if p.LowDataRateOptimize() {
		de = 1
	}

	var num, den, preambleTail float64
	if p.SF == SF5 || p.SF == SF6 {
		num = float64(8*payloadLen - 4*int(p.SF) + 20 + 16*crc - 20*ih)
		den = float64(4 * int(p.SF))
		preambleTail = 6.25
	} else {
		num = float64(8*payloadLen - 4*int(p.SF) + 28 + 16*crc - 20*ih)
		den = float64(4 * (int(p.SF) - 2*de))
		preambleTail = 4.25
	}

	payloadSymbols := 8.0
	if num > 0 {
		payloadSymbols += math.Ceil(num/den) * float64(p.CR)
	}

	symbols := float64(p.Preamble) + preambleTail + payloadSymbols
	return time.Duration(symbols * float64(sym))
}
