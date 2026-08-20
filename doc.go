// Package lora describes the LoRa physical layer: modulation parameters,
// airtime, channel assessment, and the interfaces a transceiver driver
// implements.
//
// It is deliberately protocol-agnostic — frames are opaque bytes — so a
// repeater, a sniffer and a bench tool can share one radio stack. Chip
// drivers live in subpackages (see lora/sx126x); the transports they
// need are small interfaces (SPI, GPIO) with a Linux implementation in
// lora/linux, which keeps everything above them testable without
// hardware.
//
// This is raw LoRa PHY, not LoRaWAN. Not affiliated with or endorsed by
// Semtech.
package lora
