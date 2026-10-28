package pdfexport

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func intPtr(i int) *int { return &i }

// fullInput 构造一份数据齐全的输入。
func fullInput() InterviewReportInput {
	completed := time.Date(2026, 10, 28, 10, 30, 0, 0, time.UTC)
	return InterviewReportInput{
		SessionID:       "s1",
		TotalQuestions:  2,
		StatusText:      "已评估",
		CreatedAt:       time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
		CompletedAt:     &completed,
		OverallScore:    intPtr(75),
		OverallFeedback: "整体不错",
		Strengths:       []string{"基础扎实"},
		Improvements:    []string{"多练项目"},
		CategoryScores:  []InterviewCategoryScore{{Category: "Java", Score: 80, QuestionCount: 1}, {Category: "网络", Score: 70, QuestionCount: 1}},
		Questions: []InterviewQuestionBlock{
			{Number: 1, Category: "Java", Question: "题一", UserAnswer: strPtr("答一"), Score: 80,
				Feedback: "不错", ReferenceAnswer: "参考一", KeyPoints: []string{"要点A", "要点B"}},
			{Number: 2, Category: "", Question: "题二", UserAnswer: nil, Score: 0},
		},
	}
}

func sectionTitles(c interviewContent) []string {
	out := make([]string, 0, len(c.Sections))
	for _, s := range c.Sections {
		out = append(out, s.Title)
	}
	return out
}

func hasLine(c interviewContent, substr string) bool {
	for _, s := range c.Sections {
		for _, ln := range s.Lines {
			if strings.Contains(ln.Text, substr) {
				return true
			}
		}
	}
	return false
}

// 章节顺序与标题文案对齐笔记 07（标题→面试信息→综合评分→总体评价→表现优势→改进建议→问答详情），
// 另含票面验收补的"分类得分"节。
func TestBuildInterviewContentSectionOrder(t *testing.T) {
	c := buildInterviewContent(fullInput())
	if c.Title != "模拟面试报告" {
		t.Fatalf("文档标题不符: %q", c.Title)
	}
	want := []string{"面试信息", "综合评分", "分类得分", "总体评价", "表现优势", "改进建议", "问答详情"}
	got := sectionTitles(c)
	if len(got) != len(want) {
		t.Fatalf("章节数不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("章节顺序不符: got %v want %v", got, want)
		}
	}
}

// 面试信息字段口径（Java 逐字）：会话ID/题目数量/面试状态/开始时间/完成时间。
func TestBuildInterviewInfoFields(t *testing.T) {
	c := buildInterviewContent(fullInput())
	for _, want := range []string{
		"会话ID: s1", "题目数量: 2", "面试状态: 已评估",
		"开始时间: 2026-10-28 09:00:00", "完成时间: 2026-10-28 10:30:00",
	} {
		if !hasLine(c, want) {
			t.Fatalf("面试信息缺字段: %q", want)
		}
	}
}

// 分数文案与字段口径：总分 " / 100"、单题 "得分: X/100"、未回答 "未回答"、分类空 → "综合"。
func TestBuildInterviewScoreAndAnswerCaliber(t *testing.T) {
	c := buildInterviewContent(fullInput())
	for _, want := range []string{
		"总分: 75 / 100", "得分: 80/100", "得分: 0/100",
		"A: 答一", "A: 未回答", "问题 2 [综合]", "问题 1 [Java]",
	} {
		if !hasLine(c, want) {
			t.Fatalf("缺文案: %q", want)
		}
	}
}

