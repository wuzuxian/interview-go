// Package pdfexport 简历分析报告 PDF 导出，布局与 Java 版 PdfExportService 对齐，
// 内嵌朱雀仿宋与 Noto Sans Symbols 字体以支持中文。
package pdfexport

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/go-pdf/fpdf"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ReportInput 导出简历分析报告所需的输入数据。
type ReportInput struct {
	Filename   string
	UploadedAt *time.Time
	Analysis   *AnalysisReport
}

// AnalysisReport 分析报告数据（与 resume.AnalysisResponse 结构一致，解耦避免循环依赖）。
type AnalysisReport struct {
	OverallScore int
	ScoreDetail  *ScoreDetail
	Summary      string
	Strengths    []string
	Suggestions  []Suggestion
}

// ScoreDetail 各维度评分。
type ScoreDetail struct {
	ContentScore    int
	StructureScore  int
	SkillMatchScore int
	ExpressionScore int
	ProjectScore    int
}

// Suggestion 改进建议。
type Suggestion struct {
	Category       string `json:"category"`
	Priority       string `json:"priority"`
	Issue          string `json:"issue"`
	Recommendation string `json:"recommendation"`
}

//go:embed fonts/ZhuqueFangsong-Regular.ttf
var fontCJK []byte

//go:embed fonts/NotoSansSymbols-Regular.ttf
var fontSymbols []byte

const (
	fontCJKName     = "ZhuqueFangsong"
	fontSymbolsName = "NotoSansSymbols"
)

var (
	colorHeader  = rgb(41, 128, 185)
	colorSection = rgb(52, 73, 94)
	colorGreen   = rgb(39, 174, 96)
	colorYellow  = rgb(241, 196, 15)
	colorRed     = rgb(231, 76, 60)
)

func rgb(r, g, b int) fpdf.RGBType {
	return fpdf.RGBType{R: r, G: g, B: b}
}

func newDoc() (*fpdf.Fpdf, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 18)
	pdf.AddPage()

	// fpdf 内部记录错误状态，加载后用 Error() 检查。
	// 中文字体注册常规/粗体/斜体/粗斜四变体（同字形，仅用于 SetFont 样式查找；
	// 单字重字体无法合成真粗体，样式为 Java 排版意图的近似）。
	for _, style := range []string{"", "B", "I", "BI"} {
		pdf.AddUTF8FontFromBytes(fontCJKName, style, fontCJK)
		if err := pdf.Error(); err != nil {
			return nil, apperr.New(apperr.CodeExportPDFFailed, "创建中文字体失败，请联系管理员")
		}
	}
	pdf.AddUTF8FontFromBytes(fontSymbolsName, "", fontSymbols)
	if err := pdf.Error(); err != nil {
		return nil, apperr.New(apperr.CodeExportPDFFailed, "创建符号字体失败，请联系管理员")
	}
	return pdf, nil
}

