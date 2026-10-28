// 批次评估引擎单测（票 05）：spec 测试决策——注入 stub ChatCompleter 按调用次序
// 返回脚本化 LLM 响应，只断言外部行为（报告内容与 LLM 收到的 prompt），
// 不 mock 内部步骤。覆盖：装批成组、合并三条对齐路径、二分恢复与预算耗尽降级、
// 修复重试提示注入、二次汇总降级与占位符组装、总分/分类分边界、截断文案。
package interview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/wuzuxian/interview/internal/ai"
)

// ===== 测试替身 =====

// evalStubCall 记录一次 LLM 调用收到的 [system, user]。
type evalStubCall struct {
	system string
	user   string
}

// evalStubCompleter 脚本化 ChatCompleter：respond 按调用次序（1-based）决定响应。
type evalStubCompleter struct {
	respond func(call int, system, user string) (string, error)
	calls   []evalStubCall
}

func (s *evalStubCompleter) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	system, user := input[0].Content, input[1].Content
	s.calls = append(s.calls, evalStubCall{system: system, user: user})
	if s.respond == nil {
		return nil, errors.New("stub 未配置响应脚本")
	}
	content, err := s.respond(len(s.calls), system, user)
	if err != nil {
		return nil, err
	}
	return &schema.Message{Content: content}, nil
}

func newTestEngine() *EvaluationEngine {
	return NewEvaluationEngine(DefaultEvaluationConfig(), nil)
}

func newTestInvoker() *ai.StructuredInvoker {
	return ai.NewStructuredInvoker(ai.DefaultStructuredConfig(), nil)
}

// ===== 数据与脚本构造辅助 =====

func answered(s string) *string { return &s }

func mainQuestion(idx int, question, category string, answer *string) QuestionDTO {
	return QuestionDTO{QuestionIndex: idx, Question: question, Category: &category, UserAnswer: answer}
}

func followQuestion(idx int, question, category string, answer *string, parent int) QuestionDTO {
	return QuestionDTO{QuestionIndex: idx, Question: question, Category: &category,
		UserAnswer: answer, IsFollowUp: true, ParentQuestionIndex: &parent}
}

// qaIndexPattern 从 batch user prompt 提取本批题号（buildQARecordsText 的固定格式）。
var qaIndexPattern = regexp.MustCompile(`问题 questionIndex=(\d+) \[`)

func promptIndexes(user string) []int {
	matches := qaIndexPattern.FindAllStringSubmatch(user, -1)
	indexes := make([]int, 0, len(matches))
	for _, m := range matches {
		n, _ := strconv.Atoi(m[1])
		indexes = append(indexes, n)
	}
	return indexes
}

// isSummaryCall 二次汇总调用的 user prompt 含"类别得分概览"段，批次调用含"面试问答记录"段。
func isSummaryCall(user string) bool {
	return strings.Contains(user, "类别得分概览")
}

// batchReportJSONFor 构造批次评估合法 JSON：对给定题号逐题给出分数/反馈/参考答案。
func batchReportJSONFor(indexes []int, scoreOf func(int) int) string {
	evals := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		evals = append(evals, map[string]any{
			"questionIndex":   idx,
			"score":           scoreOf(idx),
			"feedback":        fmt.Sprintf("评语-%d", idx),
			"referenceAnswer": fmt.Sprintf("参考-%d", idx),
			"keyPoints":       []string{fmt.Sprintf("要点-%d", idx)},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"overallScore":        80,
		"overallFeedback":     "批次评语",
		"strengths":           []string{"优势A"},
		"improvements":        []string{"改进B"},
		"questionEvaluations": evals,
	})
	return string(b)
}

func summaryReportJSON(feedback string, strengths, improvements []string) string {
	b, _ := json.Marshal(map[string]any{
		"overallFeedback": feedback,
		"strengths":       strengths,
		"improvements":    improvements,
	})
	return string(b)
}

// respondSummaryOK 汇总调用返回固定汇总；批次调用交给 respondBatch。
func respondSummaryOK(respondBatch func(call int, user string) (string, error)) func(call int, system, user string) (string, error) {
	return func(call int, system, user string) (string, error) {
		if isSummaryCall(user) {
			return summaryReportJSON("汇总评语", []string{"汇总优势"}, []string{"汇总改进"}), nil
		}
		return respondBatch(call, user)
	}
}

// ===== Prompt 资源逐字锚点 =====

// 契约：4 个评估模板必须保持 Java 原始字节拷贝；用独特片段锚定防止被重写/重排。
func TestEvaluationPromptResourcesEmbedVerbatim(t *testing.T) {
	anchors := []struct {
		name string
		text string
		want []string
	}{
		{"评估system", evalSystemPrompt, []string{
			"# Role", "无效回答必须给 0 分", "追问评分规则", "questionIndex 必须原样返回",
			"该题分数必须为 0"}},
		{"评估user", evalUserPrompt, []string{
			"{resumeText}", "{qaRecords}", "{referenceContext}",
			"原样复制，不要自行加减 1 或重新编号", "参考答案基线只用于校准深度与完整性"}},
		{"汇总system", evalSummarySystemPrompt, []string{
			"分批评估结果", "strengths 与 improvements 各输出 3-6 条"}},
		{"汇总user", evalSummaryUserPrompt, []string{
			"{resumeText}", "{referenceContext}", "{categorySummary}", "{questionHighlights}",
			"{fallbackOverallFeedback}", "{fallbackStrengths}", "{fallbackImprovements}"}},
	}
	for _, a := range anchors {
		for _, frag := range a.want {
			if !strings.Contains(a.text, frag) {
				t.Errorf("%s 缺少逐字片段 %q", a.name, frag)
			}
		}
	}
}

// ===== buildQARecordsText 拼接格式（逐字）=====

