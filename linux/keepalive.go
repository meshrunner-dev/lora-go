package linux

import "runtime"

// runtimeKeepAlive pins values across a syscall that only sees their
// addresses, so the collector cannot move or free them mid-ioctl.
func runtimeKeepAlive(t *spiTransfer, tx, rx []byte) {
	runtime.KeepAlive(t)
	runtime.KeepAlive(tx)
	runtime.KeepAlive(rx)
}
