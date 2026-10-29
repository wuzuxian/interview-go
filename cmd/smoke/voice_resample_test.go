// 语音冒烟纯函数的单元测试（票 10）：线性插值重采样（逐字对齐 Java 前端
// audio-worklet/pcm-processor.js 的 resample/float32ToInt16 语义）、200ms 帧切分
// （对齐 flushChunks 的整帧出队、尾帧滞留）与 WAV 解析（读取服务端 44 字节头
// 24kHz WAV）。表驱动 + 中文用例名，只测纯函数外部行为。
package main

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/wuzuxian/interview/internal/voiceinterview"
)

func TestResampleLinear(t *testing.T) {
	cases := []struct {
		name       string
		pcm        []int16
		sourceRate int
		targetRate int
		want       []int16
	}{
		{
			name:       "同采样率原样返回",
			pcm:        []int16{1, -2, 3},
			sourceRate: 16000,
			targetRate: 16000,
			want:       []int16{1, -2, 3},
		},
		{
			name:       "24000降16000按pcm-processor插值公式逐点对齐",
			pcm:        []int16{100, 200, 400, 800},
			sourceRate: 24000,
			targetRate: 16000,
			// outLen=round(4/1.5)=3；i=1 落在 1.5 → (200+400)/2=300；i=2 落在 3.0 → 钳制到末样本。
			want: []int16{100, 300, 800},
		},
		{
			name:       "插值半点值向零截断（对齐 JS Int16Array 赋值）",
			pcm:        []int16{0, 100, 201, 50},
			sourceRate: 24000,
			targetRate: 16000,
			// i=1 → (100+201)/2=150.5 → 150。
			want: []int16{0, 150, 50},
		},
		{
			name:       "负样本半点值向零截断",
			pcm:        []int16{-100, -201, 100, 50},
			sourceRate: 24000,
			targetRate: 16000,
			// i=1 → (-201+100)/2=-50.5 → -50。
			want: []int16{-100, -50, 50},
		},
		{
			name:       "输出长度钳制不越界读取",
			pcm:        []int16{7, 9},
			sourceRate: 24000,
			targetRate: 16000,
			// outLen=round(2/1.5)=1 → 只取首样本。
			want: []int16{7},
		},
		{
			name:       "空输入返回空输出",
			pcm:        []int16{},
			sourceRate: 24000,
			targetRate: 16000,
			want:       []int16{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResampleLinear(tc.pcm, tc.sourceRate, tc.targetRate)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("重采样结果不符: got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestResampleLinearLengthMatchesPcmProcessorFormula(t *testing.T) {
	// 长度公式 round(len/ratio) 与 pcm-processor.js 一致：24k→16k 时 3 进 2 出。
	pcm := make([]int16, 48000) // 2 秒 @24kHz → 32000 样本 @16kHz
	got := ResampleLinear(pcm, 24000, 16000)
	if len(got) != 32000 {
		t.Fatalf("2 秒 24kHz 音频重采样后应得 32000 样本，实际 %d", len(got))
	}
}

func TestFramePCM16(t *testing.T) {
	cases := []struct {
		name      string
		pcm       []int16
		frameSize int
		want      [][]int16
	}{
		{
			name:      "整除时逐帧切出",
			pcm:       []int16{1, 2, 3, 4, 5, 6, 7, 8},
			frameSize: 4,
			want:      [][]int16{{1, 2, 3, 4}, {5, 6, 7, 8}},
		},
		{
			name:      "不足整帧的尾部丢弃（对齐 flushChunks 滞留语义）",
			pcm:       []int16{1, 2, 3, 4, 5, 6},
			frameSize: 4,
			want:      [][]int16{{1, 2, 3, 4}},
		},
		{
			name:      "不足一帧时不产帧",
			pcm:       []int16{1, 2},
			frameSize: 4,
			want:      nil,
		},
		{
			name:      "空输入不产帧",
			pcm:       []int16{},
			frameSize: 4,
			want:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FramePCM16(tc.pcm, tc.frameSize)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("切帧结果不符: got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestFramePCM16SmokeFrameSize(t *testing.T) {
	// 冒烟实际帧长：200ms @16kHz = 3200 样本/帧（pcm-processor.js samplesPerChunk）。
	if pcmSamplesPerFrame != 3200 {
		t.Fatalf("帧长常量应为 3200 样本（200ms@16kHz），实际 %d", pcmSamplesPerFrame)
	}
	pcm := make([]int16, 3200*3)
	frames := FramePCM16(pcm, pcmSamplesPerFrame)
	if len(frames) != 3 {
		t.Fatalf("9600 样本应切 3 帧，实际 %d 帧", len(frames))
	}
	for i, f := range frames {
		if len(f) != pcmSamplesPerFrame {
			t.Fatalf("第 %d 帧长度应为 %d，实际 %d", i, pcmSamplesPerFrame, len(f))
		}
	}
}

func TestParseWavPCM(t *testing.T) {
	t.Run("解析服务端 PcmToWav 产物", func(t *testing.T) {
		src := []int16{10, -20, 300, -400, 5000}
		raw := make([]byte, 0, len(src)*2)
		for _, s := range src {
			raw = binary.LittleEndian.AppendUint16(raw, uint16(s))
		}
		wav := voiceinterview.PcmToWav(raw) // 服务端 44 字节头（24000Hz/16bit/mono）

		pcm, rate, err := ParseWavPCM(wav)
		if err != nil {
			t.Fatalf("合法 WAV 应解析成功: %v", err)
		}
		if rate != 24000 {
			t.Fatalf("采样率应为 24000，实际 %d", rate)
		}
		if !reflect.DeepEqual(pcm, src) {
			t.Fatalf("PCM 样本不符: got=%v want=%v", pcm, src)
		}
	})

	t.Run("非 RIFF 魔数报错", func(t *testing.T) {
		if _, _, err := ParseWavPCM([]byte("not-a-wav-file")); err == nil {
			t.Fatal("非 WAV 数据应报错")
		}
	})

	t.Run("头部长度不足报错", func(t *testing.T) {
		if _, _, err := ParseWavPCM([]byte("RIFF")); err == nil {
			t.Fatal("截断的 WAV 应报错")
		}
	})

	t.Run("非 PCM 编码报错", func(t *testing.T) {
		wav := testWav(t, 3 /*audioFormat*/, 1, 16, []byte{0, 0})
		if _, _, err := ParseWavPCM(wav); err == nil {
			t.Fatal("非 PCM 编码应报错")
		}
	})

	t.Run("非 16bit 报错", func(t *testing.T) {
		wav := testWav(t, 1, 1, 8, []byte{0})
		if _, _, err := ParseWavPCM(wav); err == nil {
			t.Fatal("非 16bit 应报错")
		}
	})

	t.Run("多声道报错", func(t *testing.T) {
		wav := testWav(t, 1, 2, 16, []byte{0, 0, 0, 0})
		if _, _, err := ParseWavPCM(wav); err == nil {
			t.Fatal("多声道应报错")
		}
	})

	t.Run("缺少 data 块报错", func(t *testing.T) {
		wav := testWav(t, 1, 1, 16, nil)
		wav = wav[:len(wav)-8] // 去掉 data 块头，模拟只有 fmt 的 WAV
		if _, _, err := ParseWavPCM(wav); err == nil {
			t.Fatal("缺少 data 块应报错")
		}
	})

	t.Run("data 块长度为奇数时按块对齐解析后续块", func(t *testing.T) {
		// data(size=3, payload 01 00 AB) 是奇数长度：解出 1 个样本，尾字节随
		// 奇数块对齐填充跳过，随后 4 字节 junk 块不干扰解析。
		wav := testWav(t, 1, 1, 16, []byte{1, 0, 0xAB})
		wav = append(wav, 0x00) // RIFF 块对齐填充（奇数长度块后 1 字节，不计入块长）
		wav = append(wav, []byte{'j', 'u', 'n', 'k'}...)
		wav = binary.LittleEndian.AppendUint32(wav, 4)
		wav = append(wav, []byte{'a', 'b', 'c', 'd'}...)
		pcm, _, err := ParseWavPCM(wav)
		if err != nil {
			t.Fatalf("奇数 data 块应解析成功: %v", err)
		}
		if len(pcm) != 1 || pcm[0] != 1 {
			t.Fatalf("应解出单样本 [1]，实际 %v", pcm)
		}
	})
}

// testWav 手工构造最小 WAV：RIFF 头 + fmt 块（指定编码/声道/位深）+ data 块。
func testWav(t *testing.T, audioFormat, channels, bits int, data []byte) []byte {
	t.Helper()
	wav := []byte("RIFF")
	wav = binary.LittleEndian.AppendUint32(wav, uint32(36+len(data)))
	wav = append(wav, "WAVE"...)
	wav = append(wav, "fmt "...)
	wav = binary.LittleEndian.AppendUint32(wav, 16)
	wav = binary.LittleEndian.AppendUint16(wav, uint16(audioFormat))
	wav = binary.LittleEndian.AppendUint16(wav, uint16(channels))
	wav = binary.LittleEndian.AppendUint32(wav, 24000)
	wav = binary.LittleEndian.AppendUint32(wav, uint32(24000*channels*bits/8))
	wav = binary.LittleEndian.AppendUint16(wav, uint16(channels*bits/8))
	wav = binary.LittleEndian.AppendUint16(wav, uint16(bits))
	wav = append(wav, "data"...)
	wav = binary.LittleEndian.AppendUint32(wav, uint32(len(data)))
	wav = append(wav, data...)
	return wav
}
