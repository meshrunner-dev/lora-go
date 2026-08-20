//go:build linux

// Command lorabench exercises a LoRa radio on real hardware: it reports
// what the chip says about itself, samples the channel, and listens.
//
// It never transmits.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/alecthomas/kong"

	"meshrunner.dev/pkg/lora"
	"meshrunner.dev/pkg/lora/linux"
	"meshrunner.dev/pkg/lora/sx126x"
)

type cli struct {
	SPI  string `default:"/dev/spidev0.0" help:"spidev node the transceiver is on."`
	Chip string `default:"gpiochip0"      help:"GPIO character device carrying the control lines."`

	Reset int `default:"16" help:"BCM line wired to NRESET."`
	Busy  int `default:"24" help:"BCM line wired to BUSY."`
	DIO1  int `default:"22" help:"BCM line wired to DIO1."`

	Freq     uint32 `default:"869618000" help:"Carrier in Hz."`
	SF       uint8  `default:"8"         help:"Spreading factor, 5-12."`
	BW       uint32 `default:"62500"     help:"Bandwidth in Hz."`
	CR       uint8  `default:"8"         help:"Coding rate denominator, 5-8."`
	Preamble uint16 `default:"32"        help:"Preamble symbols."`
	SyncWord uint8  `default:"0x12"      help:"Sync word."`

	TCXO  string `default:"1.8"                                                                                                help:"TCXO supply voltage (1.6/1.7/1.8/2.2/2.4/2.7/3.0/3.3), or 'none' for a bare crystal."`
	Boost bool   `help:"Enable boosted RX gain (datasheet 9.6): ~1 dB of sensitivity for extra current."`
	Patch bool   `help:"Enable the undocumented 0x08B5 register patch. Nobody knows what it does; measure before trusting it."`

	Scan    time.Duration `default:"0"   help:"Sample the noise floor for this long before listening."`
	Listen  time.Duration `default:"30s" help:"How long to listen for frames."`
	AGC     time.Duration `default:"0"   help:"Reset the AGC on this interval while listening (repeaters commonly use 4s)."`
	CADPeak uint8         `default:"0"   help:"Override the CAD detection peak (0 = Semtech base for the SF). For site studies."`
}

var errUnknownTCXO = errors.New("unknown TCXO voltage")

var tcxoVoltages = map[string]sx126x.TCXOVoltage{
	"none": sx126x.TCXONone,
	"1.6":  sx126x.TCXO1V6,
	"1.7":  sx126x.TCXO1V7,
	"1.8":  sx126x.TCXO1V8,
	"2.2":  sx126x.TCXO2V2,
	"2.4":  sx126x.TCXO2V4,
	"2.7":  sx126x.TCXO2V7,
	"3.0":  sx126x.TCXO3V0,
	"3.3":  sx126x.TCXO3V3,
}

func main() {
	var c cli
	kong.Parse(&c,
		kong.Name("lorabench"),
		kong.Description("Bench-test a LoRa transceiver: identify, assess the channel, listen. Never transmits."))
	if err := c.run(); err != nil {
		fmt.Fprintln(os.Stderr, "lorabench:", err)
		os.Exit(1)
	}
}

func (c *cli) run() error {
	radio, err := c.open()
	if err != nil {
		return err
	}
	defer func() { _ = radio.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	params := lora.Params{
		Frequency: c.Freq,
		SF:        lora.SpreadingFactor(c.SF),
		BW:        lora.Bandwidth(c.BW),
		CR:        lora.CodingRate(c.CR),
		Preamble:  c.Preamble,
		SyncWord:  c.SyncWord,
		CRC:       true,
	}
	if err := c.identify(radio, params); err != nil {
		return err
	}
	if err := c.assess(ctx, radio); err != nil {
		return err
	}
	if err := radio.StartReceive(); err != nil {
		return err
	}
	if c.Scan > 0 {
		fmt.Println()
		if err := scanFloor(ctx, radio, params, c.Scan); err != nil {
			return err
		}
	}
	return c.listen(ctx, radio)
}

// open acquires the transports and hands them to the driver; on any
// failure before that handover, everything acquired so far is released.
func (c *cli) open() (*sx126x.Radio, error) {
	tcxo, ok := tcxoVoltages[c.TCXO]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownTCXO, c.TCXO)
	}

	spi, err := linux.OpenSPI(c.SPI, 2_000_000)
	if err != nil {
		return nil, err
	}
	pins := lora.Pins{}
	// Until the driver takes ownership, everything acquired here is
	// released on the way out of an error.
	handedOver := false
	defer func() {
		if !handedOver {
			_ = spi.Close()
			_ = pins.Close()
		}
	}()
	if pins.Reset, err = linux.Output(c.Chip, c.Reset, true); err != nil {
		return nil, err
	}
	if pins.Busy, err = linux.Input(c.Chip, c.Busy); err != nil {
		return nil, err
	}
	if pins.DIO1, err = linux.Interrupt(c.Chip, c.DIO1); err != nil {
		return nil, err
	}

	radio, err := sx126x.Open(spi, pins, sx126x.Config{
		TCXO:                tcxo,
		UseDCDC:             true,
		RXBoostedGain:       c.Boost,
		UndocumentedRXPatch: c.Patch,
	})
	if err != nil {
		return nil, err
	}
	handedOver = true
	return radio, nil
}

