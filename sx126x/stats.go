package sx126x

// Stats are the chip's own reception counters, accumulated since the
// last ResetStats (or power-up): an independent second opinion for any
// tally the host keeps itself.
type Stats struct {
	Received     uint16 // frames that reached RxDone, CRC valid or not
	CRCErrors    uint16
	HeaderErrors uint16
}

// Stats reads the counters — one command, three big-endian words.
func (r *Radio) Stats() (Stats, error) {
	rx, err := r.dev.cmd(opGetStats, 0, 0, 0, 0, 0, 0, 0)
	if err != nil {
		return Stats{}, err
	}
	return Stats{
		Received:     uint16(rx[2])<<8 | uint16(rx[3]),
		CRCErrors:    uint16(rx[4])<<8 | uint16(rx[5]),
		HeaderErrors: uint16(rx[6])<<8 | uint16(rx[7]),
	}, nil
}

// ResetStats zeroes the counters.
func (r *Radio) ResetStats() error {
	_, err := r.dev.cmd(opResetStats, 0, 0, 0, 0, 0, 0)
	return err
}
