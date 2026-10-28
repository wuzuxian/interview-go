// 面试评估报告 PDF（票 08）。章节顺序、标题文案、字段口径对齐 Java
// PdfExportService.exportInterviewReport（笔记 07 逐字）：
// 标题 → 面试信息 → 综合评分 → 总体评价 → 表现优势 → 改进建议 → 问答详情。
// 无页眉页脚、带书签树（裁决 #10）；版式不逐像素复刻 iText。
//
// 与 Java 的有意差异（票面验收要求的补充，均在报告中注明）：
//   - "分类得分"节（票面验收"四类章节"之一，Java PDF 无此节），仅已评估（有总分）时渲染；
//   - 问答详情每题补"要点"（票面验收"参考答案与要点"）与"（追问）"标记（票面字段口径）；
//   - 问答详情遍历全部题目（复用票 06 details 语义），未作答以"未回答"呈现（票面空值口径），
//     而 Java PDF 仅遍历 session.getAnswers()（已作答）。
package pdfexport

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/wuzuxian/interview/internal/apperr"
)

// InterviewReportInput 面试报告 PDF 视图模型，由 interview 模块从 DB 数据组装。
type InterviewReportInput struct {
	SessionID       string
	TotalQuestions  int
	StatusText      string // 已创建/进行中/已完成/已评估（Java getStatusText 逐字）
	CreatedAt       time.Time
	CompletedAt     *time.Time
	OverallScore    *int    // nil → 跳过"综合评分"节（Java 逐字）
	OverallFeedback string  // 空 → 跳过"总体评价"节
	Strengths       []string // 空 → 跳过"表现优势"节
	Improvements    []string // 空 → 跳过"改进建议"节
	CategoryScores  []InterviewCategoryScore
	Questions       []InterviewQuestionBlock
}

// InterviewCategoryScore 分类得分。
type InterviewCategoryScore struct {
	Category      string
	Score         int
	QuestionCount int
}

// InterviewQuestionBlock 问答详情单题。
type InterviewQuestionBlock struct {
	Number          int    // 题号（questionIndex+1）
	Category        string // 空 → "综合"
	IsFollowUp      bool   // 追问标记
	Question        string
	UserAnswer      *string  // nil → "未回答"
	Score           int
	Feedback        string   // 空 → 跳过"评价:"
	ReferenceAnswer string   // 空 → 跳过"参考答案:"
	KeyPoints       []string // 要点
}

// ===== 章节组装（纯函数，可测章节文案与空值防御）=====

// contentLine 文档中的一行，携带渲染样式与可选书签。
type contentLine struct {
	Text          string
	Size          float64
	Bold          bool
	Italic        bool
	Color         fpdf.RGBType
	BookmarkLevel int // -1 无书签；0/1 书签层级
	BookmarkTitle string
}

// contentSection 一个章节（标题 + 行）。
type contentSection struct {
	Title         string
	Lines         []contentLine
	BookmarkLevel int // -1 无；0 顶层书签
	BookmarkTitle string
}

// interviewContent 纯函数产物：文档标题 + 有序章节。
type interviewContent struct {
	Title    string
	Sections []contentSection
}

const (
	docTitleText   = "模拟面试报告"
	sectionInfo    = "面试信息"
	sectionScore   = "综合评分"
	sectionCats    = "分类得分"
	sectionOverall = "总体评价"
	sectionPros    = "表现优势"
	sectionCons    = "改进建议"
	sectionQA      = "问答详情"
)

var colorBody = rgb(51, 51, 51)

