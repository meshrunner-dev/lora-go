package lora

import (
	"math"
	"time"
)

// Airtime returns how long a frame of payloadLen bytes occupies the
// channel under these parameters — preamble included.
//
// This is the number every higher layer leans on: duty-cycle budgets,
// listen-before-talk deadlines and retransmission spacing are all
// expressed in airtime, not in bytes.
//
// The symbol count follows Semtech's formula, with the SF5/SF6 variant
// the SX126x family introduced: those two spreading factors use a
// longer preamble offset and skip the low-data-rate term.
func (p Params) Airtime(payloadLen int) time.Duration {
	sym := p.SymbolDuration()

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

	num := float64(8*payloadLen - 4*int(p.SF) + 28 + 16*crc - 20*ih)
	var den float64
	var preambleOffset float64
	if p.SF == SF5 || p.SF == SF6 {
		den = float64(4 * int(p.SF))
		preambleOffset = 6.25
	} else {
		den = float64(4 * (int(p.SF) - 2*de))
		preambleOffset = 4.25
	}

	payloadSymbols := 8.0
	if num > 0 {
		payloadSymbols += math.Ceil(num/den) * float64(p.CR)
	}

	symbols := float64(p.Preamble) + preambleOffset + payloadSymbols
	return time.Duration(symbols * float64(sym))
}
