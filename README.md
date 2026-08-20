# lora-go

> An independent Go implementation of the LoRa physical layer.
> Not affiliated with or endorsed by Semtech.

Raw LoRa PHY — **not LoRaWAN**: modulation parameters, airtime, channel
assessment, and drivers for the transceivers a node actually carries.
Frames are opaque bytes, so a repeater, a sniffer and a bench tool share
one radio stack without any of them agreeing on a protocol.

```sh
go get meshrunner.dev/pkg/lora
```

```go
import (
    "meshrunner.dev/pkg/lora"
    "meshrunner.dev/pkg/lora/linux"
    "meshrunner.dev/pkg/lora/sx126x"
)

spi, _ := linux.OpenSPI("/dev/spidev0.0", 2_000_000)

// Which line carries which signal depends entirely on the board, and
// vendor documentation is not always right — measure yours.
pins := lora.Pins{}
pins.Reset, _ = linux.Output("gpiochip0", nreset, true)
pins.Busy, _ = linux.Input("gpiochip0", busy)
pins.DIO1, _ = linux.Interrupt("gpiochip0", dio1)

radio, err := sx126x.Open(spi, pins, sx126x.Config{TCXO: sx126x.TCXO1V8, UseDCDC: true})
radio.Configure(lora.Params{Frequency: 869_525_000, SF: lora.SF8,
    BW: lora.BW62500, CR: lora.CR8, Preamble: 32, CRC: true})

busy, _ := radio.AssessChannel(ctx, sx126x.CAD4Symbols) // listen before talk
radio.StartReceive()
frame, _ := radio.Receive(ctx)                          // payload, RSSI, SNR
```

## Design

**One owner, no hidden locks.** A `Radio` is deliberately not safe for
concurrent use. The chip has one bus, one interrupt line and one set of
latched flags, so exactly one goroutine must own it; a lock inside a
method would only hide the races it cannot prevent.

**The chip's flags are the source of truth.** The driver keeps no shadow
copy of "a reception is in progress" — `ReceiveInProgress` asks the
hardware. Interrupt flags are cleared narrowly, never with a blanket
sweep, because clearing more than was read discards events that arrived
in between and leaves no edge to announce them.

**The interrupt line is a hint, not the payload.** Whatever watches DIO1
does nothing but signal; the owner then reads the chip's flags to learn
what happened. Waits poll on a floor, so a missed edge costs latency
rather than the event itself.

**Standby is destructive.** It aborts a reception in progress, so it is
an explicit call the caller makes on purpose, never a step buried in
another operation.

## Contents

| Package | Contents |
|---|---|
| `lora` | modulation parameters, symbol duration, airtime, SPI/GPIO interfaces |
| `lora/linux` | spidev via raw ioctl (no dependency), GPIO via the chardev uAPI v2 |
| `lora/sx126x` | SX1261/1262/1268 driver: reset, TCXO, calibration, CAD, receive |

`cmd/lorabench` exercises a board end to end — identity, channel
assessment, noise floor, listening — and never transmits.

## Status

Early. Receive and channel assessment are implemented and validated on
hardware; transmit is not yet.

## License

[MIT](LICENSE).
