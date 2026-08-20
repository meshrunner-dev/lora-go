package main

import (
	"fmt"

	"meshrunner.dev/pkg/meshcore"
)

// describe decodes a received frame as a MeshCore packet when it looks
// like one. A bench tool that only prints byte counts cannot tell a
// working radio from a noisy one; a decoded packet can.
func describe(payload []byte) {
	p, err := meshcore.ParsePacket(payload)
	if err != nil {
		fmt.Printf("        pas un paquet MeshCore (%v)\n", err)
		return
	}
	fmt.Printf("        %s\n", p)
}
