// TokenTextSplitter 忠实移植 Spring AI 2.0 org.springframework.ai.transformer.splitter.TokenTextSplitter
// （spring-ai-commons 2.0.0，含 2.0.0 的「恰好等于 chunkSize 不做标点截断」语义），
// 参数与句末标点集由知识库向量化配置驱动（对应 Java KnowledgeBaseVectorProperties）。
package textsplit

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/components/document"
	"github.com/cloudwego/eino/schema"
)

// Config 切分策略配置，JSON 序列化顺序与 Java vectorConfigJson 的
// LinkedHashMap 插入顺序逐字段一致（该 JSON 进入 vector_config 快照与
// source_id 的 md5 摘要，两个实例必须产出逐字节相同的快照）。
type Config struct {
	Splitter              string   `json:"splitter"`
	ChunkSize             int      `json:"chunkSize"`
	MinChunkSizeChars     int      `json:"minChunkSizeChars"`
	MinChunkLengthToEmbed int      `json:"minChunkLengthToEmbed"`
	MaxNumChunks          int      `json:"maxNumChunks"`
	KeepSeparator         bool     `json:"keepSeparator"`
	PunctuationMarks      []string `json:"punctuationMarks"`
	StrategyVersion       string   `json:"strategyVersion"`
}

// DefaultConfig 默认值与 Java KnowledgeBaseVectorProperties / application.yml 一致
// （app.ai.rag.vectorization.*，punctuationMarks 中 "\\n" 为字面反斜杠 n 两个字符）。
func DefaultConfig() Config {
	return Config{
		Splitter:              "token",
		ChunkSize:             800,
		MinChunkSizeChars:     350,
		MinChunkLengthToEmbed: 5,
		MaxNumChunks:          10000,
		KeepSeparator:         true,
		PunctuationMarks:      []string{".", "?", "!", "\\n", "。", "？", "！", "；"},
		StrategyVersion:       "token-v1",
	}
}

// ToPunctuationRunes 把标点字符串列表转为字符集合；"\\n" 转义为换行符，
// 其余取首字符（Java KnowledgeBaseVectorProperties.toPunctuationCharacters）。
func (c Config) ToPunctuationRunes() []rune {
	var out []rune
	for _, mark := range c.PunctuationMarks {
		switch {
		case mark == "":
			continue
		case mark == "\\n":
			out = append(out, '\n')
		default:
			out = append(out, []rune(mark)[0])
		}
	}
	return out
}

