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
	// lastEdge is the kernel's timestamp for the most recent edge, as
	// wall-clock unix nanoseconds; zero until the first edge. The
	// kernel stamps events in its interrupt path, so this is
	// microsecond truth unstretched by scheduling.
	lastEdge atomic.Int64
	// bootWall anchors the kernel's monotonic event clock to the wall.
	bootWall time.Time
}

var _ lora.InterruptPin = (*EdgeLine)(nil)

// Interrupt requests an input watching rising edges. The edge callback
// does nothing but stamp and signal — the "handler is one signal"
// rule — so it never touches SPI and never blocks the kernel's event
// delivery.
func Interrupt(chip string, offset int) (*EdgeLine, error) {
	g := &EdgeLine{offset: offset, edges: make(chan struct{}, 1)}
	// Kernel event timestamps count from boot on the monotonic clock;
	// anchor that origin to the wall once, here.
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return nil, fmt.Errorf("lora/linux: monotonic clock: %w", err)
	}
	g.bootWall = time.Now().Add(-time.Duration(mono.Nano()))
	l, err := gpiocdev.RequestLine(chip, offset,
		gpiocdev.WithRisingEdge,
		gpiocdev.WithConsumer("lora"),
		gpiocdev.WithEventHandler(func(ev gpiocdev.LineEvent) {
			g.lastEdge.Store(g.bootWall.Add(ev.Timestamp).UnixNano())
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
// edge; ok is false until one has been seen.
func (g *EdgeLine) LastEdge() (time.Time, bool) {
	ns := g.lastEdge.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
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