func TestBuildQARecordsTextFormat(t *testing.T) {
	records := []qaRecord{
		{questionIndex: 0, question: "题干", category: "MYSQL", userAnswer: answered("我知道")},
		{questionIndex: 1, question: "追问题", category: "MYSQL（追问1）",
			userAnswer: nil, followUp: true, parentQuestionIndex: intPtr(0)},
	}
	got := buildQARecordsText(records)
	want := "问题 questionIndex=0 [MYSQL]: 题干\n回答: 我知道\n\n" +
		"问题 questionIndex=1 [MYSQL（追问1）]（追问，针对 questionIndex=0）: 追问题\n回答: (未回答)\n\n"
	if got != want {
		t.Errorf("拼接格式不符\nwant: %q\ngot:  %q", want, got)
	}
}

// ===== 装批：成组与批边界（表驱动）=====

// 契约：主问题与其追问成组不可拆散；追问父索引异常（父索引不存在、不小于自身、
// 未指向已处理的主问题）降级为独立组；批按组切分，放入下一组会超限即切批。
func TestBuildGroupsAndPackBatches(t *testing.T) {
	r := func(idx int, followUp bool, parent *int) qaRecord {
		return qaRecord{questionIndex: idx, followUp: followUp, parentQuestionIndex: parent}
	}
	p := intPtr

	cases := []struct {
		name        string
		records     []qaRecord
		batchSize   int
		wantGroups  [][]int // 每组的题号
		wantBatches [][]int // 每批的题号（组拼平后）
	}{
		{
			name:        "主问题与追问成组不拆散",
			records:     []qaRecord{r(0, false, nil), r(1, true, p(0)), r(2, false, nil), r(3, true, p(2))},
			batchSize:   8,
			wantGroups:  [][]int{{0, 1}, {2, 3}},
			wantBatches: [][]int{{0, 1, 2, 3}},
		},
		{
			name:        "组不可拆散溢出即切批",
			records:     []qaRecord{r(0, false, nil), r(1, true, p(0)), r(2, true, p(0))}, // 组大小 3（主+2追问）
			batchSize:   2,
			wantGroups:  [][]int{{0, 1, 2}},
			wantBatches: [][]int{{0, 1, 2}}, // 组大于批大小也整组放入，绝不拆
		},
		{
			name:        "恰好装满不切批",
			records:     []qaRecord{r(0, false, nil), r(1, true, p(0))},
			batchSize:   2,
			wantGroups:  [][]int{{0, 1}},
			wantBatches: [][]int{{0, 1}},
		},
		{
			name: "多组按序装批",
			records: []qaRecord{
				r(0, false, nil), r(1, true, p(0)),
				r(2, false, nil), r(3, true, p(2)),
				r(4, false, nil),
			},
			batchSize:   2,
			wantGroups:  [][]int{{0, 1}, {2, 3}, {4}},
			wantBatches: [][]int{{0, 1}, {2, 3}, {4}},
		},
		{
			name:        "追问父索引不存在降级独立组",
			records:     []qaRecord{r(0, false, nil), r(1, true, p(9))},
			batchSize:   8,
			wantGroups:  [][]int{{0}, {1}},
			wantBatches: [][]int{{0, 1}},
		},
		{
			name:        "追问父索引不小于自身降级独立组",
			records:     []qaRecord{r(0, false, nil), r(1, true, p(1))},
			batchSize:   8,
			wantGroups:  [][]int{{0}, {1}},
			wantBatches: [][]int{{0, 1}},
		},
		{
			name:        "追问无父索引降级独立组",
			records:     []qaRecord{r(0, false, nil), r(1, true, nil)},
			batchSize:   8,
			wantGroups:  [][]int{{0}, {1}},
			wantBatches: [][]int{{0, 1}},
		},
		{
			name:        "父索引指向未处理的记录视为无主可挂",
			records:     []qaRecord{r(1, true, p(0)), r(0, false, nil)},
			batchSize:   8,
			wantGroups:  [][]int{{1}, {0}},
			wantBatches: [][]int{{1, 0}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := buildGroups(tc.records, nil)
			if len(groups) != len(tc.wantGroups) {
				t.Fatalf("组数 want %d got %d", len(tc.wantGroups), len(groups))
			}
			for i, g := range groups {
				if got := recordIndexes(g.records); !equalInts(got, tc.wantGroups[i]) {
					t.Errorf("组%d 题号 want %v got %v", i, tc.wantGroups[i], got)
				}
			}
			batches := packBatches(groups, tc.batchSize)
			if len(batches) != len(tc.wantBatches) {
				t.Fatalf("批数 want %d got %d", len(tc.wantBatches), len(batches))
			}
			for i, b := range batches {
				if got := recordIndexes(groupRecords(b)); !equalInts(got, tc.wantBatches[i]) {
					t.Errorf("批%d 题号 want %v got %v", i, tc.wantBatches[i], got)
				}
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ===== 合并对齐：三条路径（表驱动）=====

// 契约：按 questionIndex 映射；整批一致 1-based 偏移按位置修正并改写题号；
// 全非法（无任何合法题号且数量一致）按位置对齐；缺失位置补 0 分默认文案。
func TestMergeQuestionEvaluations(t *testing.T) {
	mkEval := func(idx, score int) *questionEvalDTO {
		return &questionEvalDTO{QuestionIndex: idx, Score: score,
			Feedback: stringPtr(fmt.Sprintf("评语-%d", idx)), ReferenceAnswer: stringPtr(""), KeyPoints: []string{}}
	}
	mergeMissing := mergeMissingFeedback

	cases := []struct {
		name      string
		expected  []int
		returned  []int // LLM 返回的题号（nil 元素模拟 JSON null）
		nilReport bool  // 整批失败（report 为 nil）
		wantIdx   []int
		wantScore []int
		wantFeed  []string
	}{
		{
			name:      "正常映射原样保留",
			expected:  []int{0, 1, 2},
			returned:  []int{0, 1, 2},
			wantIdx:   []int{0, 1, 2},
			wantScore: []int{90, 80, 70},
			wantFeed:  []string{"评语-0", "评语-1", "评语-2"},
		},
		{
			name:      "整批一致1based偏移按位置修正",
			expected:  []int{3, 4, 5},
			returned:  []int{4, 5, 6},
			wantIdx:   []int{3, 4, 5},
			wantScore: []int{90, 80, 70}, // 对应位置 LLM 原值
			wantFeed:  []string{"评语-4", "评语-5", "评语-6"},
		},
		{
			name:      "全非法按位置对齐",
			expected:  []int{0, 1},
			returned:  []int{7, 8},
			wantIdx:   []int{0, 1},
			wantScore: []int{90, 80},
			wantFeed:  []string{"评语-7", "评语-8"},
		},
		{
			name:      "数量不符缺失位置补默认文案",
			expected:  []int{0, 1, 2},
			returned:  []int{0, 2},
			wantIdx:   []int{0, 1, 2},
			wantScore: []int{90, 0, 80},
			wantFeed:  []string{"评语-0", mergeMissing, "评语-2"},
		},
		{
			name:      "整批失败全部补默认文案",
			expected:  []int{0, 1},
			nilReport: true,
			wantIdx:   []int{0, 1},
			wantScore: []int{0, 0},
			wantFeed:  []string{mergeMissing, mergeMissing},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var result batchResult
			if tc.nilReport {
				result = batchResult{questionIndexes: tc.expected, report: nil}
			} else {
				evals := make([]*questionEvalDTO, 0, len(tc.returned))
				for i, idx := range tc.returned {
					if idx < 0 {
						evals = append(evals, nil)
						continue
					}
					// 分数脚本：第一个 90，其后每个递减 10
					evals = append(evals, mkEval(idx, 90-10*i))
				}
				result = batchResult{questionIndexes: tc.expected,
					report: &batchReportDTO{QuestionEvaluations: evals}}
			}
			merged := mergeQuestionEvaluations([]batchResult{result})
			if len(merged) != len(tc.wantIdx) {
				t.Fatalf("合并条数 want %d got %d", len(tc.wantIdx), len(merged))
			}
			for i, dto := range merged {
				if dto.QuestionIndex != tc.wantIdx[i] || dto.Score != tc.wantScore[i] ||
					*dto.Feedback != tc.wantFeed[i] {
					t.Errorf("第%d条 want (idx=%d score=%d fb=%q) got (idx=%d score=%d fb=%q)",
						i, tc.wantIdx[i], tc.wantScore[i], tc.wantFeed[i],
						dto.QuestionIndex, dto.Score, *dto.Feedback)
				}
			}
		})
	}
}

// ===== 引擎：正常多批 =====

// 契约：9 题（各独立成组）按 8 条/批切成 2 批，批内题号保持输入顺序；
// 汇总调用一次；报告总分 = 全体题目均分，分类得分按分类平均且稳定排序。
func TestEvaluateSessionMultiBatch(t *testing.T) {
	questions := make([]QuestionDTO, 0, 9)
	categories := []string{"MYSQL", "REDIS", "SPRING"}
	for i := 0; i < 9; i++ {
		questions = append(questions, mainQuestion(i, fmt.Sprintf("题目%d", i),
			categories[i%3], answered(fmt.Sprintf("回答%d", i))))
	}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		indexes := promptIndexes(user)
		if len(indexes) == 0 {
			return "", errors.New("批次 prompt 无题号")
		}
		return batchReportJSONFor(indexes, func(idx int) int { return 100 - idx }), nil
	})}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", ResumeText: "简历", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	// 2 批 + 1 次汇总
	if len(stub.calls) != 3 {
		t.Fatalf("LLM 调用次数 want 3 got %d", len(stub.calls))
	}
	if got := promptIndexes(stub.calls[0].user); !equalInts(got, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("批1题号 want 0..7 got %v", got)
	}
	if got := promptIndexes(stub.calls[1].user); !equalInts(got, []int{8}) {
		t.Errorf("批2题号 want [8] got %v", got)
	}

	if report.TotalQuestions != 9 {
		t.Errorf("totalQuestions want 9 got %d", report.TotalQuestions)
	}
	// 全体均分：avg(100..92) = 96
	if report.OverallScore != 96 {
		t.Errorf("overallScore want 96 got %d", report.OverallScore)
	}
	if report.OverallFeedback != "汇总评语" || len(report.Strengths) != 1 || report.Strengths[0] != "汇总优势" {
		t.Errorf("汇总结论未生效: %q %v", report.OverallFeedback, report.Strengths)
	}
	// 逐题分数与反馈
	for i, detail := range report.QuestionDetails {
		if detail.QuestionIndex != i || detail.Score != 100-i || detail.Feedback != fmt.Sprintf("评语-%d", i) {
			t.Errorf("题%d want (idx=%d score=%d) got (idx=%d score=%d fb=%q)",
				i, i, 100-i, detail.QuestionIndex, detail.Score, detail.Feedback)
		}
		if detail.UserAnswer == nil || *detail.UserAnswer != fmt.Sprintf("回答%d", i) {
			t.Errorf("题%d userAnswer 未原样保留", i)
		}
	}
	// 分类得分：按名排序 + 平均（int 截断）
	wantCat := []CategoryScoreDTO{
		{Category: "MYSQL", Score: 97, QuestionCount: 3},  // (100+97+94)/3
		{Category: "REDIS", Score: 96, QuestionCount: 3},  // (99+96+93)/3
		{Category: "SPRING", Score: 95, QuestionCount: 3}, // (98+95+92)/3
	}
	if !reflect.DeepEqual(report.CategoryScores, wantCat) {
		t.Errorf("categoryScores want %+v got %+v", wantCat, report.CategoryScores)
	}
	// 参考答案来自批次评估
	if len(report.ReferenceAnswers) != 9 || report.ReferenceAnswers[0].ReferenceAnswer != "参考-0" ||
		len(report.ReferenceAnswers[0].KeyPoints) != 1 || report.ReferenceAnswers[0].KeyPoints[0] != "要点-0" {
		t.Errorf("referenceAnswers 不符: %+v", report.ReferenceAnswers[0])
	}
}

// ===== 引擎：修复重试（坏 JSON → 好 JSON）=====

// 契约：首次输出解析失败后重试，第二次调用的 system 注入严格 JSON 指令、
// user 注入上次解析失败原因；最终结果来自第二次合法输出。
func TestEvaluateSessionRepairRetryInjectsRepairPrompt(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "题目零", "MYSQL", answered("回答零")),
		mainQuestion(1, "题目一", "MYSQL", answered("回答一")),
	}
	batchOK := batchReportJSONFor([]int{0, 1}, func(int) int { return 77 })
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		if call == 1 {
			return "这不是 JSON {{{", nil
		}
		return batchOK, nil
	})}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	if len(stub.calls) != 3 { // 批次坏 + 批次好 + 汇总
		t.Fatalf("LLM 调用次数 want 3 got %d", len(stub.calls))
	}
	first, second := stub.calls[0], stub.calls[1]
	if strings.Contains(first.system, "严格输出一个 JSON 对象") {
		t.Errorf("首次调用不应注入修复提示")
	}
	if !strings.Contains(second.system, "严格输出一个 JSON 对象") {
		t.Errorf("重试调用 system 未注入严格 JSON 指令")
	}
	if !strings.Contains(second.user, "上一次输出解析失败，请修复以下问题后重新仅输出 JSON：") {
		t.Errorf("重试调用 user 未注入上次失败原因")
	}
	// user 其余部分保持不变（数据段不因重试而丢失）
	if !strings.Contains(second.user, "---简历内容开始---") || !strings.Contains(second.user, "回答零") {
		t.Errorf("重试 user 丢失原始数据段")
	}
	if len(report.QuestionDetails) != 2 || report.QuestionDetails[0].Score != 77 ||
		report.QuestionDetails[1].Score != 77 {
		t.Errorf("重试后结果未生效: %+v", report.QuestionDetails)
	}
}

