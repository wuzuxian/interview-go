// 语音冒烟的音频纯函数（票 10）：线性插值重采样、200ms 帧切分与 WAV 解析。
//
// 重采样与切帧逐字对齐 Java 前端 audio-worklet/pcm-processor.js（浏览器把麦克风
// 音频喂给 ASR 的真实路径）：resample() 的线性插值公式、float32ToInt16 的向零截断、
// flushChunks 的整帧出队（不足一帧的尾部滞留不下发）。WAV 解析读取服务端
// 44 字节头（PcmToWav：24000Hz/16bit/mono）的下行开场音频。
package main

import (
	"encoding/binary"
	"fmt"
	"math"
)

// pcmSamplesPerFrame 上行帧长：200ms @16kHz = 3200 样本（pcm-processor.js
// samplesPerChunk = 3200；16bit 下即 6400 字节/帧）。
const pcmSamplesPerFrame = 3200

// ResampleLinear 线性插值重采样（pcm-processor.js resample 逐字移植，样本域）：
//
//	ratio = sourceRate / targetRate
//	outLen = max(1, round(len/ratio))
//	out[i] = in[lower]*(1-w) + in[upper]*w，lower=floor(i*ratio)，
//	upper=min(lower+1, len-1)，w=i*ratio-lower
//
// 浮点结果按 JS Int16Array 赋值语义向零截断为 int16。
// 与 JS 的差异：空输入返回空切片（JS 会产出 NaN 垃圾值，Go 侧直接短路更安全）。
func ResampleLinear(pcm []int16, sourceRate, targetRate int) []int16 {
	if sourceRate == targetRate {
		return pcm
	}
	if len(pcm) == 0 {
		return []int16{}
	}
	ratio := float64(sourceRate) / float64(targetRate)
	outLen := int(math.Round(float64(len(pcm)) / ratio))
	if outLen < 1 {
		outLen = 1
	}
	out := make([]int16, outLen)
	last := len(pcm) - 1
	for i := 0; i < outLen; i++ {
		sourceIndex := float64(i) * ratio
		lower := int(math.Floor(sourceIndex))
		if lower > last {
			lower = last
		}
		upper := lower + 1
		if upper > last {
			upper = last
		}
		weight := sourceIndex - math.Floor(sourceIndex)
		out[i] = int16(float64(pcm[lower])*(1-weight) + float64(pcm[upper])*weight)
	}
	return out
}

// FramePCM16 把 PCM 样本切成定长帧（pcm-processor.js flushChunks 语义：
// 只出完整帧，不足一帧的尾部滞留缓冲不下发）。samplesPerFrame ≤ 0 视为无帧。
func FramePCM16(pcm []int16, samplesPerFrame int) [][]int16 {
	if samplesPerFrame <= 0 || len(pcm) < samplesPerFrame {
		return nil
	}
	var frames [][]int16
	for off := 0; off+samplesPerFrame <= len(pcm); off += samplesPerFrame {
		frame := make([]int16, samplesPerFrame)
		copy(frame, pcm[off:off+samplesPerFrame])
		frames = append(frames, frame)
	}
	return frames
}

// ParseWavPCM 解析服务端下发的 WAV（44 字节头）为 16bit 单声道 PCM 样本与采样率。
// 只接受 PCM 编码（audioFormat=1）、16bit、单声道——即 PcmToWav 的产物形状。
func ParseWavPCM(wav []byte) ([]int16, int, error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("不是合法的 WAV（RIFF/WAVE 魔数缺失，前 12 字节: %q）", excerptHead(wav, 12))
	}
	var (
		sampleRate int
		pcmRaw     []byte
		hasData    bool
	)
	pos := 12
	for pos+8 <= len(wav) {
		id := string(wav[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		body := pos + 8
		if body+size > len(wav) {
			return nil, 0, fmt.Errorf("WAV 块 %q 长度 %d 超出数据范围", id, size)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("WAV fmt 块长度 %d 不足 16 字节", size)
			}
			audioFormat := binary.LittleEndian.Uint16(wav[body : body+2])
			channels := binary.LittleEndian.Uint16(wav[body+2 : body+4])
			sampleRate = int(binary.LittleEndian.Uint32(wav[body+4 : body+8]))
			bits := binary.LittleEndian.Uint16(wav[body+14 : body+16])
			if audioFormat != 1 {
				return nil, 0, fmt.Errorf("WAV 编码格式 %d 非 PCM（仅支持服务端下发的 PCM 编码）", audioFormat)
			}
			if bits != 16 {
				return nil, 0, fmt.Errorf("WAV 位深 %dbit 非 16bit", bits)
			}
			if channels != 1 {
				return nil, 0, fmt.Errorf("WAV 声道数 %d 非单声道", channels)
			}
		case "data":
			pcmRaw = wav[body : body+size]
			hasData = true
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // 块按 2 字节对齐（RIFF 规范）
		}
	}
	if sampleRate == 0 {
		return nil, 0, fmt.Errorf("WAV 缺少合法 fmt 块")
	}
	if !hasData {
		return nil, 0, fmt.Errorf("WAV 缺少 data 块")
	}
	pcm := make([]int16, len(pcmRaw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(pcmRaw[i*2:]))
	}
	return pcm, sampleRate, nil
}

// excerptHead 错误信息用的字节头片段（安全截断）。
func excerptHead(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max])
	}
	return string(b)
}
