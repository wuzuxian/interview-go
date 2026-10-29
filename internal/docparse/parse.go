// Package docparse 从上传文档中提取纯文本，支持 PDF / DOCX / DOC / TXT / MD，
// 对应 Java 版 DocumentParseService + TextCleaningService 的能力子集。
package docparse

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gabriel-vasile/mimetype"
	"github.com/ledongthuc/pdf"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ParseResult 解析结果。
type ParseResult struct {
	Text        string
	ContentType string
}

// Parse 按内容嗅探的 MIME 类型解析文档。
func Parse(data []byte, fileName string) (*ParseResult, error) {
	text, ct, err := ExtractText(data, fileName)
	if err != nil {
		return nil, err
	}
	return &ParseResult{Text: CleanText(text), ContentType: ct}, nil
}

// ExtractText 仅提取文本（不做清洗），返回 (文本, MIME 类型)。
// 知识库链路用：解析后按 Java TextCleaningService.cleanText 单独清洗。
func ExtractText(data []byte, fileName string) (string, string, error) {
	ct := DetectContentType(data, fileName)
	text, err := parseByType(ct, data, fileName)
	return text, ct, err
}

// DetectContentType 基于文件内容嗅探 MIME，失败时按扩展名兜底。
func DetectContentType(data []byte, fileName string) string {
	if len(data) > 0 {
		if m := mimetype.Detect(data); m != nil {
			return m.String()
		}
	}
	return contentTypeByExtension(fileNameExt(fileName))
}

// contentTypeByExtension 常见简历格式的扩展名 → MIME 映射。
func contentTypeByExtension(ext string) string {
	switch ext {
	case "pdf":
		return "application/pdf"
	case "doc":
		return "application/msword"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "md", "markdown", "mdown":
		return "text/markdown"
	case "txt", "text":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

func fileNameExt(name string) string {
	idx := strings.LastIndexByte(name, '.')
	if idx < 0 {
		return ""
	}
	return strings.ToLower(name[idx+1:])
}

func parseByType(ct string, data []byte, fileName string) (string, error) {
	switch {
	case strings.Contains(ct, "pdf"):
		return parsePDF(data)
	case strings.Contains(ct, "wordprocessingml"):
		return parseDOCX(data)
	case strings.Contains(ct, "msword"):
		return parseDOC(data)
	case strings.Contains(ct, "rtf"):
		return ParseRTF(data), nil
	case strings.HasPrefix(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"):
		return string(data), nil
	default:
		// 扩展名兜底：md/markdown/txt 等浏览器可能给 application/octet-stream。
		switch fileNameExt(fileName) {
		case "md", "markdown", "mdown", "txt", "text":
			return string(data), nil
		case "rtf":
			return ParseRTF(data), nil
		case "docx":
			return parseDOCX(data)
		case "doc":
			return parseDOC(data)
		case "pdf":
			return parsePDF(data)
		}
		return "", apperr.New(apperr.CodeResumeParseFailed, "不支持的文件类型: "+ct)
	}
}

// parsePDF 提取 PDF 文本。
// 优先使用 poppler 的 pdftotext（对内嵌 CID 字体中文名词的映射远好于纯 Go 方案，
// 纯 Go 提取常见中文简历会出现成段乱码并导致评分失真）；不可用时回退纯 Go 提取。
func parsePDF(data []byte) (string, error) {
	if path := findPdftotext(); path != "" {
		if text, err := parsePDFByPdftotext(path, data); err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
		// pdftotext 失败或输出为空时继续走纯 Go 回退。
	}
	return parsePDFPureGo(data)
}

// pdfToolPathOnce 缓存 pdftotext 探测结果，避免每次上传重复 LookPath。
var pdfToolPathOnce struct {
	sync.Once
	path string
}

// findPdftotext 探测可用的 pdftotext：依次尝试 PATH 与常见的本地安装位置。
func findPdftotext() string {
	pdfToolPathOnce.Do(func() {
		if p, err := exec.LookPath("pdftotext"); err == nil {
			pdfToolPathOnce.path = p
			return
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, ".local", "poppler", "bin", "pdftotext")
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				pdfToolPathOnce.path = candidate
			}
		}
	})
	return pdfToolPathOnce.path
}

// pdftotextTimeout 单次提取超时，防止异常 PDF 卡住异步消费者。
const pdftotextTimeout = 60 * time.Second

// parsePDFByPdftotext 调用外部 pdftotext 提取文本（临时文件入、stdout 出）。
func parsePDFByPdftotext(toolPath string, data []byte) (string, error) {
	tmp, err := os.CreateTemp("", "docparse-*.pdf")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdftotextTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, toolPath, "-enc", "UTF-8", tmpName, "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pdftotext: %w", err)
	}
	return string(out), nil
}

