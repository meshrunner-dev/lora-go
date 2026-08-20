package sx126x

// SX126x command opcodes and register addresses (datasheet §11-13). Only
// the ones the driver uses are listed; the names match the datasheet.
const (
	opSetSleep             = 0x84
	opSetStandby           = 0x80
	opSetFs                = 0xC1
	opSetTx                = 0x83
	opSetRx                = 0x82
	opSetCad               = 0xC5
	opSetRegulatorMode     = 0x96
	opCalibrate            = 0x89
	opCalibrateImage       = 0x98
	opSetPaConfig          = 0x95
	opSetRfFrequency       = 0x86
	opSetPacketType        = 0x8A
	opSetTxParams          = 0x8E
	opSetModulationParams  = 0x8B
	opSetPacketParams      = 0x8C
	opSetCadParams         = 0x88
	opSetBufferBaseAddress = 0x8F
	opSetDio2AsRfSwitch    = 0x9D
	opSetDio3AsTcxoCtrl    = 0x97
	opSetDioIrqParams      = 0x08
	opGetIrqStatus         = 0x12
	opClearIrqStatus       = 0x02
	opGetStatus            = 0xC0
	opGetRxBufferStatus    = 0x13
	opGetPacketStatus      = 0x14
	opGetRssiInst          = 0x15
	opGetDeviceErrors      = 0x17
	opClearDeviceErrors    = 0x07
	opWriteRegister        = 0x0D
	opReadRegister         = 0x1D
	opWriteBuffer          = 0x0E
	opReadBuffer           = 0x1E

	regLoRaSyncWordMSB = 0x0740
	regRxGain          = 0x08AC
	regOCPConfig       = 0x08E7
)

// Standby oscillator selection for SetStandby.
const (
	standbyRC   = 0x00 // 13 MHz RC, lowest power
	standbyXOSC = 0x01 // crystal/TCXO running
)

// Packet type for SetPacketType.
const packetTypeLoRa = 0x01

// IRQ flags (datasheet table 13-29), as a bit field.
const (
	irqTxDone         = 1 << 0
	irqRxDone         = 1 << 1
	irqPreambleDetect = 1 << 2
	irqSyncWordValid  = 1 << 3
	irqHeaderValid    = 1 << 4
	irqHeaderErr      = 1 << 5
	irqCRCErr         = 1 << 6
	irqCadDone        = 1 << 7
	irqCadDetected    = 1 << 8
	irqTimeout        = 1 << 9
	irqAll            = 0x03FF
)

// Device-error bits (GetDeviceErrors).
const (
	errRC64KCalib = 1 << 0
	errRC13MCalib = 1 << 1
	errPLLCalib   = 1 << 2
	errADCCalib   = 1 << 3
	errIMGCalib   = 1 << 4
	errXOSCStart  = 1 << 5
	errPLLLock    = 1 << 6
	errPARamp     = 1 << 8
)
