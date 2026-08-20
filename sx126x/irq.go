package sx126x

import "strings"

// IRQ is a set of the chip's latched interrupt flags (datasheet table
// 8-3). The latched word on the chip — not any software copy of it — is
// the source of truth for what happened.
type IRQ uint16

// Interrupt flags.
const (
	IRQTxDone           IRQ = 1 << 0
	IRQRxDone           IRQ = 1 << 1
	IRQPreambleDetected IRQ = 1 << 2
	IRQSyncWordValid    IRQ = 1 << 3
	IRQHeaderValid      IRQ = 1 << 4
	IRQHeaderErr        IRQ = 1 << 5
	IRQCRCErr           IRQ = 1 << 6
	IRQCadDone          IRQ = 1 << 7
	IRQCadDetected      IRQ = 1 << 8
	IRQTimeout          IRQ = 1 << 9
	IRQAll              IRQ = 0x03FF
)

// irqProgress are the markers of a reception under way, as opposed to
// its outcomes.
const irqProgress = IRQPreambleDetected | IRQSyncWordValid | IRQHeaderValid

// irqOutcome are the terminal flags a reception can end with.
const irqOutcome = IRQRxDone | IRQCRCErr | IRQHeaderErr | IRQTimeout

var irqNames = []struct {
	bit  IRQ
	name string
}{
	{IRQTxDone, "TxDone"},
	{IRQRxDone, "RxDone"},
	{IRQPreambleDetected, "PreambleDetected"},
	{IRQSyncWordValid, "SyncWordValid"},
	{IRQHeaderValid, "HeaderValid"},
	{IRQHeaderErr, "HeaderErr"},
	{IRQCRCErr, "CRCErr"},
	{IRQCadDone, "CadDone"},
	{IRQCadDetected, "CadDetected"},
	{IRQTimeout, "Timeout"},
}

// String implements fmt.Stringer.
func (i IRQ) String() string {
	if i == 0 {
		return "none"
	}
	var parts []string
	for _, n := range irqNames {
		if i&n.bit != 0 {
			parts = append(parts, n.name)
		}
	}
	return strings.Join(parts, "|")
}

// IRQ reads the chip's latched interrupt flags.
func (r *Radio) IRQ() (IRQ, error) { return r.dev.irqStatus() }

// ClearIRQ clears exactly the given flags. Narrow clears are the rule:
// clearing more than was read discards events that arrived in between,
// and the interrupt line falls with no edge left to announce them.
func (r *Radio) ClearIRQ(flags IRQ) error { return r.dev.clearIRQ(flags) }