// ===== 引擎：二分恢复（批失败 → 按组拆半重试）=====

// 契约：整批失败后按组二分，兄弟半批各耗 1 次预算；半批成功则结果合并回原题。
func TestEvaluateSessionRecoverBatchSplitsByGroups(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "主零", "MYSQL", answered("答零")),
		followQuestion(1, "追零", "MYSQL（追问1）", answered("追答零"), 0),
		mainQuestion(2, "主一", "REDIS", answered("答一")),
		followQuestion(3, "追一", "REDIS（追问1）", nil, 2),
	}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		indexes := promptIndexes(user)
		if len(indexes) >= 4 {
			return "", errors.New("模拟整批模型失败")
		}
		return batchReportJSONFor(indexes, func(int) int { return 80 }), nil
	})}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	// 1 次整批失败 + 2 次半批成功 + 1 次汇总
	if len(stub.calls) != 4 {
		t.Fatalf("LLM 调用次数 want 4 got %d", len(stub.calls))
	} // 半批保持组完整、顺序不变：[0,1] 与 [2,3]
	if got := promptIndexes(stub.calls[1].user); !equalInts(got, []int{0, 1}) {
		t.Errorf("半批1题号 want [0 1] got %v", got)
	}
	if got := promptIndexes(stub.calls[2].user); !equalInts(got, []int{2, 3}) {
		t.Errorf("半批2题号 want [2 3] got %v", got)
	}
	// 追问与主问题同批且拼接格式保留
	if !strings.Contains(stub.calls[1].user, "（追问，针对 questionIndex=0）") {
		t.Errorf("半批 prompt 丢失追问标注")
	}
	if report.OverallScore != 60 {
		t.Errorf("overallScore want 60（未答追问按 0 分计入均分）got %d", report.OverallScore)
	}
	for i, detail := range report.QuestionDetails {
		want := 80
		if i == 3 {
			want = 0 // 未回答的追问按 0 分
		}
		if detail.Score != want {
			t.Errorf("题%d score want %d got %d", i, want, detail.Score)
		}
	}
}

