package sx126x

import "strings"

// DeviceError is the chip's latched error word (GetDeviceErrors,
// datasheet table 13-86). Bits persist until explicitly cleared, so a
// stale error will misdirect a diagnosis if reads are not paired with
// clears deliberately.
type DeviceError uint16

// Device error causes.
const (
	DevErrRC64KCalib DeviceError = 1 << 0
	DevErrRC13MCalib DeviceError = 1 << 1
	DevErrPLLCalib   DeviceError = 1 << 2
	DevErrADCCalib   DeviceError = 1 << 3
	DevErrIMGCalib   DeviceError = 1 << 4
	DevErrXOSCStart  DeviceError = 1 << 5
	DevErrPLLLock    DeviceError = 1 << 6
	DevErrPARamp     DeviceError = 1 << 8
)

var devErrNames = []struct {
	bit  DeviceError
	name string
}{
	{DevErrRC64KCalib, "RC64K calibration"},
	{DevErrRC13MCalib, "RC13M calibration"},
	{DevErrPLLCalib, "PLL calibration"},
	{DevErrADCCalib, "ADC calibration"},
	{DevErrIMGCalib, "image calibration"},
	{DevErrXOSCStart, "crystal start"},
	{DevErrPLLLock, "PLL lock"},
	{DevErrPARamp, "PA ramp"},
}

// String implements fmt.Stringer.
func (e DeviceError) String() string {
	if e == 0 {
		return "none"
	}
	var parts []string
	for _, n := range devErrNames {
		if e&n.bit != 0 {
			parts = append(parts, n.name)
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

// DeviceErrors reads the latched error word. Reading does not clear:
// a health metric must be able to observe without destroying what a
// log line still wants to see. Pair with ClearDeviceErrors.
func (r *Radio) DeviceErrors() (DeviceError, error) { return r.dev.deviceErrors() }

// ClearDeviceErrors resets the latched error word.
func (r *Radio) ClearDeviceErrors() error { return r.dev.clearDeviceErrors() }
