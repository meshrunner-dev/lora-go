// Command lorabench exercises a LoRa radio on real hardware: it reports
// what the chip says about itself, samples the channel, and listens.
//
// It never transmits.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	Chip string `default:"gpiochip0" help:"GPIO character device carrying the control lines."`

	Reset int `default:"16" help:"BCM line wired to NRESET."`
	Busy  int `default:"24" help:"BCM line wired to BUSY."`
	DIO1  int `default:"22" help:"BCM line wired to DIO1."`

	Freq     uint32 `default:"869618000" help:"Carrier in Hz."`
	SF       uint8  `default:"8" help:"Spreading factor, 5-12."`
	BW       uint32 `default:"62500" help:"Bandwidth in Hz."`
	CR       uint8  `default:"8" help:"Coding rate denominator, 5-8."`
	Preamble uint16 `default:"32" help:"Preamble symbols."`
	SyncWord uint8  `default:"0x12" help:"Sync word."`

	TCXO   uint8         `default:"2" help:"TCXO supply code (2 = 1.8 V), 255 for none."`
	Boost  bool          `help:"Enable boosted RX gain (datasheet 9.6)."`
	Patch  bool          `help:"Enable the undocumented 0x08B5 RX patch."`
	AGC    time.Duration `default:"0" help:"Reset the AGC on this interval while listening (repeaters commonly use 4s)."`
	Listen time.Duration `default:"30s" help:"How long to listen for frames."`
	Scan   time.Duration `default:"0" help:"Sample the noise floor for this long before listening."`
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
	spi, err := linux.OpenSPI(c.SPI, 2_000_000)
	if err != nil {
		return err
	}
	pins := lora.Pins{}
	if pins.Reset, err = linux.Output(c.Chip, c.Reset, true); err != nil {
		return err
	}
	if pins.Busy, err = linux.Input(c.Chip, c.Busy); err != nil {
		return err
	}
	if pins.DIO1, err = linux.Interrupt(c.Chip, c.DIO1); err != nil {
		return err
	}

	radio, err := sx126x.Open(spi, pins, sx126x.Config{
		TCXO:                sx126x.TCXOVoltage(c.TCXO),
		UseDCDC:             true,
		RXBoostedGain:       c.Boost,
		UndocumentedRXPatch: c.Patch,
	})
	if err != nil {
		return err
	}
	defer radio.Close()

	mode, cmd, err := radio.Status()
	if err != nil {
		return err
	}
	fmt.Printf("Radio ouverte         : mode=%s, dernière commande=%s\n", mode, cmd)
	fmt.Printf("Réglages récepteur    : gain boosté=%v, patch 0x08B5=%v\n", c.Boost, c.Patch)
	if errs, err := radio.DeviceErrors(); err == nil {
		fmt.Printf("Erreurs matérielles   : %s\n", errs)
	}

	params := lora.Params{
		Frequency: c.Freq,
		SF:        lora.SpreadingFactor(c.SF),
		BW:        lora.Bandwidth(c.BW),
		CR:        lora.CodingRate(c.CR),
		Preamble:  c.Preamble,
		SyncWord:  c.SyncWord,
		CRC:       true,
	}
	if err := radio.Configure(params); err != nil {
		return err
	}
	fmt.Printf("Canal                 : %.3f MHz SF%d BW%.1fkHz CR4/%d, symbole %v\n",
		float64(params.Frequency)/1e6, params.SF, float64(params.BW)/1000, params.CR,
		params.SymbolDuration().Round(time.Microsecond))
	fmt.Printf("Airtime d'une trame   : 16 o -> %v | 64 o -> %v | 184 o -> %v\n",
		params.Airtime(16).Round(time.Millisecond),
		params.Airtime(64).Round(time.Millisecond),
		params.Airtime(184).Round(time.Millisecond))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// A few channel assessments, timed: this is the primitive
	// listen-before-talk is built on, so its cost matters.
	for i := range 3 {
		start := time.Now()
		busy, err := radio.AssessChannel(ctx, sx126x.CAD4Symbols)
		if err != nil {
			return err
		}
		state := "libre"
		if busy {
			state = "OCCUPÉ"
		}
		fmt.Printf("CAD #%d                : %s en %v\n", i+1, state, time.Since(start).Round(100*time.Microsecond))
	}

	if err := radio.StartReceive(); err != nil {
		return err
	}

	// The noise floor separates a deaf radio from a quiet channel: a
	// connected antenna sits in the -95..-125 dBm range and wanders,
	// while a dead front end reads flat at the very bottom.
	var sum, lo, hi float64
	lo, n := 999.0, 0
	for range 20 {
		v, err := radio.RSSI()
		if err != nil {
			return err
		}
		// A reading of exactly 0 dBm means the chip had nothing to report
		// yet; counting it would wreck the average.
		if v < 0 {
			sum += v
			lo, hi = min(lo, v), max(hi, v)
			n++
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n > 0 {
		thermal := -174 + 10*math.Log10(float64(params.BW))
		fmt.Printf("Plancher de bruit     : %.1f dBm (min %.1f, max %.1f) — bruit thermique théorique %.1f dBm\n",
			sum/float64(n), lo, hi, thermal)
		if sum/float64(n) < thermal+6 {
			fmt.Println("                        au niveau thermique: aucune énergie RF (antenne débranchée ?)")
		}
	}

	if c.Scan > 0 {
		fmt.Println()
		if err := scanFloor(ctx, radio, params, c.Scan); err != nil {
			return err
		}
	}

	fmt.Printf("\nÉcoute %v (Ctrl-C pour arrêter)...\n\n", c.Listen)
	deadline, cancel := context.WithTimeout(ctx, c.Listen)
	defer cancel()

	count := 0
	nextAGC := time.Now().Add(c.AGC)
	agcDone, agcDeferred := 0, 0
	var agcTotal time.Duration
	for {
		// A repeater resets the AGC on a timer; the reset refuses while a
		// frame is arriving, which is the whole point — it never costs a
		// reception, it just waits for the next opportunity.
		if c.AGC > 0 && time.Now().After(nextAGC) {
			resetStart := time.Now()
			switch err := radio.ResetAGC(); {
			case err == nil:
				agcDone++
				agcTotal += time.Since(resetStart)
				if err := radio.StartReceive(); err != nil {
					return err
				}
			case errors.Is(err, sx126x.ErrReceiveInProgress):
				agcDeferred++
			default:
				return err
			}
			nextAGC = time.Now().Add(c.AGC)
		}
		// Bound the wait so periodic maintenance still runs on a quiet
		// channel: a Receive that blocks until the next frame starves
		// everything else the owner has to do.
		wait := deadline
		var cancelWait context.CancelFunc
		if c.AGC > 0 {
			wait, cancelWait = context.WithDeadline(deadline, nextAGC)
		}
		frame, err := radio.Receive(wait)
		if cancelWait != nil {
			cancelWait()
		}
		if err != nil {
			if deadline.Err() != nil {
				break
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue // maintenance window, not a failure
			}
			fmt.Printf("  trame rejetée: %v\n", err)
			continue
		}
		count++
		fmt.Printf("[%s] %d o  RSSI %.1f dBm  SNR %.2f dB\n",
			frame.At.Format("15:04:05.000"), len(frame.Payload), frame.RSSI, frame.SNR)
		fmt.Printf("        %x\n", frame.Payload)
	}
	fmt.Printf("\n%d trame(s) reçue(s)", count)
	if c.AGC > 0 {
		fmt.Printf(", %d reset AGC (%d différés)", agcDone, agcDeferred)
		if agcDone > 0 {
			fmt.Printf(", %v par reset, %.1f%% du temps sourd",
				(agcTotal / time.Duration(agcDone)).Round(100*time.Microsecond),
				100*agcTotal.Seconds()/c.Listen.Seconds())
		}
	}
	fmt.Println(".")
	return nil
}