// ===== 引擎：预算耗尽 → 显式降级 =====

// 契约：预算 6 次全部耗尽后仍失败的段显式降级 0 分 + 系统降级文案；
// 汇总也失败时综合评语降级为批次聚合默认文案。
func TestEvaluateSessionBudgetExhaustedDegrades(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "主零", "MYSQL", answered("答零")),
		followQuestion(1, "追零", "MYSQL（追问1）", answered("追答零"), 0),
		mainQuestion(2, "主一", "REDIS", answered("答一")),
		followQuestion(3, "追一", "REDIS（追问1）", answered("追答一"), 2),
	}
	stub := &evalStubCompleter{respond: func(call int, system, user string) (string, error) {
		return "", errors.New("模拟模型持续失败")
	}}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("降级路径不应返回错误: %v", err)
	}

	// 1 整批 + 2 半批 + 2 叶子整段加试 = 5 次评估调用（预算 6 未再耗第 6 次）+ 1 汇总失败
	if len(stub.calls) != 6 {
		t.Fatalf("LLM 调用次数 want 6 got %d", len(stub.calls))
	}
	if report.OverallScore != 0 {
		t.Errorf("降级后 overallScore want 0 got %d", report.OverallScore)
	}
	for i, detail := range report.QuestionDetails {
		if detail.Score != 0 || detail.Feedback != "模型评估失败后的系统降级，该分数不代表真实表现。" {
			t.Errorf("题%d 未按显式降级处理: score=%d fb=%q", i, detail.Score, detail.Feedback)
		}
	}
	// 合并层报告评语为空 → 批次聚合默认文案（汇总调用也失败，直接落到报告）
	if report.OverallFeedback != "本次面试已完成分批评估，但未生成有效综合评语。" {
		t.Errorf("overallFeedback want 批次聚合默认文案 got %q", report.OverallFeedback)
	}
	if report.Strengths == nil || len(report.Strengths) != 0 ||
		report.Improvements == nil || len(report.Improvements) != 0 {
		t.Errorf("降级后 strengths/improvements 应为空列表: %v %v", report.Strengths, report.Improvements)
	}
}