// ExportResumeAnalysis 导出简历分析报告，返回 PDF 字节。
func ExportResumeAnalysis(in ReportInput) ([]byte, error) {
	a := in.Analysis
	if a == nil {
		return nil, apperr.New(apperr.CodeAnalysisNotFound, "简历分析结果不存在")
	}
	pdf, err := newDoc()
	if err != nil {
		return nil, err
	}

	// 标题
	pdf.SetFont(fontCJKName, "", 24)
	pdf.SetTextColor(colorHeader.R, colorHeader.G, colorHeader.B)
	pdf.CellFormat(0, 14, "简历分析报告", "", 1, "C", false, 0, "")
	ln := pdf

	// 基本信息
	ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
	ln.SetFont(fontCJKName, "", 14)
	ln.Ln(4)
	sectionTitle(ln, "基本信息")
	ln.SetFont(fontCJKName, "", 11)
	ln.SetTextColor(51, 51, 51)
	ln.CellFormat(0, 7, "文件名: "+sanitize(in.Filename), "", 1, "L", false, 0, "")
	uploadText := "未知"
	if in.UploadedAt != nil {
		uploadText = in.UploadedAt.Format("2006-01-02 15:04:05")
	}
	ln.CellFormat(0, 7, "上传时间: "+uploadText, "", 1, "L", false, 0, "")

	// 总分
	ln.Ln(3)
	ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
	ln.SetFont(fontCJKName, "", 14)
	sectionTitle(ln, "综合评分")
	ln.SetFont(fontCJKName, "", 18)
	scoreColor := scoreColor(a.OverallScore)
	ln.SetTextColor(scoreColor.R, scoreColor.G, scoreColor.B)
	ln.CellFormat(0, 10, fmt.Sprintf("总分: %d / 100", a.OverallScore), "", 1, "L", false, 0, "")

	// 各维度评分
	if a.ScoreDetail != nil {
		ln.Ln(3)
		ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
		ln.SetFont(fontCJKName, "", 14)
		sectionTitle(ln, "各维度评分")

		ln.SetFont(fontCJKName, "", 11)
		addScoreRow(ln, "项目经验", a.ScoreDetail.ProjectScore, 40)
		addScoreRow(ln, "技能匹配度", a.ScoreDetail.SkillMatchScore, 20)
		addScoreRow(ln, "内容完整性", a.ScoreDetail.ContentScore, 15)
		addScoreRow(ln, "结构清晰度", a.ScoreDetail.StructureScore, 15)
		addScoreRow(ln, "表达专业性", a.ScoreDetail.ExpressionScore, 10)
	}

	// 简历摘要
	if a.Summary != "" {
		ln.Ln(3)
		ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
		ln.SetFont(fontCJKName, "", 14)
		sectionTitle(ln, "简历摘要")
		ln.SetTextColor(51, 51, 51)
		ln.SetFont(fontCJKName, "", 11)
		multiParagraph(ln, sanitize(a.Summary))
	}

	// 优势亮点
	if len(a.Strengths) > 0 {
		ln.Ln(3)
		ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
		ln.SetFont(fontCJKName, "", 14)
		sectionTitle(ln, "优势亮点")
		ln.SetTextColor(51, 51, 51)
		ln.SetFont(fontCJKName, "", 11)
		for _, s := range a.Strengths {
			multiParagraph(ln, "• "+sanitize(s))
		}
	}

	// 改进建议
	if len(a.Suggestions) > 0 {
		ln.Ln(3)
		ln.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
		ln.SetFont(fontCJKName, "", 14)
		sectionTitle(ln, "改进建议")
		ln.SetFont(fontCJKName, "", 11)
		for _, sg := range a.Suggestions {
			ln.SetTextColor(51, 51, 51)
			multiParagraph(ln, "【"+sg.Priority+"】"+sanitize(sg.Category))
			multiParagraph(ln, "问题: "+sanitize(sg.Issue))
			multiParagraph(ln, "建议: "+sanitize(sg.Recommendation))
			ln.Ln(3)
		}
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, apperr.Wrap(apperr.CodeExportPDFFailed, "输出 PDF 失败", err)
	}
	return buf.Bytes(), nil
}

func sectionTitle(pdf *fpdf.Fpdf, title string) {
	pdf.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
	pdf.CellFormat(0, 9, title, "", 1, "L", false, 0, "")
}

func addScoreRow(pdf *fpdf.Fpdf, dimension string, score, maxScore int) {
	pdf.SetTextColor(51, 51, 51)
	pdf.CellFormat(80, 8, dimension, "", 0, "L", false, 0, "")
	c := scoreColor(score * 100 / maxScore)
	pdf.SetTextColor(c.R, c.G, c.B)
	pdf.CellFormat(0, 8, fmt.Sprintf("%d / %d", score, maxScore), "", 1, "L", false, 0, "")
}

// multiParagraph 支持自动换行的多行文本输出。
func multiParagraph(pdf *fpdf.Fpdf, text string) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			pdf.Ln(5)
			continue
		}
		pdf.MultiCell(0, 6, line, "", "L", false)
	}
}

// sanitize 移除 emoji 等符号类字符，与 Java sanitizeText 行为一致。
func sanitize(text string) string {
	if text == "" {
		return ""
	}
	var sb strings.Builder
	for _, r := range text {
		if unicode.Is(unicode.So, r) || unicode.Is(unicode.Cs, r) {
			continue
		}
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

func scoreColor(score int) fpdf.RGBType {
	if score >= 80 {
		return colorGreen
	}
	if score >= 60 {
		return colorYellow
	}
	return colorRed
}