// identify configures the channel and reports what the chip says about
// itself and what the parameters imply.
func (c *cli) identify(radio *sx126x.Radio, params lora.Params) error {
	mode, cmd, err := radio.Status()
	if err != nil {
		return err
	}
	fmt.Printf("Radio open            : %q, mode=%s, last command=%s\n", radio.Version(), mode, cmd)
	if errs, err := radio.DeviceErrors(); err == nil {
		fmt.Printf("Device errors         : %s\n", errs)
	}
	fmt.Printf("Receiver tuning       : boosted gain=%v, 0x08B5 patch=%v\n", c.Boost, c.Patch)

	if err := radio.Configure(params); err != nil {
		return err
	}
	fmt.Printf("Channel               : %.3f MHz SF%d BW%.1fkHz CR4/%d, %v/symbol\n",
		float64(params.Frequency)/1e6, params.SF, float64(params.BW)/1000, params.CR,
		params.SymbolDuration().Round(time.Microsecond))
	fmt.Printf("Frame airtime         : 16 B -> %v | 64 B -> %v | 184 B -> %v\n",
		params.Airtime(16).Round(time.Millisecond),
		params.Airtime(64).Round(time.Millisecond),
		params.Airtime(184).Round(time.Millisecond))
	return nil
}

// assess times a few channel activity detections: the primitive
// listen-before-talk is built on, so its cost is worth knowing.
func (c *cli) assess(ctx context.Context, radio *sx126x.Radio) error {
	for i := range 3 {
		start := time.Now()
		busy, err := radio.AssessChannel(ctx, sx126x.CAD{DetectPeak: c.CADPeak})
		if err != nil {
			return err
		}
		state := "free"
		if busy {
			state = "BUSY"
		}
		fmt.Printf("CAD #%d                : %s in %v\n", i+1, state, time.Since(start).Round(100*time.Microsecond))
	}
	return nil
}

// summary prints the session tallies.
func (c *cli) summary(frames, corrupt, agcDone, agcDeferred int, agcTotal time.Duration) {
	fmt.Printf("\n%d frame(s), %d corrupt", frames, corrupt)
	if c.AGC > 0 {
		fmt.Printf(", %d AGC resets (%d deferred", agcDone, agcDeferred)
		if agcDone > 0 {
			fmt.Printf(", %v each", (agcTotal / time.Duration(agcDone)).Round(100*time.Microsecond))
		}
		fmt.Print(")")
	}
	fmt.Println(".")
}

// listen receives frames until the deadline, resetting the AGC on the
// configured interval. The loop is the owner pattern in miniature:
// bounded waits so periodic maintenance still runs on a quiet channel.
func (c *cli) listen(ctx context.Context, radio *sx126x.Radio) error {
	fmt.Printf("\nListening for %v (Ctrl-C to stop)...\n\n", c.Listen)
	deadline, cancel := context.WithTimeout(ctx, c.Listen)
	defer cancel()

	frames, corrupt := 0, 0
	nextAGC := time.Now().Add(c.AGC)
	agcDone, agcDeferred := 0, 0
	var agcTotal time.Duration
	for {
		if c.AGC > 0 && time.Now().After(nextAGC) {
			start := time.Now()
			switch err := radio.ResetAGC(); {
			case err == nil:
				agcDone++
				agcTotal += time.Since(start)
			case errors.Is(err, sx126x.ErrReceiveInProgress), errors.Is(err, sx126x.ErrUnreadFrame):
				// The guard did its job: never at the price of a frame.
				agcDeferred++
			default:
				return err
			}
			nextAGC = time.Now().Add(c.AGC)
		}

		wait := deadline
		var cancelWait context.CancelFunc
		if c.AGC > 0 {
			wait, cancelWait = context.WithDeadline(deadline, nextAGC)
		}
		frame, err := radio.Receive(wait)
		if cancelWait != nil {
			cancelWait()
		}
		switch {
		case err == nil:
			frames++
			fmt.Printf("[%s] %d B  RSSI %.1f dBm  SNR %.2f dB  airtime %v\n",
				frame.At.Format("15:04:05.000"), len(frame.Payload), frame.RSSI, frame.SNR,
				frame.Airtime.Round(time.Millisecond))
			fmt.Printf("        %x\n", frame.Payload)
		case errors.Is(err, sx126x.ErrCRC), errors.Is(err, sx126x.ErrHeader):
			corrupt++ // expected traffic on a busy band: count, don't print
		case errors.Is(err, context.DeadlineExceeded):
			if deadline.Err() != nil {
				c.summary(frames, corrupt, agcDone, agcDeferred, agcTotal)
				return nil
			}
			// AGC maintenance window on a quiet channel, not a failure.
		default:
			return err
		}
	}
}
