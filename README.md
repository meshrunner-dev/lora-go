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

The quickstart lives in [`example_linux_test.go`](example_linux_test.go)
— a compiled example, so the toolchain keeps it honest. The short of it:

```go
radio, err := sx126x.Open(spi, pins, sx126x.Config{TCXO: sx126x.TCXO1V8, UseDCDC: true})
err = radio.Configure(lora.Params{ /* frequency, SF, BW, CR, preamble, sync word */ })

busy, err := radio.AssessChannel(ctx, sx126x.CAD4Symbols) // listen before talk
err = radio.StartReceive()
frame, err := radio.Receive(ctx)          // blocking; Poll()/Events() for event loops
```

Pin numbers are a per-board affair, and vendor documentation is not
always right about them — measure yours.

## Design

**One owner, no hidden locks.** A `Radio` is deliberately not safe for
concurrent use. The chip has one bus, one interrupt line and one set of
latched flags, so exactly one goroutine must own it; a lock inside a
method would only hide the races it cannot prevent. For owners juggling
several clocks — a repeater has at least six — `Poll` collects without
blocking and `Events` exposes the interrupt hint to `select` on.

**The chip's flags are the source of truth.** The driver keeps no shadow
copy of the radio's state, with one documented exception: the chip
latches its reception-progress markers but never ages them, so telling
"a frame is arriving" from "noise tripped the detector an hour ago"
takes a clock. `ReceiveInProgress` expires stale detector state against
the channel's own timing; everything else is asked of the hardware.

**Destructive operations refuse, restore, or say so.** `AssessChannel`
refuses while a frame is arriving or unread, and re-arms reception on
every path out. `ResetAGC` — the periodic front-end restart some sites
need — does the same, and replays the whole channel configuration,
because calibration silently clears settings that have nothing to do
with calibration. `Configure` and `Sleep` document their
post-conditions; `Standby` is the unguarded abort button, on purpose.

**Failure is loud and recoverable.** Every command's status byte is
parsed, so a rejected command or an empty bus fails instead of
succeeding into silence; calibration verdicts are read, not discarded.
`ErrBusyTimeout` means the chip stopped answering, and `Reset` is its
recovery. Corrupt frames are traffic, not faults: `ErrCRC` and
`ErrHeader` are exported so a caller can count them — received-to-corrupt
is the standard site-health ratio.

**The interrupt line is a hint, not the payload.** Whatever watches DIO1
does nothing but signal; the owner reads the chip's flags to learn what
happened. Only frame completion is routed to the line — the progress
markers stay latched but do not drive it — so the edge fires when there
is something to collect. Waits poll on a floor, so a missed edge costs
latency rather than the event itself.

## Contents

| Package | Contents |
|---|---|
| `lora` | modulation parameters with strict validation, symbol/preamble/frame durations, airtime, SPI/GPIO/RF-switch interfaces |
| `lora/linux` | spidev via raw ioctl (no SPI library), GPIO via the chardev uAPI v2 |
| `lora/sx126x` | SX1261/1262/1268 driver: bring-up with TCXO proof, calibration with verdicts, CAD, receive, AGC reset, sleep/wake/reset lifecycle |

`cmd/lorabench` exercises a board end to end — identity, channel
assessment, noise floor, listening — and never transmits.

## Testing

The driver is developed against a scripted chip replaying golden SPI
transcripts of sequences validated on real hardware: the bring-up, the
channel programming bytes, a real frame's reception, the CAD cycle and
its restore, the sleep/wake BUSY inversion. One wrong byte fails the
suite; so does any blanket IRQ clear, anywhere. `task check` runs the
full gate.

## Status

Receive, channel assessment and the recovery lifecycle are implemented,
tested against transcripts and validated on hardware; transmit is not
yet.

## License

[MIT](LICENSE).
