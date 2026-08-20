package sx126x

import "fmt"

// ChipMode is the radio's current operating mode, as reported by
// GetStatus.
type ChipMode uint8

// Chip modes (datasheet table 13-76).
const (
	ModeStandbyRC   ChipMode = 0x2
	ModeStandbyXOSC ChipMode = 0x3
	ModeFS          ChipMode = 0x4
	ModeRx          ChipMode = 0x5
	ModeTx          ChipMode = 0x6
)

// String implements fmt.Stringer.
func (m ChipMode) String() string {
	switch m {
	case ModeStandbyRC:
		return "STDBY_RC"
	case ModeStandbyXOSC:
		return "STDBY_XOSC"
	case ModeFS:
		return "FS"
	case ModeRx:
		return "RX"
	case ModeTx:
		return "TX"
	default:
		return fmt.Sprintf("ChipMode(%d)", uint8(m))
	}
}

// CommandStatus is the outcome of the last command, as reported by
// GetStatus.
type CommandStatus uint8

// Command statuses (datasheet table 13-77).
const (
	CmdDataAvailable CommandStatus = 0x2
	CmdTimeout       CommandStatus = 0x3
	CmdProcessingErr CommandStatus = 0x4
	CmdExecFailure   CommandStatus = 0x5
	CmdTxDone        CommandStatus = 0x6
)

// String implements fmt.Stringer.
func (c CommandStatus) String() string {
	switch c {
	case CmdDataAvailable:
		return "data available"
	case CmdTimeout:
		return "timeout"
	case CmdProcessingErr:
		return "processing error"
	case CmdExecFailure:
		return "exec failure"
	case CmdTxDone:
		return "tx done"
	default:
		return fmt.Sprintf("CommandStatus(%d)", uint8(c))
	}
}

// deviceErrorNames maps the GetDeviceErrors bits to readable causes.
var deviceErrorNames = []struct {
	bit  uint16
	name string
}{
	{errRC64KCalib, "RC64K calibration"},
	{errRC13MCalib, "RC13M calibration"},
	{errPLLCalib, "PLL calibration"},
	{errADCCalib, "ADC calibration"},
	{errIMGCalib, "image calibration"},
	{errXOSCStart, "crystal start"},
	{errPLLLock, "PLL lock"},
	{errPARamp, "PA ramp"},
}

// describeDeviceErrors renders the latched error bits in words.
func describeDeviceErrors(errs uint16) string {
	if errs == 0 {
		return "none"
	}
	out := ""
	for _, e := range deviceErrorNames {
		if errs&e.bit != 0 {
			if out != "" {
				out += ", "
			}
			out += e.name
		}
	}
	if out == "" {
		return fmt.Sprintf("unknown (0x%04X)", errs)
	}
	return out
}
