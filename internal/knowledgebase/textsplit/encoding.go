// Package textsplit 忠实移植 Spring AI 2.0 TokenTextSplitter（ADR 0004），
// 供知识库向量化按 token 预算切片；对外实现 eino document.Transformer 接口。
//
// 词表与词法对齐 Java 侧 jtokkit 1.1.0（Spring AI 2.0 的编码实现）：
//   - cl100k_base 词表 go:embed，与 jtokkit jar 内资源逐字节一致，启动零网络依赖；
//   - 词法正则与 BPE 合并由 tiktoken-go 提供（与 OpenAI tiktoken 同源算法）；
//   - encode 语义对齐 jtokkit：文本包含已注册特殊 token（<|endoftext|> 等）时
//     直接报错（jtokkit SpecialEncoder.checkForSpecialTokens 抛
//     UnsupportedOperationException），由调用方走向量化失败路径。
package textsplit

import (
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

//go:embed cl100k_base.tiktoken
var cl100kFile embed.FS

// cl100kPatStr cl100k_base 词法正则，与 OpenAI tiktoken 官方注册值逐字一致。
const cl100kPatStr = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// cl100kSpecialTokens 特殊 token 注册表（tiktoken 官方值；jtokkit 同表）。
var cl100kSpecialTokens = map[string]int{
	"<|endoftext|>":   100257,
	"<|fim_prefix|>":  100258,
	"<|fim_middle|>":  100259,
	"<|fim_suffix|>":  100260,
	"<|endofprompt|>": 100276,
}

// errSpecialTokens 特殊 token 报错文案，与 jtokkit SpecialEncoder 逐字一致。
const errSpecialTokens = "Encoding special tokens is not supported."

var (
	encOnce sync.Once
	enc     *tiktoken.Tiktoken
	encErr  error
)

// cl100k 构建内嵌词表的编码器（单例；词表约 10 万条，解析一次复用）。
func cl100k() (*tiktoken.Tiktoken, error) {
	encOnce.Do(func() {
		data, err := cl100kFile.ReadFile("cl100k_base.tiktoken")
		if err != nil {
			encErr = fmt.Errorf("textsplit: 读取内嵌 cl100k_base 词表失败: %w", err)
			return
		}
		ranks := make(map[string]int, 100256)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			idx := strings.LastIndexByte(line, ' ')
			if idx <= 0 {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(line[:idx])
			if err != nil {
				encErr = fmt.Errorf("textsplit: cl100k_base 词表行解析失败: %w", err)
				return
			}
			rank, err := strconv.Atoi(line[idx+1:])
			if err != nil {
				encErr = fmt.Errorf("textsplit: cl100k_base 词表 rank 解析失败: %w", err)
				return
			}
			ranks[string(raw)] = rank
		}
		bpe, err := tiktoken.NewCoreBPE(ranks, cl100kSpecialTokens, cl100kPatStr)
		if err != nil {
			encErr = fmt.Errorf("textsplit: 构建 cl100k_base BPE 失败: %w", err)
			return
		}
		enc = tiktoken.NewTiktoken(bpe, &tiktoken.Encoding{
			Name:           "cl100k_base",
			PatStr:         cl100kPatStr,
			MergeableRanks: ranks,
			SpecialTokens:  cl100kSpecialTokens,
		}, nil)
	})
	return enc, encErr
}

// Encode 把文本编码为 token 序列（对齐 jtokkit Encoding.encode）：
// 文本包含已注册特殊 token 时返回错误，其余走普通 BPE。
func Encode(text string) ([]int, error) {
	e, err := cl100k()
	if err != nil {
		return nil, err
	}
	if err := checkSpecialTokens(text); err != nil {
		return nil, err
	}
	return e.EncodeOrdinary(text), nil
}

// EncodeCount 返回文本 token 数。
func EncodeCount(text string) (int, error) {
	tokens, err := Encode(text)
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}

// Decode 把 token 序列还原为文本（对齐 jtokkit Encoding.decode 的
// new String(bytes, UTF_8)：按 Java UTF-8 解码器语义，每个非法字节子序列
// 以一个 U+FFFD 替换）。
func Decode(tokens []int) string {
	e, err := cl100k()
	if err != nil {
		return ""
	}
	return javaDecodeUTF8([]byte(e.Decode(tokens)))
}

// javaDecodeUTF8 对齐 new String(bytes, StandardCharsets.UTF_8) 的替换语义
// （JDK sun.nio.cs.UTF_8 解码器 REPLACE 策略）：每个 malformed 子序列产出一个
// U+FFFD——孤立的续字节各计一个子序列；截断的多字节序列按已消费长度计一个
// 子序列；越界续字节按已消费前缀长度计一个子序列。
// 注意：Go 的 strings.ToValidUTF8 把相邻非法字节并为一个 run 只替换一次，
// 与 JDK 语义不同，故此处自行实现。
func javaDecodeUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80: // ASCII
			sb.WriteByte(c)
			i++
		case c < 0xC2: // 孤立续字节 0x80-0xBF，或超长首字节 0xC0/0xC1：malformed 1
			sb.WriteRune(0xFFFD)
			i++
		case c < 0xE0: // C2-DF：二字节序列
			if i+1 >= len(b) { // 截断：malformed 剩余 1 字节
				sb.WriteRune(0xFFFD)
				i++
			} else if b[i+1]&0xC0 != 0x80 { // 第二字节不是续字节：malformed 1
				sb.WriteRune(0xFFFD)
				i++
			} else {
				sb.WriteRune(rune(c&0x1F)<<6 | rune(b[i+1]&0x3F))
				i += 2
			}
		case c < 0xF0: // E0-EF：三字节序列（含 E0/ED 边界）
			lo, hi := byte(0xA0), byte(0xBF)
			if c != 0xE0 {
				lo = 0x80
			}
			if c == 0xED {
				hi = 0x9F
			}
			if i+1 >= len(b) { // 截断：malformed 1
				sb.WriteRune(0xFFFD)
				i++
			} else if b[i+1] < lo || b[i+1] > hi { // 第二字节越界：malformed 1
				sb.WriteRune(0xFFFD)
				i++
			} else if i+2 >= len(b) { // 截断：malformed 2
				sb.WriteRune(0xFFFD)
				i += 2
			} else if b[i+2]&0xC0 != 0x80 { // 第三字节不是续字节：malformed 2
				sb.WriteRune(0xFFFD)
				i += 2
			} else {
				sb.WriteRune(rune(c&0x0F)<<12 | rune(b[i+1]&0x3F)<<6 | rune(b[i+2]&0x3F))
				i += 3
			}
		case c < 0xF5: // F0-F4：四字节序列（含 F0/F4 边界）
			lo, hi := byte(0x90), byte(0xBF)
			if c != 0xF0 {
				lo = 0x80
			}
			if c == 0xF4 {
				hi = 0x8F
			}
			if i+1 >= len(b) {
				sb.WriteRune(0xFFFD)
				i++
			} else if b[i+1] < lo || b[i+1] > hi {
				sb.WriteRune(0xFFFD)
				i++
			} else if i+2 >= len(b) {
				sb.WriteRune(0xFFFD)
				i += 2
			} else if b[i+2]&0xC0 != 0x80 {
				sb.WriteRune(0xFFFD)
				i += 2
			} else if i+3 >= len(b) {
				sb.WriteRune(0xFFFD)
				i += 3
			} else if b[i+3]&0xC0 != 0x80 {
				sb.WriteRune(0xFFFD)
				i += 3
			} else {
				sb.WriteRune(rune(c&0x07)<<18 | rune(b[i+1]&0x3F)<<12 | rune(b[i+2]&0x3F)<<6 | rune(b[i+3]&0x3F))
				i += 4
			}
		default: // F5-FF：malformed 1
			sb.WriteRune(0xFFFD)
			i++
		}
	}
	return sb.String()
}

// checkSpecialTokens 对齐 jtokkit SpecialEncoder.checkForSpecialTokens：
// 仅当文本同时含 "<|" 与 "|>" 且命中任一已注册特殊 token 时报错。
func checkSpecialTokens(text string) error {
	if !strings.Contains(text, "<|") || !strings.Contains(text, "|>") {
		return nil
	}
	for token := range cl100kSpecialTokens {
		if strings.Contains(text, token) {
			return &SpecialTokenError{Token: token}
		}
	}
	return nil
}

// SpecialTokenError 文本包含特殊 token（jtokkit 不支持编码）。
type SpecialTokenError struct {
	Token string
}

func (e *SpecialTokenError) Error() string { return errSpecialTokens }
