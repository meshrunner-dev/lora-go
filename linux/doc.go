// Package linux implements the lora transports on Linux: SPI through
// spidev and GPIO through the gpiochip character device.
//
// SPI goes straight to the spidev ioctls rather than through a library:
// the encoding is a few dozen lines and it keeps the dependency surface
// to the GPIO side alone.
package linux
