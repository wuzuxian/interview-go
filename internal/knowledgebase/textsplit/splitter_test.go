// TokenTextSplitter 黄金用例：期望值由 Java 侧真实类生成（gradle 缓存中的
// spring-ai-commons 2.0.0 TokenTextSplitter + jtokkit 1.1.0，反射调用 doSplit），
// 覆盖中英混排、跨句边界、超长段、纯换行文本、emoji（UTF-16 计数）、
// 生僻字跨 token 边界（decode 产生 U+FFFD）、剩余兜底分支、不保留分隔符等。
package textsplit

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// goldenCase 黄金用例（gold.json，由 Java 侧生成脚本产出）。
type goldenCase struct {
	Name                  string   `json:"name"`
	ChunkSize             int      `json:"chunkSize"`
	MinChunkSizeChars     int      `json:"minChunkSizeChars"`
	MinChunkLengthToEmbed int      `json:"minChunkLengthToEmbed"`
	MaxNumChunks          int      `json:"maxNumChunks"`
	KeepSeparator         bool     `json:"keepSeparator"`
	Input                 string   `json:"input"`
	Chunks                []string `json:"chunks"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	data, err := os.ReadFile("gold.json")
	if err != nil {
		t.Fatalf("读取黄金用例失败: %v", err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("解析黄金用例失败: %v", err)
	}
	return cases
}

// TestTokenTextSplitter黄金用例 与 Java 真实实现的输出逐字节一致。
func TestTokenTextSplitter黄金用例(t *testing.T) {
	for _, c := range loadGolden(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			s := NewTokenTextSplitter(Config{
				ChunkSize:             c.ChunkSize,
				MinChunkSizeChars:     c.MinChunkSizeChars,
				MinChunkLengthToEmbed: c.MinChunkLengthToEmbed,
				MaxNumChunks:          c.MaxNumChunks,
				KeepSeparator:         c.KeepSeparator,
				PunctuationMarks:      DefaultConfig().PunctuationMarks,
			})
			got, err := s.SplitText(c.Input)
			if err != nil {
				t.Fatalf("切分失败: %v", err)
			}
			if len(got) != len(c.Chunks) {
				t.Fatalf("块数不一致: got %d, want %d\ngot  = %q\nwant = %q", len(got), len(c.Chunks), got, c.Chunks)
			}
			for i := range got {
				if got[i] != c.Chunks[i] {
					t.Errorf("第 %d 块不一致:\ngot  = %q\nwant = %q", i, got[i], c.Chunks[i])
				}
			}
		})
	}
}

// TestSplit特殊Token报错 对齐 jtokkit：文本包含已注册特殊 token 时切分失败。
func TestSplit特殊Token报错(t *testing.T) {
	s := NewTokenTextSplitter(DefaultConfig())
	_, err := s.SplitText("模型输出以 <|endoftext|> 结尾。")
	if err == nil {
		t.Fatal("包含特殊 token 的文本应报错")
	}
	var se *SpecialTokenError
	if !errors.As(err, &se) {
		t.Fatalf("应返回 SpecialTokenError, got %v", err)
	}
	if se.Error() != "Encoding special tokens is not supported." {
		t.Errorf("错误文案应与 jtokkit 逐字一致, got %q", se.Error())
	}
	if se.Token != "<|endoftext|>" {
		t.Errorf("Token = %q, want <|endoftext|>", se.Token)
	}
	// 含 "<|" 与 "|>" 但不是已注册特殊 token：正常切分。
	got, err := s.SplitText("模板 <| 占位 |> 不是特殊 token。")
	if err != nil {
		t.Fatalf("伪特殊标记不应报错: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "<| 占位 |>") {
		t.Errorf("伪特殊标记应原样保留, got %v", got)
	}
}

// TestTokenCount已知名词 对齐 jtokkit 的 cl100k_base 计数（防 tiktoken 移植走样）。
func TestTokenCount已知名词(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"hello world", 2},
		{"你好，世界", 6},
		{"", 0},
	}
	for _, c := range cases {
		got, err := EncodeCount(c.text)
		if err != nil {
			t.Fatalf("EncodeCount(%q) 失败: %v", c.text, err)
		}
		if got != c.want {
			t.Errorf("EncodeCount(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// TestConfigJSON快照逐字节对齐Jackson 默认配置的 vector_config 快照
// 必须与 Java vectorConfigJson 输出逐字节一致（进入 source_id 的 md5 摘要）。
func TestConfigJSON快照逐字节对齐Jackson(t *testing.T) {
	got, err := DefaultConfig().ConfigJSON()
	if err != nil {
		t.Fatalf("ConfigJSON 失败: %v", err)
	}
	want := `{"splitter":"token","chunkSize":800,"minChunkSizeChars":350,"minChunkLengthToEmbed":5,` +
		`"maxNumChunks":10000,"keepSeparator":true,` +
		`"punctuationMarks":[".","?","!","\\n","。","？","！","；"],"strategyVersion":"token-v1"}`
	if got != want {
		t.Errorf("vector_config 快照不一致:\ngot  = %s\nwant = %s", got, want)
	}
}

// TestToPunctuationRunes "\\n" 转义与首字符截取。
func TestToPunctuationRunes(t *testing.T) {
	cfg := DefaultConfig()
	got := cfg.ToPunctuationRunes()
	want := []rune{'.', '?', '!', '\n', '。', '？', '！', '；'}
	if len(got) != len(want) {
		t.Fatalf("标点数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("标点[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