// 空值防御：缺各可选数据时节/行合理缺省，不崩溃。
func TestBuildInterviewEmptyValueDefense(t *testing.T) {
	// 全空输入：仅"面试信息"节。
	empty := InterviewReportInput{SessionID: "s0", StatusText: "进行中", CreatedAt: time.Now()}
	c := buildInterviewContent(empty)
	titles := sectionTitles(c)
	if len(titles) != 1 || titles[0] != "面试信息" {
		t.Fatalf("空输入应仅面试信息: %v", titles)
	}

	// 无总分 → 无综合评分/分类得分。
	noScore := fullInput()
	noScore.OverallScore = nil
	c = buildInterviewContent(noScore)
	for _, title := range sectionTitles(c) {
		if title == "综合评分" || title == "分类得分" {
			t.Fatalf("无总分不应有 %q 节", title)
		}
	}

	// 空优势/改进 → 对应节省略。
	noList := fullInput()
	noList.Strengths = nil
	noList.Improvements = nil
	noList.OverallFeedback = ""
	c = buildInterviewContent(noList)
	for _, title := range sectionTitles(c) {
		if title == "表现优势" || title == "改进建议" || title == "总体评价" {
			t.Fatalf("空列表不应有 %q 节", title)
		}
	}

	// 空问答 → 无问答详情节。
	noQ := fullInput()
	noQ.Questions = nil
	c = buildInterviewContent(noQ)
	for _, title := range sectionTitles(c) {
		if title == "问答详情" {
			t.Fatalf("空问答不应有问答详情节")
		}
	}

	// 单题全空：feedback/referenceAnswer/keyPoints 空 → 对应行缺省，但仍有 Q/A/得分。
	qEmpty := fullInput()
	qEmpty.Questions = []InterviewQuestionBlock{{Number: 1, Question: "题", UserAnswer: nil, Score: 0}}
	c = buildInterviewContent(qEmpty)
	if !hasLine(c, "A: 未回答") {
		t.Fatalf("空作答应显示未回答")
	}
	for _, absent := range []string{"评价: ", "参考答案: ", "要点: "} {
		if hasLine(c, absent) {
			t.Fatalf("空字段不应渲染 %q", absent)
		}
	}
}

// 书签树（裁决 #10）：顶层"问答详情"→子"问题 N：题干"，题干空白折叠。
func TestBuildInterviewBookmarks(t *testing.T) {
	in := fullInput()
	in.Questions = append(in.Questions, InterviewQuestionBlock{
		Number: 3, Question: "  多  空白 题  ", UserAnswer: strPtr("a"), Score: 1,
	})
	c := buildInterviewContent(in)

	qa := c.Sections[len(c.Sections)-1]
	if qa.BookmarkLevel != 0 || qa.BookmarkTitle != "问答详情" {
		t.Fatalf("问答详情书签不符: %+v", qa)
	}
	if qa.Lines[0].BookmarkLevel != 1 || qa.Lines[0].BookmarkTitle != "问题 1：题一" {
		t.Fatalf("问题书签不符: %+v", qa.Lines[0])
	}
	// 空白折叠：多空格 → 单空格。
	found := false
	for _, ln := range qa.Lines {
		if ln.BookmarkTitle == "问题 3：多 空白 题" {
			found = true
		}
	}
	if !found {
		t.Fatalf("题干空白折叠书签未找到，实际书签: %+v", qa.Lines)
	}
}

// PDF smoke：非空、%PDF 魔数、含页对象（页数>0）；空输入亦不崩溃。
func TestExportInterviewReportSmoke(t *testing.T) {
	for name, in := range map[string]InterviewReportInput{
		"full":  fullInput(),
		"empty": {SessionID: "s0"},
	} {
		pdf, err := ExportInterviewReport(in)
		if err != nil {
			t.Fatalf("[%s] 导出失败: %v", name, err)
		}
		if len(pdf) == 0 {
			t.Fatalf("[%s] PDF 为空", name)
		}
		if !bytes.HasPrefix(pdf, []byte("%PDF")) {
			t.Fatalf("[%s] 缺 %%PDF 魔数", name)
		}
		if !bytes.Contains(pdf, []byte("/Type /Page")) {
			t.Fatalf("[%s] 无页对象", name)
		}
	}
}
