package linux

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"meshrunner.dev/pkg/lora"
)

// spidev ioctl requests, from linux/spi/spidev.h. _IOW('k', n, size)
// packs direction, size, type and number into one word.
const (
	iocWrMode        = 0x40016B01 // _IOW('k', 1, __u8)
	iocWrBitsPerWord = 0x40016B03 // _IOW('k', 3, __u8)
	iocWrMaxSpeedHz  = 0x40046B04 // _IOW('k', 4, __u32)
	iocMessage1      = 0x40206B00 // _IOW('k', 0, [1]spiTransfer), 32 bytes
)

// spiTransfer mirrors struct spi_ioc_transfer. The layout is ABI, so the
// field order and padding must not be touched.
type spiTransfer struct {
	txBuf, rxBuf          uint64
	length, speedHz       uint32
	delayUsecs            uint16
	bitsPerWord, csChange uint8
	txNbits, rxNbits      uint8
	wordDelayUsecs, pad   uint8
}

// SPI is a spidev bus, e.g. /dev/spidev0.0.
type SPI struct {
	f     *os.File
	speed uint32
}

// OpenSPI opens a spidev node in mode 0, 8 bits per word, at speedHz.
// SX126x parts accept up to 16 MHz; a couple of megahertz is plenty and
// far more tolerant of wiring.
func OpenSPI(path string, speedHz uint32) (*SPI, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("lora/linux: open %s: %w", path, err)
	}
	for _, s := range []struct {
		req  uint
		val  int
		name string
	}{
		{iocWrMode, 0, "mode"},
		{iocWrBitsPerWord, 8, "bits per word"},
		{iocWrMaxSpeedHz, int(speedHz), "speed"},
	} {
		if err := unix.IoctlSetPointerInt(int(f.Fd()), s.req, s.val); err != nil {
			f.Close()
			return nil, fmt.Errorf("lora/linux: set %s on %s: %w", s.name, path, err)
		}
	}
	return &SPI{f: f, speed: speedHz}, nil
}

// Transfer clocks tx out and returns what came back on MISO.
func (s *SPI) Transfer(tx []byte) ([]byte, error) {
	if len(tx) == 0 {
		return nil, nil
	}
	rx := make([]byte, len(tx))
	t := spiTransfer{
		txBuf:       uint64(uintptr(unsafe.Pointer(&tx[0]))),
		rxBuf:       uint64(uintptr(unsafe.Pointer(&rx[0]))),
		length:      uint32(len(tx)),
		speedHz:     s.speed,
		bitsPerWord: 8,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, s.f.Fd(), uintptr(iocMessage1),
		uintptr(unsafe.Pointer(&t)))
	// The struct must outlive the syscall; this keeps it reachable.
	runtimeKeepAlive(&t, tx, rx)
	if errno != 0 {
		return nil, fmt.Errorf("lora/linux: SPI transfer: %w", errno)
	}
	return rx, nil
}

// Close releases the bus.
func (s *SPI) Close() error { return s.f.Close() }

var _ lora.SPI = (*SPI)(nil)
