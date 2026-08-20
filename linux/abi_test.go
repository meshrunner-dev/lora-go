//go:build linux

package linux

import (
	"testing"
	"unsafe"
)

// The spiTransfer layout is kernel ABI: its size feeds the ioctl request
// number, so a drift would surface as ENOTTY with no clue. Pin it.
func TestSpiTransferABI(t *testing.T) {
	if got := unsafe.Sizeof(spiTransfer{}); got != 32 {
		t.Fatalf("sizeof(spiTransfer) = %d, want 32 (kernel ABI)", got)
	}
	if iocMessage1 != 0x40206B00 {
		t.Fatalf("iocMessage1 = %#x, want 0x40206B00 (SPI_IOC_MESSAGE(1))", iocMessage1)
	}
}