// ConfigJSON 序列化为 vector_config 快照 JSON。Java 侧用 Jackson（默认不转义
// HTML 字符），Go 这里显式关闭 HTML 转义并去掉 Encode 追加的换行，保证逐字节一致。
func (c Config) ConfigJSON() (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// TokenTextSplitter 按 token 预算切分文本的切分器。
type TokenTextSplitter struct {
	cfg     Config
	puncts  []rune
	lineSep string
}

// NewTokenTextSplitter 按配置构建切分器（Java createTextSplitter 对应入口）。
func NewTokenTextSplitter(cfg Config) *TokenTextSplitter {
	return &TokenTextSplitter{cfg: cfg, puncts: cfg.ToPunctuationRunes(), lineSep: "\n"}
}

// SplitText 把文本切分为若干块（Java splitText → doSplit）。
func (s *TokenTextSplitter) SplitText(text string) ([]string, error) {
	return s.doSplit(text, s.cfg.ChunkSize)
}

// doSplit 忠实移植 Spring AI TokenTextSplitter.doSplit：
//   - 仅当剩余 token 数大于 chunkSize 时才做标点回退截断（恰好等于 chunkSize 不截断）；
//   - 块长不足 minChunkLengthToEmbed 的丢弃；
//   - 循环以「重新编码块文本」的 token 数推进（decode 可能产生 U+FFFD，与 Java 一致）；
//   - 循环耗尽 maxNumChunks 后，剩余 token 兜底为一块（始终把换行替换为空格）。
func (s *TokenTextSplitter) doSplit(text string, chunkSize int) ([]string, error) {
	if javaTrim(text) == "" {
		return nil, nil
	}

	tokens, err := Encode(text)
	if err != nil {
		return nil, err
	}
	chunks := []string{}
	numChunks := 0
	for len(tokens) > 0 && numChunks < s.cfg.MaxNumChunks {
		n := chunkSize
		if len(tokens) < n {
			n = len(tokens)
		}
		chunk := tokens[:n]
		chunkText := Decode(chunk)

		// 跳过空块或纯空白块。
		if javaTrim(chunkText) == "" {
			tokens = tokens[len(chunk):]
			continue
		}

		// 剩余 token 多于 chunkSize 时才按句末标点回退截断。
		if len(tokens) > chunkSize {
			if last, found := s.lastPunctuationIndex(chunkText); found && last > s.cfg.MinChunkSizeChars {
				chunkText = cutUTF16(chunkText, last+1)
			}
		}

		chunkTextToAppend := chunkText
		if !s.cfg.KeepSeparator {
			chunkTextToAppend = strings.ReplaceAll(chunkTextToAppend, s.lineSep, " ")
		}
		chunkTextToAppend = javaTrim(chunkTextToAppend)
		if utf16Len(chunkTextToAppend) > s.cfg.MinChunkLengthToEmbed {
			chunks = append(chunks, chunkTextToAppend)
		}

		// 以重新编码的 token 数推进（Java tokens.subList(getEncodedTokens(chunkText).size(), …)）。
		consumed, err := Encode(chunkText)
		if err != nil {
			return nil, err
		}
		tokens = tokens[len(consumed):]
		numChunks++
	}

	// 剩余 token 兜底块（不占 maxNumChunks 配额，且始终替换换行符）。
	if len(tokens) > 0 {
		remainingText := javaTrim(strings.ReplaceAll(Decode(tokens), s.lineSep, " "))
		if utf16Len(remainingText) > s.cfg.MinChunkLengthToEmbed {
			chunks = append(chunks, remainingText)
		}
	}
	return chunks, nil
}

// lastPunctuationIndex 求任一句末标点在文本中的最大位置（UTF-16 码元索引，
// 对齐 Java String.lastIndexOf）。found=false 表示不含任何标点。
func (s *TokenTextSplitter) lastPunctuationIndex(chunkText string) (int, bool) {
	maxByte := -1
	for _, p := range s.puncts {
		if i := strings.LastIndex(chunkText, string(p)); i > maxByte {
			maxByte = i
		}
	}
	if maxByte < 0 {
		return 0, false
	}
	return utf16Len(chunkText[:maxByte]), true
}

// Transform 实现 eino document.Transformer：切分文本并让每个块继承源文档的
// metadata（Java TextSplitter.doSplitDocuments 的元数据拷贝语义）。
func (s *TokenTextSplitter) Transform(ctx context.Context, src []*schema.Document, _ ...document.TransformerOption) ([]*schema.Document, error) {
	var out []*schema.Document
	for _, d := range src {
		chunks, err := s.SplitText(d.Content)
		if err != nil {
			return nil, err
		}
		for _, c := range chunks {
			meta := make(map[string]any, len(d.MetaData))
			for k, v := range d.MetaData {
				meta[k] = v
			}
			out = append(out, &schema.Document{Content: c, MetaData: meta})
		}
	}
	return out, nil
}

// javaTrim 对齐 Java String.trim：去除首尾 Unicode 码点 <= U+0020 的字符。
// UTF-8 下多字节序列的各字节均 >= 0x80，按字节扫描即可。
func javaTrim(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] <= ' ' {
		start++
	}
	for start < end && s[end-1] <= ' ' {
		end--
	}
	return s[start:end]
}

// utf16Len 字符串的 UTF-16 码元长度（增补平面字符计 2，对齐 Java String.length）。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// cutUTF16 截取前 n 个 UTF-16 码元（n 落在 BMP 字符边界上，
// 对齐 Java substring(0, n)）。
func cutUTF16(s string, n int) string {
	pos := 0
	for i, r := range s {
		if pos >= n {
			return s[:i]
		}
		if r > 0xFFFF {
			pos += 2
		} else {
			pos++
		}
	}
	return s
}
