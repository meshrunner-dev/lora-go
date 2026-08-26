//go:build linux

package linux

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/warthog618/go-gpiocdev"
	"golang.org/x/sys/unix"

	"meshrunner.dev/pkg/lora"
)

// Each pin type wraps a single requested line. One line per request
// keeps ownership and cleanup obvious, which matters more here than the
// small cost of extra file descriptors.

// OutLine is an output line, requested through Output.
type OutLine struct {
	l      *gpiocdev.Line
	offset int
}

var _ lora.OutputPin = (*OutLine)(nil)

// Output requests an output line on chip (e.g. "gpiochip0"), driven to
// the given initial level.
func Output(chip string, offset int, high bool) (*OutLine, error) {
	init := 0
	if high {
		init = 1
	}
	l, err := gpiocdev.RequestLine(chip, offset, gpiocdev.AsOutput(init), gpiocdev.WithConsumer("lora"))
	if err != nil {
		return nil, fmt.Errorf("lora/linux: output %s:%d: %w", chip, offset, err)
	}
	return &OutLine{l: l, offset: offset}, nil
}

// Set drives the line.
func (g *OutLine) Set(high bool) error {
	v := 0
	if high {
		v = 1
	}
	if err := g.l.SetValue(v); err != nil {
		return fmt.Errorf("lora/linux: set line %d: %w", g.offset, err)
	}
	return nil
}

// Close releases the line.
func (g *OutLine) Close() error { return g.l.Close() }

// InLine is a plain input line, requested through Input.
type InLine struct {
	l      *gpiocdev.Line
	offset int
}

var _ lora.InputPin = (*InLine)(nil)

// Input requests a plain input line.
func Input(chip string, offset int) (*InLine, error) {
	l, err := gpiocdev.RequestLine(chip, offset, gpiocdev.AsInput, gpiocdev.WithConsumer("lora"))
	if err != nil {
		return nil, fmt.Errorf("lora/linux: input %s:%d: %w", chip, offset, err)
	}
	return &InLine{l: l, offset: offset}, nil
}

// Get reads the line.
func (g *InLine) Get() (bool, error) {
	v, err := g.l.Value()
	if err != nil {
		return false, fmt.Errorf("lora/linux: read line %d: %w", g.offset, err)
	}
	return v == 1, nil
}

// Close releases the line.
func (g *InLine) Close() error { return g.l.Close() }

// EdgeLine is an input line watching rising edges, requested through
// Interrupt.
type EdgeLine struct {
	l      *gpiocdev.Line
	offset int
	edges  chan struct{}
	// lastEdge is the kernel's timestamp for the most recent edge, kept
	// exactly as the kernel gave it: nanoseconds on the monotonic
	// clock, zero until the first edge. The kernel stamps events in its
	// interrupt path, so this is microsecond truth unstretched by
	// scheduling — and storing it unconverted keeps it true across a
	// wall-clock step, which a host without an RTC takes as soon as it
	// finds the network.
	lastEdge atomic.Int64
}

var _ lora.InterruptPin = (*EdgeLine)(nil)

// Interrupt requests an input watching rising edges. The edge callback
// does nothing but stamp and signal — the "handler is one signal"
// rule — so it never touches SPI and never blocks the kernel's event
// delivery.
func Interrupt(chip string, offset int) (*EdgeLine, error) {
	g := &EdgeLine{offset: offset, edges: make(chan struct{}, 1)}
	l, err := gpiocdev.RequestLine(chip, offset,
		gpiocdev.WithRisingEdge,
		gpiocdev.WithConsumer("lora"),
		gpiocdev.WithEventHandler(func(ev gpiocdev.LineEvent) {
			g.lastEdge.Store(int64(ev.Timestamp))
			select {
			case g.edges <- struct{}{}:
			default: // a pending signal already says "go look"
			}
		}))
	if err != nil {
		return nil, fmt.Errorf("lora/linux: interrupt %s:%d: %w", chip, offset, err)
	}
	g.l = l
	return g, nil
}

// LastEdge reports the kernel's timestamp for the most recent rising
// edge, as a wall-clock instant; ok is false until one has been seen.
//
// The conversion happens here, against a monotonic reading taken now,
// rather than against an origin captured at start-up: on a host whose
// clock steps once the network answers — every Raspberry Pi without an
// RTC — an origin captured before the step would be wrong by the step
// for the life of the process, and every edge with it.
func (g *EdgeLine) LastEdge() (time.Time, bool) {
	edge := g.lastEdge.Load()
	if edge == 0 {
		return time.Time{}, false
	}
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return time.Time{}, false
	}
	age := time.Duration(mono.Nano() - edge)
	return time.Now().Add(-age), true
}

// Get reads the line's current level.
func (g *EdgeLine) Get() (bool, error) {
	v, err := g.l.Value()
	if err != nil {
		return false, fmt.Errorf("lora/linux: read line %d: %w", g.offset, err)
	}
	return v == 1, nil
}

// Edges returns the edge-hint channel.
func (g *EdgeLine) Edges() <-chan struct{} { return g.edges }

// Close releases the line, waiting out any in-flight event handler.
func (g *EdgeLine) Close() error { return g.l.Close() }