// ===== 引擎：叶子段加试耗尽预算的另一种降级文案来源 =====

// 契约：单组批失败时不可再拆，整段加试一次后仍失败 → degradedReport 自带评语
// "模型评估失败后的系统降级。"会成为批次聚合评语。
func TestEvaluateSessionLeafRetryFailureUsesDegradedFeedback(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "主零", "MYSQL", answered("答零")),
		followQuestion(1, "追零", "MYSQL（追问1）", answered("追答零"), 0),
	}
	stub := &evalStubCompleter{respond: func(call int, system, user string) (string, error) {
		return "", errors.New("模拟模型失败")
	}}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("降级路径不应返回错误: %v", err)
	}
	// 1 整批 + 1 叶子加试 + 1 汇总 = 3
	if len(stub.calls) != 3 {
		t.Fatalf("LLM 调用次数 want 3 got %d", len(stub.calls))
	}
	if report.OverallFeedback != "模型评估失败后的系统降级。" {
		t.Errorf("overallFeedback want 显式降级评语 got %q", report.OverallFeedback)
	}
	for i, detail := range report.QuestionDetails {
		if detail.Score != 0 || detail.Feedback != "模型评估失败后的系统降级，该分数不代表真实表现。" {
			t.Errorf("题%d 未按显式降级处理: %d %q", i, detail.Score, detail.Feedback)
		}
	}
}

// ===== 引擎：关闭二分恢复时部分批失败走合并默认文案 =====

// 契约：关闭二分恢复后整批失败不留降级产物，但只要还有批成功（部分成功），
// 仍正常出报告，缺失批的题走合并默认文案（裁决 #9 前者文案逐字）。
func TestEvaluateSessionWithoutSplitUsesMergeDefault(t *testing.T) {
	questions := make([]QuestionDTO, 0, 9)
	for i := 0; i < 9; i++ {
		questions = append(questions, mainQuestion(i, fmt.Sprintf("题目%d", i),
			"MYSQL", answered(fmt.Sprintf("回答%d", i))))
	}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		indexes := promptIndexes(user)
		if containsInt(indexes, 8) {
			return "", errors.New("模拟第二批次模型失败")
		}
		return batchReportJSONFor(indexes, func(int) int { return 60 }), nil
	})}
	cfg := DefaultEvaluationConfig()
	cfg.FallbackSplitEnabled = false
	engine := NewEvaluationEngine(cfg, nil)

	report, err := engine.EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("部分成功不应返回错误: %v", err)
	}
	// 批1成功 + 批2失败 + 汇总，共 3 次调用
	if len(stub.calls) != 3 {
		t.Fatalf("LLM 调用次数 want 3 got %d", len(stub.calls))
	}
	for i, detail := range report.QuestionDetails {
		if i == 8 {
			if detail.Score != 0 || detail.Feedback != "该题未成功生成评估结果，系统按 0 分处理。" {
				t.Errorf("题%d 应走合并默认文案: %d %q", i, detail.Score, detail.Feedback)
			}
			continue
		}
		if detail.Score != 60 || detail.Feedback != fmt.Sprintf("评语-%d", i) {
			t.Errorf("题%d 批1结果未生效: %d %q", i, detail.Score, detail.Feedback)
		}
	}
}

// ===== 引擎：全批失败上抛（主控仲裁，有意偏离 Java）=====

// 契约：成功的批数 == 0 且总题数 > 0（stub 全失败、连显式降级产物都没有）时
// 返回 error 而非全零报告；错误信息含可定位信息（sessionId/批数/题数）。
func TestEvaluateSessionAllBatchesFailedReturnsError(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "主零", "MYSQL", answered("答零")),
		mainQuestion(1, "主一", "REDIS", answered("答一")),
	}
	stub := &evalStubCompleter{respond: func(call int, system, user string) (string, error) {
		return "", errors.New("模拟模型持续失败")
	}}
	cfg := DefaultEvaluationConfig()
	cfg.FallbackSplitEnabled = false // 无降级产物：报告为 nil 的极端情况
	engine := NewEvaluationEngine(cfg, nil)

	report, err := engine.EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-err-1", Questions: questions})
	if err == nil {
		t.Fatal("全批失败应返回 error 而非全零报告")
	}
	if report != nil {
		t.Errorf("error 时不应返回报告: %+v", report)
	}
	for _, want := range []string{"sess-err-1", "全部 1 个评估批次均未产出逐题评估结果", "题数=2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少可定位信息 %q: %v", want, err)
		}
	}
	// 上抛前不再发起汇总调用
	if len(stub.calls) != 1 {
		t.Errorf("LLM 调用次数 want 1 got %d", len(stub.calls))
	}
}

