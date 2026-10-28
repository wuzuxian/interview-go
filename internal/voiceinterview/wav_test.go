package voiceinterview

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// TestPcmToWavHeader 字节级验证 WAV 头：与 Java
// VoiceInterviewWebSocketHandler.convertPcmToWav（L1276-1314）逐字节对齐——
// 44 字节头、RIFF/WAVE/fmt /data 四段、PCM=1、24000Hz/16bit/单声道、
// 小端 RIFF size（data+36）与 data size。
func TestPcmToWavHeader(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	got := PcmToWav(pcm)

	want := make([]byte, 0, 48)
	want = append(want, 'R', 'I', 'F', 'F')
	want = binary.LittleEndian.AppendUint32(want, 4+36) // RIFF size = data size + 36
	want = append(want, 'W', 'A', 'V', 'E')
	want = append(want, 'f', 'm', 't', ' ')
	want = binary.LittleEndian.AppendUint32(want, 16) // fmt chunk size
	want = binary.LittleEndian.AppendUint16(want, 1)  // audio format = PCM
	want = binary.LittleEndian.AppendUint16(want, 1)  // 单声道
	want = binary.LittleEndian.AppendUint32(want, 24000)
	want = binary.LittleEndian.AppendUint32(want, 24000*1*16/8) // byte rate = 48000
	want = binary.LittleEndian.AppendUint16(want, 1*16/8)       // block align = 2
	want = binary.LittleEndian.AppendUint16(want, 16)           // 16 bit
	want = append(want, 'd', 'a', 't', 'a')
	want = binary.LittleEndian.AppendUint32(want, 4) // data size
	want = append(want, pcm...)

	if !bytes.Equal(got, want) {
		t.Fatalf("WAV 字节与 Java convertPcmToWav 不一致\n got: %v\nwant: %v", got, want)
	}
}

// TestPcmToWav 表驱动覆盖边界输入：长度字段始终正确，PCM 负载原样追加。
func TestPcmToWav(t *testing.T) {
	tests := []struct {
		name string
		pcm  []byte
	}{
		{"空 PCM 仅输出 44 字节头", nil},
		{"单字节 PCM", []byte{0x7F}},
		{"非偶数字节长度不做对齐", []byte{1, 2, 3}},
		{"多块音频拼接后的负载", bytes.Repeat([]byte{0x00, 0x10}, 50)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PcmToWav(tt.pcm)

			if len(got) != 44+len(tt.pcm) {
				t.Fatalf("WAV 总长 = %d，want 44 + %d", len(got), len(tt.pcm))
			}
			if !bytes.Equal(got[44:], tt.pcm) {
				t.Fatalf("PCM 负载被改写\n got: %v\nwant: %v", got[44:], tt.pcm)
			}
			if riff := binary.LittleEndian.Uint32(got[4:8]); riff != uint32(len(tt.pcm)+36) {
				t.Fatalf("RIFF size = %d，want %d", riff, len(tt.pcm)+36)
			}
			if dataSize := binary.LittleEndian.Uint32(got[40:44]); dataSize != uint32(len(tt.pcm)) {
				t.Fatalf("data size = %d，want %d", dataSize, len(tt.pcm))
			}
			if sampleRate := binary.LittleEndian.Uint32(got[24:28]); sampleRate != 24000 {
				t.Fatalf("sample rate = %d，want 24000（对齐 Java 硬编码）", sampleRate)
			}
		})
	}
}

// TestPcmToWavEmptyIsHeaderOnly 空合成结果也应产出合法 WAV（Java synthesizeToWav
// 对空 PCM 仍包头下发）。
func TestPcmToWavEmptyIsHeaderOnly(t *testing.T) {
	got := PcmToWav([]byte{})
	if len(got) != 44 {
		t.Fatalf("空 PCM 的 WAV 长度 = %d，want 44", len(got))
	}
	if want := []byte("RIFF"); !bytes.Equal(got[:4], want) {
		t.Fatalf("RIFF 标识 = %q，want %q", got[:4], want)
	}
	if !reflect.DeepEqual(got[8:12], []byte("WAVE")) {
		t.Fatalf("WAVE 标识 = %q", got[8:12])
	}
}
