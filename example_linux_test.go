//go:build linux

package lora_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"meshrunner.dev/pkg/lora"
	"meshrunner.dev/pkg/lora/linux"
	"meshrunner.dev/pkg/lora/sx126x"
)

// Example_receive brings up an SX1262 on a Linux board and listens.
// This is the README quickstart, kept compiling by the toolchain.
func Example_receive() {
	// Which line carries which signal depends entirely on the board,
	// and vendor documentation is not always right — measure yours.
	const nreset, busy, dio1 = 16, 24, 22

	spi, err := linux.OpenSPI("/dev/spidev0.0", 2_000_000)
	if err != nil {
		log.Fatal(err)
	}
	pins := lora.Pins{}
	if pins.Reset, err = linux.Output("gpiochip0", nreset, true); err != nil {
		log.Fatal(err)
	}
	if pins.Busy, err = linux.Input("gpiochip0", busy); err != nil {
		log.Fatal(err)
	}
	if pins.DIO1, err = linux.Interrupt("gpiochip0", dio1); err != nil {
		log.Fatal(err)
	}

	// Open verifies the chip is really there and that its oscillator
	// starts: TCXO mistakes fail here, loudly, not later and silently.
	radio, err := sx126x.Open(spi, pins, sx126x.Config{
		TCXO:    sx126x.TCXO1V8,
		UseDCDC: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer radio.Close()

	err = radio.Configure(lora.Params{
		Frequency: 869_525_000,
		SF:        lora.SF8,
		BW:        lora.BW62500,
		CR:        lora.CR8,
		Preamble:  32,   // a network agreement: declare it, never default it
		SyncWord:  0x12, // private networks
		CRC:       true,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Listen before talk: the scan refuses rather than destroy a
	// reception, and hands the radio back the way it found it.
	ctx := context.Background()
	if busy, err := radio.AssessChannel(ctx, sx126x.CAD4Symbols); err == nil && !busy {
		fmt.Println("channel is free")
	}

	if err := radio.StartReceive(); err != nil {
		log.Fatal(err)
	}
	for {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		frame, err := radio.Receive(ctx)
		cancel()
		switch {
		case err == nil:
			fmt.Printf("%d bytes at %.0f dBm, %v of airtime\n",
				len(frame.Payload), frame.RSSI, frame.Airtime)
		case errors.Is(err, sx126x.ErrCRC), errors.Is(err, sx126x.ErrHeader):
			continue // corrupt frames are traffic, not faults
		default:
			log.Fatal(err)
		}
	}
}