// 契约：单批成功 + 单批降级（批恢复显式降级 0 分）= 部分成功，不报错，
// 报告含显式降级文案（裁决 #9 后者文案逐字）。
func TestEvaluateSessionPartialSuccessWithDegradedKeepsReport(t *testing.T) {
	questions := make([]QuestionDTO, 0, 9)
	for i := 0; i < 9; i++ {
		questions = append(questions, mainQuestion(i, fmt.Sprintf("题目%d", i),
			"MYSQL", answered(fmt.Sprintf("回答%d", i))))
	}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		indexes := promptIndexes(user)
		if containsInt(indexes, 8) {
			return "", errors.New("模拟第二批次模型失败")
		}
		return batchReportJSONFor(indexes, func(int) int { return 70 }), nil
	})}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("单批成功+单批降级不应报错: %v", err)
	}
	// 批1成功 + 批2失败 + 叶子加试失败 + 汇总，共 4 次调用
	if len(stub.calls) != 4 {
		t.Fatalf("LLM 调用次数 want 4 got %d", len(stub.calls))
	}
	if report.QuestionDetails[8].Score != 0 ||
		report.QuestionDetails[8].Feedback != "模型评估失败后的系统降级，该分数不代表真实表现。" {
		t.Errorf("题8 应为显式降级产物: %d %q",
			report.QuestionDetails[8].Score, report.QuestionDetails[8].Feedback)
	}
	if report.QuestionDetails[0].Feedback != "评语-0" {
		t.Errorf("题0 成功批结果未生效: %q", report.QuestionDetails[0].Feedback)
	}
}

// ===== 引擎：二次汇总失败降级为批次拼接 =====

func TestEvaluateSessionSummaryFailureFallsBackToBatchResults(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "题目零", "MYSQL", answered("回答零")),
		mainQuestion(1, "题目一", "REDIS", answered("回答一")),
	}
	stub := &evalStubCompleter{respond: func(call int, system, user string) (string, error) {
		if isSummaryCall(user) {
			return "", errors.New("模拟汇总失败")
		}
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 66 }), nil
	}}

	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("LLM 调用次数 want 2 got %d", len(stub.calls))
	}
	if report.OverallFeedback != "批次评语" {
		t.Errorf("汇总失败应回退批次评语 got %q", report.OverallFeedback)
	}
	if !reflect.DeepEqual(report.Strengths, []string{"优势A"}) ||
		!reflect.DeepEqual(report.Improvements, []string{"改进B"}) {
		t.Errorf("汇总失败应回退批次优劣项: %v %v", report.Strengths, report.Improvements)
	}
	if report.OverallScore != 66 {
		t.Errorf("overallScore want 66 got %d", report.OverallScore)
	}
}

// ===== 引擎：二次汇总占位符组装 =====

// 契约：汇总 user prompt 的 categorySummary 行格式、questionHighlights 行格式
// （Q 编号 1-based）、批次初始结论三段与简历/参考基线逐一注入；未回答题在
// 类别概览按 0 分计入，题目高亮仍展示模型给分。
func TestEvaluateSessionSummaryPromptPlaceholders(t *testing.T) {
	questions := []QuestionDTO{
		mainQuestion(0, "MySQL 索引题", "MYSQL", answered("答零")),
		mainQuestion(1, strings.Repeat("长", 60), "MYSQL", answered("答一")),
		mainQuestion(2, "Redis 题", "REDIS", nil),
	}
	stub := &evalStubCompleter{respond: func(call int, system, user string) (string, error) {
		if isSummaryCall(user) {
			return summaryReportJSON("汇总评语", []string{"汇总优势"}, []string{"汇总改进"}), nil
		}
		return batchReportJSONFor(promptIndexes(user), func(idx int) int {
			if idx == 0 {
				return 80
			}
			if idx == 1 {
				return 60
			}
			return 50
		}), nil
	}}

	_, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", ResumeText: "候选人简历", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("LLM 调用次数 want 2 got %d", len(stub.calls))
	}
	summaryUser := stub.calls[1].user

	// 类别概览：未回答题按 0 分计入均分与题数；按行字典序排序
	if !strings.Contains(summaryUser, "- MYSQL: 平均分 70, 题数 2") {
		t.Errorf("categorySummary 缺少 MYSQL 行: %s", summaryUser)
	}
	if !strings.Contains(summaryUser, "- REDIS: 平均分 0, 题数 1") {
		t.Errorf("categorySummary 缺少 REDIS 行（未答按 0 分计入）: %s", summaryUser)
	}
	// 题目高亮：Q 编号 1-based；60 字题干截到 50 字加省略号；未答题高亮仍展示模型给分
	if !strings.Contains(summaryUser, "- Q1 | MySQL 索引题 | 分数:80 | 反馈:评语-0") {
		t.Errorf("questionHighlights 第 1 行不符: %s", summaryUser)
	}
	if !strings.Contains(summaryUser, "- Q2 | "+strings.Repeat("长", 50)+"... | 分数:60 | 反馈:评语-1") {
		t.Errorf("questionHighlights 第 2 行（题干截断）不符: %s", summaryUser)
	}
	if !strings.Contains(summaryUser, "- Q3 | Redis 题 | 分数:50 | 反馈:评语-2") {
		t.Errorf("questionHighlights 第 3 行不符: %s", summaryUser)
	}
	// 初始结论三段
	if !strings.Contains(summaryUser, "批次评语") ||
		!strings.Contains(summaryUser, "优势A") || !strings.Contains(summaryUser, "改进B") {
		t.Errorf("汇总 prompt 缺少批次初始结论: %s", summaryUser)
	}
	// 占位符全部被替换
	for _, ph := range []string{"{resumeText}", "{categorySummary}", "{questionHighlights}",
		"{fallbackOverallFeedback}", "{fallbackStrengths}", "{fallbackImprovements}"} {
		if strings.Contains(summaryUser, ph) {
			t.Errorf("汇总 prompt 残留占位符 %s", ph)
		}
	}
	if !strings.Contains(summaryUser, "候选人简历") {
		t.Errorf("汇总 prompt 未注入简历")
	}
	// 批次 prompt 同样注入简历与问答记录，且不残留占位符
	batchUser := stub.calls[0].user
	if !strings.Contains(batchUser, "问题 questionIndex=0 [MYSQL]: MySQL 索引题") ||
		!strings.Contains(batchUser, "回答: 答零") ||
		!strings.Contains(batchUser, "问题 questionIndex=2 [REDIS]: Redis 题") ||
		!strings.Contains(batchUser, "回答: (未回答)") {
		t.Errorf("批次 prompt 问答记录格式不符: %s", batchUser)
	}
	if strings.Contains(batchUser, "{resumeText}") || strings.Contains(batchUser, "{qaRecords}") {
		t.Errorf("批次 prompt 残留占位符")
	}
	if !strings.Contains(stub.calls[0].system, "# 安全边界") {
		t.Errorf("批次 system 未追加防注入段")
	}
}

