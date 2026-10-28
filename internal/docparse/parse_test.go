package docparse

import (
	"os"
	"strings"
	"testing"
)

// 验证 pdftotext 优先路径：中文 CID 字体 PDF 的标签（姓名/性别等）不应出现乱码。
// 该用例依赖本机安装的 poppler（~/.local/poppler/bin/pdftotext）；缺失时跳过。
func TestParsePDFWithPdftotext(t *testing.T) {
	if findPdftotext() == "" {
		t.Skip("pdftotext not installed")
	}
	path := os.Getenv("TEST_PDF")
	if path == "" {
		t.Skip("set TEST_PDF to a pdf file to enable this test")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Parse(data, "简历.pdf")
	if err != nil {
		t.Fatal(err)
	}
	// 乱码特征：Latin-1 补充区字符（如 Ä ö ÿ）在中文简历正文中不应成片出现
	garbage := strings.Count(res.Text, "Ä") + strings.Count(res.Text, "ö") + strings.Count(res.Text, "ÿ")
	if garbage > 3 {
		t.Fatalf("extracted text looks garbled (latin-1 chars=%d): %.200s", garbage, res.Text)
	}
	t.Logf("extracted %d runes, head: %.120s", len([]rune(res.Text)), res.Text)
}
