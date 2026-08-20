//go:build linux

package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"meshrunner.dev/pkg/lora"
	"meshrunner.dev/pkg/lora/sx126x"
)

// scanFloor samples the received signal strength continuously and
// reports one line per second. Watching the floor move — rather than
// reading it once — is what distinguishes a live front end from a dead
// one: thermal noise wanders by a decibel or two, and any real emitter
// nearby lifts the whole second. This is a diagnostic, not a
// noise-floor estimator; it deliberately wants to see RF energy.
func scanFloor(ctx context.Context, radio *sx126x.Radio, p lora.Params, d time.Duration) error {
	thermal := -174 + 10*math.Log10(float64(p.BW))
	fmt.Printf("Scanning for %v — theoretical thermal floor %.1f dBm\n", d, thermal)
	fmt.Println("  time       n     min    mean     max   >gate  scale -130 .. -60 dBm")

	deadline := time.Now().Add(d)
	var all []float64
	const gate = -110.0 // anything above this is not thermal noise

	for time.Now().Before(deadline) && ctx.Err() == nil {
		second := time.Now().Add(time.Second)
		var s []float64
		above := 0
		for time.Now().Before(second) && ctx.Err() == nil {
			v, err := radio.RSSI()
			if err != nil {
				return err
			}
			if v < 0 { // 0 dBm means the chip had nothing to report yet
				s = append(s, v)
				if v > gate {
					above++
				}
			}
			time.Sleep(4 * time.Millisecond)
		}
		if len(s) == 0 {
			continue
		}
		all = append(all, s...)
		lo, mean, hi := stats(s)
		fmt.Printf("  %s  %4d  %6.1f  %6.1f  %6.1f  %6d  %s\n",
			time.Now().Format("15:04:05"), len(s), lo, mean, hi, above, gauge(lo, hi))
	}

	if len(all) == 0 {
		return nil
	}
	lo, mean, hi := stats(all)
	sort.Float64s(all)
	p50 := all[len(all)/2]
	p99 := all[len(all)*99/100]
	fmt.Printf("\nSummary : %d samples, min %.1f, median %.1f, mean %.1f, p99 %.1f, max %.1f dBm\n",
		len(all), lo, p50, mean, p99, hi)
	switch {
	case hi > gate:
		fmt.Println("Verdict : RF energy is reaching the receiver — the chain hears.")
	case hi-lo < 3:
		fmt.Println("Verdict : floor pinned at thermal, no energy captured (antenna?).")
	default:
		fmt.Println("Verdict : the floor breathes but stays low — front end alive, no emitter nearby.")
	}
	return nil
}

func stats(v []float64) (lo, mean, hi float64) {
	lo, hi = v[0], v[0]
	var sum float64
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
		sum += x
	}
	return lo, sum / float64(len(v)), hi
}

// gauge draws where the second's range sits between -130 and -60 dBm.
func gauge(lo, hi float64) string {
	const width, floor, ceil = 40, -130.0, -60.0
	pos := func(v float64) int {
		i := int((v - floor) / (ceil - floor) * width)
		return max(0, min(width-1, i))
	}
	b := []byte("........................................")
	for i := pos(lo); i <= pos(hi); i++ {
		b[i] = '#'
	}
	return string(b)
}