// ===== 引擎：空白参考基线渲染为"无" =====

func TestEvaluateSessionBlankReferenceRenderedAsNone(t *testing.T) {
	questions := []QuestionDTO{mainQuestion(0, "题目零", "MYSQL", answered("回答零"))}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 70 }), nil
	})}
	_, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if !strings.Contains(stub.calls[0].user, "参考答案基线（用于辅助评估，不是唯一标准）\n无") {
		t.Errorf("批次 prompt 参考基线应渲染为无: %s", stub.calls[0].user)
	}
	if !strings.Contains(stub.calls[1].user, "参考答案基线（用于辅助汇总）\n无") {
		t.Errorf("汇总 prompt 参考基线应渲染为无")
	}
}

// ===== 引擎：截断提示文案（逐字）=====

// 契约：简历超 3000 字截断并追加 "\n...(简历内容过长，已截断)"；
// 参考基线超 6000 字截断并追加 "\n...(参考基线过长，已截断)"；超出部分不得出现在 prompt。
func TestEvaluateSessionTruncationNotices(t *testing.T) {
	questions := []QuestionDTO{mainQuestion(0, "题目零", "MYSQL", answered("回答零"))}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 70 }), nil
	})}
	input := EvaluateInput{
		SessionID:      "sess-1",
		ResumeText:     strings.Repeat("a", 3000) + "RESUME-TAIL",
		Questions:      questions,
		SkillReference: strings.Repeat("r", 6000) + "REF-TAIL",
	}
	_, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(), input)
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	batchUser := stub.calls[0].user
	if !strings.Contains(batchUser, strings.Repeat("a", 3000)+resumeTruncatedSuffix) {
		t.Errorf("简历截断文案缺失")
	}
	if strings.Contains(batchUser, "RESUME-TAIL") {
		t.Errorf("简历超出部分未截断")
	}
	if !strings.Contains(batchUser, strings.Repeat("r", 6000)+referenceTruncatedSuffix) {
		t.Errorf("参考基线截断文案缺失")
	}
	if strings.Contains(batchUser, "REF-TAIL") {
		t.Errorf("参考基线超出部分未截断")
	}
}

// ===== 引擎：题目自带参考答案覆盖报告参考答案 =====

func TestWithQuestionReferencesOverrides(t *testing.T) {
	questionRef := "题目自带参考"
	questions := []QuestionDTO{
		{QuestionIndex: 0, Question: "题目零", Category: stringPtr("MYSQL"), UserAnswer: answered("回答零"),
			ReferenceAnswer: &questionRef, KeyPoints: []string{"要点一"}},
		mainQuestion(1, "题目一", "MYSQL", answered("回答一")),
	}
	stub := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 70 }), nil
	})}
	report, err := newTestEngine().EvaluateSession(context.Background(), stub, newTestInvoker(),
		EvaluateInput{SessionID: "sess-1", Questions: questions})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if report.ReferenceAnswers[0].ReferenceAnswer != questionRef ||
		!reflect.DeepEqual(report.ReferenceAnswers[0].KeyPoints, []string{"要点一"}) {
		t.Errorf("题目自带参考答案应覆盖: %+v", report.ReferenceAnswers[0])
	}
	if report.ReferenceAnswers[1].ReferenceAnswer != "参考-1" {
		t.Errorf("无自带参考的题应保留模型参考答案: %+v", report.ReferenceAnswers[1])
	}
}

// ===== buildReport 总分与分类分边界 =====

