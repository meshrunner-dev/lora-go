//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"

	"meshrunner.dev/pkg/lora"
)

// spidev ioctl requests, from linux/spi/spidev.h. _IOW('k', n, size)
// packs direction, size, type and number into one word; iocMessage1 is
// derived from the struct size so that any layout drift fails loudly at
// compile time instead of as a runtime ENOTTY.
const (
	iocWrMode        = 0x40016B01 // _IOW('k', 1, __u8)
	iocWrBitsPerWord = 0x40016B03 // _IOW('k', 3, __u8)
	iocWrMaxSpeedHz  = 0x40046B04 // _IOW('k', 4, __u32)
	iocMessage1      = 0x40006B00 | (uint(unsafe.Sizeof(spiTransfer{})) << 16)
)

// spiTransfer mirrors struct spi_ioc_transfer. The layout is ABI, so the
// field order and padding must not be touched; TestSpiTransferABI pins
// the size.
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

var _ lora.SPI = (*SPI)(nil)

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
			err = fmt.Errorf("lora/linux: set %s on %s: %w", s.name, path, err)
			if cerr := f.Close(); cerr != nil {
				err = errors.Join(err, cerr)
			}
			return nil, err
		}
	}
	return &SPI{f: f, speed: speedHz}, nil
}

// Transfer clocks tx out and fills rx with what came back on MISO. The
// two slices must have the same length.
func (s *SPI) Transfer(tx, rx []byte) error {
	if len(tx) != len(rx) {
		return fmt.Errorf("lora/linux: SPI transfer: tx is %d bytes, rx is %d", len(tx), len(rx))
	}
	if len(tx) == 0 {
		return nil
	}

	// The buffer addresses ride through the kernel as integers inside
	// the struct, which is outside the conversion pattern the unsafe
	// rules bless. Pinning makes it legal: the collector can neither
	// move nor free pinned memory, whatever the allocator learns to do.
	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&tx[0])
	pin.Pin(&rx[0])

	t := spiTransfer{
		txBuf:       uint64(uintptr(unsafe.Pointer(&tx[0]))),
		rxBuf:       uint64(uintptr(unsafe.Pointer(&rx[0]))),
		length:      uint32(len(tx)),
		speedHz:     s.speed,
		bitsPerWord: 8,
	}

	// Going through SyscallConn keeps the descriptor referenced for the
	// duration: a concurrent Close yields a clean os.ErrClosed instead
	// of racing the ioctl into an EBADF (or worse, a recycled fd).
	raw, err := s.f.SyscallConn()
	if err != nil {
		return fmt.Errorf("lora/linux: SPI transfer: %w", err)
	}
	var errno unix.Errno
	cerr := raw.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, uintptr(iocMessage1),
			uintptr(unsafe.Pointer(&t)))
	})
	runtime.KeepAlive(&t)
	if cerr != nil {
		return fmt.Errorf("lora/linux: SPI transfer: %w", cerr)
	}
	if errno != 0 {
		return fmt.Errorf("lora/linux: SPI transfer: %w", errno)
	}
	return nil
}

// Close releases the bus.
func (s *SPI) Close() error { return s.f.Close() }
