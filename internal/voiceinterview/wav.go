package voiceinterview

import "encoding/binary"

// PcmToWav 给裸 PCM 数据包 44 字节 WAV 头，供浏览器直接播放（base64 下发前封装）。
//
// 与 Java VoiceInterviewWebSocketHandler.convertPcmToWav（L1276-1314）逐字节对齐：
// 采样率硬编码 24000（Qwen TTS Realtime 输出 24kHz/16bit/单声道 PCM，不随配置变化），
// fmt 块 16 字节、audio format=1（PCM）、单声道、16bit、块对齐 2、字节率 48000，
// RIFF size = data 长度 + 36，data size = PCM 长度，全部小端。
func PcmToWav(pcm []byte) []byte {
	const (
		sampleRate    = 24000 // Java：int sampleRate = 24000（硬编码，注释"Use 24000Hz for Qwen TTS Realtime API"）
		bitsPerSample = 16
		numChannels   = 1
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(pcm)
	fileSize := dataSize + 36 // Java：int fileSize = dataSize + 36

	wav := make([]byte, dataSize+44)
	pos := 0

	// RIFF header
	copy(wav[pos:], "RIFF")
	pos += 4
	binary.LittleEndian.PutUint32(wav[pos:], uint32(fileSize))
	pos += 4
	copy(wav[pos:], "WAVE")
	pos += 4

	// fmt chunk
	copy(wav[pos:], "fmt ")
	pos += 4
	binary.LittleEndian.PutUint32(wav[pos:], 16) // chunk size
	pos += 4
	binary.LittleEndian.PutUint16(wav[pos:], 1) // audio format: 1 = PCM
	pos += 2
	binary.LittleEndian.PutUint16(wav[pos:], numChannels)
	pos += 2
	binary.LittleEndian.PutUint32(wav[pos:], uint32(sampleRate))
	pos += 4
	binary.LittleEndian.PutUint32(wav[pos:], uint32(byteRate))
	pos += 4
	binary.LittleEndian.PutUint16(wav[pos:], uint16(blockAlign))
	pos += 2
	binary.LittleEndian.PutUint16(wav[pos:], bitsPerSample)
	pos += 2

	// data chunk
	copy(wav[pos:], "data")
	pos += 4
	binary.LittleEndian.PutUint32(wav[pos:], uint32(dataSize))
	pos += 4

	copy(wav[pos:], pcm)
	return wav
}