// buildInterviewContent 数据 → 文档结构（纯函数）。
func buildInterviewContent(in InterviewReportInput) interviewContent {
	content := interviewContent{Title: docTitleText}

	// 面试信息（Java 逐字字段：会话ID/题目数量/面试状态/开始时间/完成时间）。
	info := contentSection{Title: sectionInfo, BookmarkLevel: -1}
	info.Lines = []contentLine{
		textLine("会话ID: " + in.SessionID),
		textLine(fmt.Sprintf("题目数量: %d", in.TotalQuestions)),
		textLine("面试状态: " + in.StatusText),
		textLine("开始时间: " + formatInterviewTime(in.CreatedAt)),
	}
	if in.CompletedAt != nil {
		info.Lines = append(info.Lines, textLine("完成时间: "+formatInterviewTime(*in.CompletedAt)))
	}
	content.Sections = append(content.Sections, info)

	// 综合评分（overallScore 非空才渲染，Java 逐字）。
	if in.OverallScore != nil {
		score := *in.OverallScore
		sec := contentSection{Title: sectionScore, BookmarkLevel: -1}
		sec.Lines = []contentLine{{
			Text: fmt.Sprintf("总分: %d / 100", score), Size: 18, Bold: true, Color: scoreColor(score),
		}}
		content.Sections = append(content.Sections, sec)
	}

	// 分类得分（票面验收补充；与"综合评分"同 gate，仅已评估时有意义）。
	if in.OverallScore != nil && len(in.CategoryScores) > 0 {
		sec := contentSection{Title: sectionCats, BookmarkLevel: -1}
		for _, cs := range in.CategoryScores {
			sec.Lines = append(sec.Lines, bulletLine(
				fmt.Sprintf("%s: %d 分（%d 题）", cs.Category, cs.Score, cs.QuestionCount)))
		}
		content.Sections = append(content.Sections, sec)
	}

	// 总体评价。
	if strings.TrimSpace(in.OverallFeedback) != "" {
		sec := contentSection{Title: sectionOverall, BookmarkLevel: -1}
		sec.Lines = multiTextLines(sanitize(in.OverallFeedback))
		content.Sections = append(content.Sections, sec)
	}

	// 表现优势。
	if len(in.Strengths) > 0 {
		sec := contentSection{Title: sectionPros, BookmarkLevel: -1}
		for _, s := range in.Strengths {
			sec.Lines = append(sec.Lines, bulletLine("• "+sanitize(s)))
		}
		content.Sections = append(content.Sections, sec)
	}

	// 改进建议。
	if len(in.Improvements) > 0 {
		sec := contentSection{Title: sectionCons, BookmarkLevel: -1}
		for _, s := range in.Improvements {
			sec.Lines = append(sec.Lines, bulletLine("• "+sanitize(s)))
		}
		content.Sections = append(content.Sections, sec)
	}

	// 问答详情（带书签树：顶层"问答详情"→子"问题 N：题干"）。
	if len(in.Questions) > 0 {
		sec := contentSection{Title: sectionQA, BookmarkLevel: 0, BookmarkTitle: sectionQA}
		for _, q := range in.Questions {
			header := fmt.Sprintf("问题 %d [%s]", q.Number, questionCategory(q.Category))
			if q.IsFollowUp {
				header += "（追问）"
			}
			sec.Lines = append(sec.Lines, contentLine{
				Text: header, Size: 12, Bold: true,
				BookmarkLevel: 1, BookmarkTitle: questionBookmarkTitle(q),
			})
			sec.Lines = append(sec.Lines, textLine("Q: "+sanitize(q.Question)))

			answer := "未回答"
			if q.UserAnswer != nil {
				answer = sanitize(*q.UserAnswer)
			}
			sec.Lines = append(sec.Lines, textLine("A: "+answer))

			sec.Lines = append(sec.Lines, contentLine{
				Text: fmt.Sprintf("得分: %d/100", q.Score), Size: 11, Color: scoreColor(q.Score),
			})
			if strings.TrimSpace(q.Feedback) != "" {
				sec.Lines = append(sec.Lines, contentLine{
					Text: "评价: " + sanitize(q.Feedback), Size: 11, Italic: true, Color: colorBody,
				})
			}
			if strings.TrimSpace(q.ReferenceAnswer) != "" {
				sec.Lines = append(sec.Lines, contentLine{
					Text: "参考答案: " + sanitize(q.ReferenceAnswer), Size: 11, Color: colorGreen,
				})
			}
			if len(q.KeyPoints) > 0 {
				points := make([]string, 0, len(q.KeyPoints))
				for _, p := range q.KeyPoints {
					points = append(points, sanitize(p))
				}
				sec.Lines = append(sec.Lines, contentLine{
					Text: "要点: " + strings.Join(points, "；"), Size: 11, Color: colorGreen,
				})
			}
		}
		content.Sections = append(content.Sections, sec)
	}

	return content
}

