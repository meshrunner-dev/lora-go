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

	regVersionString   = 0x0320 // 16 ASCII bytes identifying the part
	regLoRaSyncWordMSB = 0x0740

	// regIQPolarity carries the errata §15.4 workaround: bit 2 must be
	// cleared when inverted IQ is selected and set when it is not, or
	// inverted-IQ reception is quietly degraded.
	regIQPolarity = 0x0736

	// regRxGain is shared: bits 1:0 select the gain mode, bits 7:2 are
	// AgcSensiAdjust — part of the band-specific RSSI/AGC calibration of
	// DS §6.1.6. The byte is always built from the configured band (see
	// rxGainByte); the classic 0x94/0x96 constants would hardwire the
	// 868-915 MHz column into every channel.
	regRxGain = 0x08AC

	// regSensitivityConfig carries the errata §15.1 workaround: bit 2
	// must be cleared for LoRa at 500 kHz and set for everything else,
	// or 500 kHz reception loses sensitivity.
	regSensitivityConfig = 0x0889

	// The rest of the DS §6.1.6 (Table 6-4) RSSI/AGC calibration set.
	// Semtech: an incorrect value here yields "an incorrect gain
	// selection in LoRa and GFSK mode", i.e. missed detections — this
	// is receive performance, not telemetry cosmetics.
	regAgcRssiMeasCalH = 0x089C // bits 4:0 only
	regAgcRssiMeasCalL = 0x089D
	regAgcGforstPowThr = 0x08B9
	regAgcGainTune     = 0x08F5 // 7 consecutive bytes, through 0x08FB
	regOCPConfig       = 0x08E7

	// regRXPatch is undocumented: setting bit 0 is reported to improve
	// reception, and no Semtech document says why. See
	// Config.UndocumentedRXPatch.
	regRXPatch = 0x08B5

	// The retention list (datasheet §9.6): registers named here survive
	// a warm sleep. Three slots; the first is pointed at the RX gain so
	// boosted gain is not silently lost across Sleep.
	regRetention0 = 0x029F
	regRetention1 = 0x02A0
	regRetention2 = 0x02A1
)

// Standby oscillator selection for SetStandby.
const (
	standbyRC   = 0x00 // 13 MHz RC, lowest power
	standbyXOSC = 0x01 // crystal/TCXO running
)

// Packet type for SetPacketType.
const packetTypeLoRa = 0x01

const xtalHz = 32_000_000