// parsePDFPureGo 纯 Go PDF 文本提取（文本型 PDF 的回退方案）。
func parsePDFPureGo(data []byte) (string, error) {
	reader := bytes.NewReader(data)
	r, err := pdf.NewReader(reader, int64(len(data)))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeResumeParseFailed, "简历解析失败：无法读取 PDF", err)
	}
	var sb strings.Builder
	pages := r.NumPage()
	for i := 1; i <= pages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		sb.WriteString(txt)
		sb.WriteString("\n")
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return "", apperr.New(apperr.CodeResumeParseFailed, "无法从文件中提取文本内容，请确保文件不是扫描版PDF")
	}
	return out, nil
}

// parseDOCX 解析 DOCX（OOXML）：提取 word/document.xml 的 w:t 文本节点。
func parseDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeResumeParseFailed, "简历解析失败：无法读取 DOCX", err)
	}
	var docFile *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", apperr.New(apperr.CodeResumeParseFailed, "简历解析失败：DOCX 缺少 document.xml")
	}
	rc, err := docFile.Open()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeResumeParseFailed, "简历解析失败", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeResumeParseFailed, "简历解析失败", err)
	}

	var sb strings.Builder
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	depth := 0
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", apperr.Wrap(apperr.CodeResumeParseFailed, "简历解析失败", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p": // 段落
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
			case "tab":
				sb.WriteString("\t")
			case "br":
				sb.WriteString("\n")
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth > 0 {
				sb.Write(t)
			}
		}
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return "", apperr.New(apperr.CodeResumeParseFailed, "无法从文件中提取文本内容")
	}
	return out, nil
}

// parseDOC 解析 Word 97-2003 二进制：尽力提取 WordDocument 流中的可读文本。
func parseDOC(data []byte) (string, error) {
	text := extractDOCText(data)
	if strings.TrimSpace(text) == "" {
		return "", apperr.New(apperr.CodeResumeParseFailed, "无法从 DOC 文件中提取文本内容，请另存为 DOCX 或 PDF 后重试")
	}
	return text, nil
}

// extractDOCText 简化的 OLE/WordDocument 文本提取：
// 优先尝试 piece table 太复杂，这里按可打印 UTF-16/ASCII 片段拼接。
func extractDOCText(data []byte) string {
	var sb strings.Builder
	i := 0
	n := len(data)
	run := make([]byte, 0, 256)
	flush := func() {
		if len(run) >= 8 { // 过短片段多为结构数据，丢弃
			sb.Write(run)
			sb.WriteByte('\n')
		}
		run = run[:0]
	}
	for i < n {
		b := data[i]
		// UTF-16LE 常见中日韩/ASCII 文本
		if i+1 < n && b >= 0x20 && b < 0xFF && (data[i+1] == 0x00 || (b >= 0x4E && b <= 0x9F && data[i+1] >= 0x30 && data[i+1] <= 0xFF)) {
			if data[i+1] == 0x00 {
				run = append(run, b)
			} else {
				// 尝试按 UTF-16 收集一个词组
				j := i
				var seg []byte
				for j+1 < n && ((data[j] >= 0x20 && data[j] < 0xFF && data[j+1] == 0x00) || (data[j] >= 0x0A && data[j] <= 0x0D && data[j+1] == 0x00)) {
					seg = append(seg, data[j], data[j+1])
					j += 2
				}
				if len(seg) >= 16 {
					run = append(run, seg...)
					i = j
					continue
				}
				run = append(run, b, data[i+1])
			}
			i += 2
			continue
		}
		if b == '\n' || b == '\r' {
			flush()
			i++
			continue
		}
		flush()
		i++
	}
	flush()
	return sb.String()
}

var (
	multipleSpaces   = regexp.MustCompile(`[ \t]{2,}`)
	multipleNewlines = regexp.MustCompile(`\n{3,}`)
)

// CleanText 清洗解析出的文本：去除控制字符、压缩空白，对应 Java TextCleaningService。
func CleanText(text string) string {
	if text == "" {
		return ""
	}
	var sb strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t' || r == ' ':
			sb.WriteRune(r)
		case unicode.IsPrint(r):
			sb.WriteRune(r)
		default:
			sb.WriteByte(' ')
		}
	}
	out := sb.String()
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = multipleSpaces.ReplaceAllString(out, " ")
	out = multipleNewlines.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}