// questionCategory 分类为空 → "综合"（Java 逐字）。
func questionCategory(category string) string {
	if strings.TrimSpace(category) == "" {
		return "综合"
	}
	return category
}

// questionBookmarkTitle 书签标题（笔记 07 §3.3 逐字）：题干空白折叠，空题干只留题号。
func questionBookmarkTitle(q InterviewQuestionBlock) string {
	number := fmt.Sprintf("问题 %d", q.Number)
	question := strings.Join(strings.Fields(q.Question), " ")
	if question == "" {
		return number
	}
	return number + "：" + question
}

// formatInterviewTime 时间格式 yyyy-MM-dd HH:mm:ss；零值 → "未知"（Java null→"未知" 口径）。
func formatInterviewTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.Format("2006-01-02 15:04:05")
}

func textLine(text string) contentLine {
	return contentLine{Text: text, Size: 11, Color: colorBody}
}

func bulletLine(text string) contentLine {
	return contentLine{Text: text, Size: 11, Color: colorBody}
}

// multiTextLines 多段文本按换行拆行（保留段落，空行 → 空行占位）。
func multiTextLines(text string) []contentLine {
	lines := []contentLine{}
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, textLine(line))
	}
	return lines
}

// ===== 渲染 =====

// ExportInterviewReport 渲染面试报告 PDF（含书签树），返回 PDF 字节。
func ExportInterviewReport(in InterviewReportInput) ([]byte, error) {
	pdf, err := newDoc()
	if err != nil {
		return nil, err
	}
	renderInterviewContent(pdf, buildInterviewContent(in))

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, apperr.Wrap(apperr.CodeExportPDFFailed, "输出面试报告 PDF 失败", err)
	}
	return buf.Bytes(), nil
}

func renderInterviewContent(pdf *fpdf.Fpdf, content interviewContent) {
	// 文档标题：24pt 粗体居中 #2980B9。
	pdf.SetFont(fontCJKName, "B", 24)
	pdf.SetTextColor(colorHeader.R, colorHeader.G, colorHeader.B)
	pdf.CellFormat(0, 14, content.Title, "", 1, "C", false, 0, "")

	for _, sec := range content.Sections {
		pdf.Ln(6)
		if sec.BookmarkLevel >= 0 && sec.BookmarkTitle != "" {
			pdf.Bookmark(sec.BookmarkTitle, sec.BookmarkLevel, -1)
		}
		pdf.SetFont(fontCJKName, "B", 14)
		pdf.SetTextColor(colorSection.R, colorSection.G, colorSection.B)
		pdf.CellFormat(0, 9, sec.Title, "", 1, "L", false, 0, "")

		for _, ln := range sec.Lines {
			if ln.BookmarkLevel >= 0 && ln.BookmarkTitle != "" {
				pdf.Bookmark(ln.BookmarkTitle, ln.BookmarkLevel, -1)
			}
			size := ln.Size
			if size <= 0 {
				size = 11 // 防御：行高随字号，0 字号会致行距塌陷、文字重叠
			}
			pdf.SetFont(fontCJKName, fontStyle(ln.Bold, ln.Italic), size)
			pdf.SetTextColor(ln.Color.R, ln.Color.G, ln.Color.B)
			pdf.MultiCell(0, size*0.55+1, ln.Text, "", "L", false)
		}
	}
}

func fontStyle(bold, italic bool) string {
	switch {
	case bold && italic:
		return "BI"
	case bold:
		return "B"
	case italic:
		return "I"
	}
	return ""
}