// 契约（裁决 8）：overallScore = 全部题目（含追问、未答 0 分）简单平均，全部未答为 0；
// 分类得分按分类平均（int 截断）；反馈缺失回退默认文案。
func TestBuildReportBoundaries(t *testing.T) {
	eval := func(idx, score int) *questionEvalDTO {
		return &questionEvalDTO{QuestionIndex: idx, Score: score,
			Feedback: stringPtr("反馈"), ReferenceAnswer: stringPtr(""), KeyPoints: []string{}}
	}
	cases := []struct {
		name           string
		records        []qaRecord
		evaluations    []*questionEvalDTO
		wantOverall    int
		wantCategories []CategoryScoreDTO
	}{
		{
			name: "全部未答总分为0",
			records: []qaRecord{
				{questionIndex: 0, category: "MYSQL"},
				{questionIndex: 1, category: "MYSQL"},
			},
			evaluations: []*questionEvalDTO{eval(0, 99), eval(1, 99)},
			wantOverall: 0,
			wantCategories: []CategoryScoreDTO{
				{Category: "MYSQL", Score: 0, QuestionCount: 2},
			},
		},
		{
			name: "追问计入总分且未答按0分拉低",
			records: []qaRecord{
				{questionIndex: 0, category: "MYSQL", userAnswer: answered("答")},
				{questionIndex: 1, category: "MYSQL", followUp: true}, // 未答
			},
			evaluations: []*questionEvalDTO{eval(0, 90), eval(1, 90)},
			wantOverall: 45, // (90+0)/2
			wantCategories: []CategoryScoreDTO{
				{Category: "MYSQL", Score: 45, QuestionCount: 2},
			},
		},
		{
			name: "均分int截断",
			records: []qaRecord{
				{questionIndex: 0, category: "MYSQL", userAnswer: answered("答")},
				{questionIndex: 1, category: "MYSQL", userAnswer: answered("答")},
			},
			evaluations: []*questionEvalDTO{eval(0, 85), eval(1, 80)},
			wantOverall: 82, // 82.5 截断
			wantCategories: []CategoryScoreDTO{
				{Category: "MYSQL", Score: 82, QuestionCount: 2},
			},
		},
		{
			name: "跨分类平均与稳定排序",
			records: []qaRecord{
				{questionIndex: 0, category: "REDIS", userAnswer: answered("答")},
				{questionIndex: 1, category: "MYSQL", userAnswer: answered("答")},
				{questionIndex: 2, category: "MYSQL", userAnswer: answered("答")},
			},
			evaluations: []*questionEvalDTO{eval(0, 70), eval(1, 90), eval(2, 80)},
			wantOverall: 80,
			wantCategories: []CategoryScoreDTO{
				{Category: "MYSQL", Score: 85, QuestionCount: 2},
				{Category: "REDIS", Score: 70, QuestionCount: 1},
			},
		},
		{
			name:           "无题目总分0空分类",
			records:        nil,
			evaluations:    nil,
			wantOverall:    0,
			wantCategories: []CategoryScoreDTO{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := buildReport("sess-1", tc.records, tc.evaluations, "评语", []string{}, []string{})
			if report.OverallScore != tc.wantOverall {
				t.Errorf("overallScore want %d got %d", tc.wantOverall, report.OverallScore)
			}
			if !reflect.DeepEqual(report.CategoryScores, tc.wantCategories) {
				t.Errorf("categoryScores want %+v got %+v", tc.wantCategories, report.CategoryScores)
			}
			if report.TotalQuestions != len(tc.records) {
				t.Errorf("totalQuestions want %d got %d", len(tc.records), report.TotalQuestions)
			}
		})
	}

	// 评估缺失位置的反馈默认文案
	report := buildReport("sess-1",
		[]qaRecord{{questionIndex: 0, category: "MYSQL", userAnswer: answered("答")}},
		nil, "评语", []string{}, []string{})
	if report.QuestionDetails[0].Feedback != "该题未成功生成评估反馈。" {
		t.Errorf("反馈缺失默认文案不符: %q", report.QuestionDetails[0].Feedback)
	}
	if report.QuestionDetails[0].Score != 0 || report.ReferenceAnswers[0].ReferenceAnswer != "" {
		t.Errorf("评估缺失时应为 0 分与空参考: %+v", report.QuestionDetails[0])
	}
	// 未回答的 userAnswer 原样保留 nil
	report2 := buildReport("sess-1",
		[]qaRecord{{questionIndex: 0, category: "MYSQL"}},
		nil, "评语", nil, nil)
	if report2.QuestionDetails[0].UserAnswer != nil {
		t.Errorf("未回答 userAnswer 应为 null")
	}
	if report2.Strengths == nil || report2.Improvements == nil {
		t.Errorf("nil strengths/improvements 应回退空列表")
	}
}

// ===== mergeListItems / sanitizeItems =====

func TestMergeListItemsDedupAndLimit(t *testing.T) {
	batch := func(items ...string) *batchReportDTO {
		return &batchReportDTO{Strengths: items, Improvements: items}
	}
	results := []batchResult{
		{report: batch("A", "B", "B", "  C  ", "")},
		{report: batch("C", "D")},
		{report: nil}, // 整批失败跳过
	}
	got := mergeListItems(results, true)
	want := []string{"A", "B", "C", "D"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeListItems want %v got %v", want, got)
	}

	// 上限 8 条：10 个去重后只留 8
	many := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		many = append(many, fmt.Sprintf("s%d", i))
	}
	got = mergeListItems([]batchResult{{report: batch(many...)}}, true)
	if len(got) != 8 || got[7] != "s7" {
		t.Errorf("mergeListItems 上限 8 条: got %v", got)
	}
}

func TestSanitizeItemsFallbackAndLimit(t *testing.T) {
	// 主列表为空回退兜底
	got := sanitizeItems(nil, []string{" 兜底A ", "兜底A", "", "兜底B"})
	if want := []string{"兜底A", "兜底B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sanitizeItems want %v got %v", want, got)
	}
	// 主列表非空则忽略兜底，trim+去重+上限 8
	primary := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		primary = append(primary, fmt.Sprintf(" 主%d ", i))
	}
	got = sanitizeItems(primary, []string{"兜底"})
	if len(got) != 8 || got[0] != "主0" || got[7] != "主7" {
		t.Errorf("sanitizeItems 上限与 trim: %v", got)
	}
	// 双空 → 空列表（非 nil）
	if empty := sanitizeItems(nil, nil); empty == nil || len(empty) != 0 {
		t.Errorf("sanitizeItems 双空应返回空列表")
	}
}

// ===== buildQuestionHighlights 行数上限 =====

func TestBuildQuestionHighlightsLimitAndTruncation(t *testing.T) {
	records := make([]qaRecord, 0, 25)
	evals := make([]*questionEvalDTO, 0, 25)
	for i := 0; i < 25; i++ {
		records = append(records, qaRecord{questionIndex: i, question: fmt.Sprintf("题目%d", i), category: "MYSQL"})
		evals = append(evals, &questionEvalDTO{QuestionIndex: i, Score: i,
			Feedback: stringPtr(strings.Repeat("反", 90))})
	}
	got := buildQuestionHighlights(records, evals)
	lines := strings.Split(got, "\n")
	if len(lines) != 20 {
		t.Fatalf("高亮行数上限 20，got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "- Q1 | 题目0 | 分数:0 | 反馈:"+strings.Repeat("反", 80)+"...") {
		t.Errorf("首行格式/反馈截断不符: %s", lines[0])
	}
	if !strings.HasPrefix(lines[19], "- Q20 | ") {
		t.Errorf("第 20 行应是 Q20: %s", lines[19])
	}
}

// ===== 辅助 =====
