package linux

import (
	"fmt"

	"github.com/warthog618/go-gpiocdev"

	"meshrunner.dev/pkg/lora"
)

// gpioOut / gpioIn / gpioIRQ wrap a single requested line each. One line
// per request keeps ownership and cleanup obvious, which matters more
// here than the small cost of extra file descriptors.

type gpioOut struct{ l *gpiocdev.Line }

// Output requests an output line on chip (e.g. "gpiochip0"), driven to
// the given initial level.
func Output(chip string, offset int, high bool) (lora.OutputPin, error) {
	init := 0
	if high {
		init = 1
	}
	l, err := gpiocdev.RequestLine(chip, offset, gpiocdev.AsOutput(init), gpiocdev.WithConsumer("lora"))
	if err != nil {
		return nil, fmt.Errorf("lora/linux: output %s:%d: %w", chip, offset, err)
	}
	return &gpioOut{l}, nil
}

func (g *gpioOut) Set(high bool) error {
	v := 0
	if high {
		v = 1
	}
	return g.l.SetValue(v)
}
func (g *gpioOut) Close() error { return g.l.Close() }

type gpioIn struct{ l *gpiocdev.Line }

// Input requests a plain input line.
func Input(chip string, offset int) (lora.InputPin, error) {
	l, err := gpiocdev.RequestLine(chip, offset, gpiocdev.AsInput, gpiocdev.WithConsumer("lora"))
	if err != nil {
		return nil, fmt.Errorf("lora/linux: input %s:%d: %w", chip, offset, err)
	}
	return &gpioIn{l}, nil
}

func (g *gpioIn) Get() (bool, error) {
	v, err := g.l.Value()
	return v == 1, err
}
func (g *gpioIn) Close() error { return g.l.Close() }

type gpioIRQ struct {
	l     *gpiocdev.Line
	edges chan struct{}
}

// Interrupt requests an input watching rising edges. The edge callback
// does nothing but a non-blocking send — the "handler is one signal"
// rule — so it never touches SPI and never blocks the kernel's event
// delivery.
func Interrupt(chip string, offset int) (lora.InterruptPin, error) {
	g := &gpioIRQ{edges: make(chan struct{}, 1)}
	l, err := gpiocdev.RequestLine(chip, offset,
		gpiocdev.WithRisingEdge,
		gpiocdev.WithConsumer("lora"),
		gpiocdev.WithEventHandler(func(gpiocdev.LineEvent) {
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

func (g *gpioIRQ) Get() (bool, error) {
	v, err := g.l.Value()
	return v == 1, err
}
func (g *gpioIRQ) Edges() <-chan struct{} { return g.edges }
func (g *gpioIRQ) Close() error           { return g.l.Close() }
