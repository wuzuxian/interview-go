// 知识库面试纯函数表驱动单测（票 05，spec 测试决策 4）：洗牌抽题语义
// （主问题整体 Fisher-Yates、追问局部洗牌不改原列表）、追问池严格抽取与防御、
// 容量计算（分类排序/档位可选性）、缺口明细消息、难度与方向归一化。
// 随机性以脚本化 nextInt 驱动，断言确定性排列。
package knowledgebase

import (
	"strings"
	"testing"

	"github.com/wuzuxian/interview/internal/apperr"
)

// kbSource 构造候选题（方向/追问池数）。
func kbSource(category string, followUps int) questionSource {
	q := KnowledgeBaseQuestionEntity{Question: "题干-" + category}
	if category != "" {
		q.Category = stringPtr(category)
	}
	pool := make([]QuestionFollowUpDTO, 0, followUps)
	for i := 0; i < followUps; i++ {
		pool = append(pool, QuestionFollowUpDTO{Question: "追问"})
	}
	return questionSource{question: q, keyPoints: []string{}, followUps: pool}
}

// seqInt 固定序列随机源（依次返回，越界取尾）。
func seqInt(values ...int) func(origin, bound int) int {
	i := 0
	return func(origin, bound int) int {
		if i >= len(values) {
			return bound - 1
		}
		v := values[i]
		i++
		if v < origin {
			return origin
		}
		if v >= bound {
			return bound - 1
		}
		return v
	}
}

func originInt(origin, _ int) int { return origin }

// 契约：主问题整体洗牌为末端 Fisher-Yates（Java Collections.shuffle），是全排列
// 而非抽样——洗牌后集合不变；脚本化随机可复现指定排列。
func TestShuffleQuestionSources整体洗牌(t *testing.T) {
	t.Run("恒取下界时复现固定排列", func(t *testing.T) {
		list := []questionSource{kbSource("A", 0), kbSource("B", 0), kbSource("C", 0)}
		// i=2 取 j=0 → C 到首位；i=1 取 j=0 → B 与 C 再换位 → 最终 B, C, A。
		shuffleQuestionSources(list, originInt)
		for i, want := range []string{"B", "C", "A"} {
			if *list[i].question.Category != want {
				t.Fatalf("list[%d] = %s, want %s", i, *list[i].question.Category, want)
			}
		}
	})
	t.Run("脚本序列复现两两交换", func(t *testing.T) {
		list := []questionSource{kbSource("A", 0), kbSource("B", 0), kbSource("C", 0)}
		// n=3：i=2 取 j∈[0,3)=0 → C 换到首位；i=1 取 j∈[0,2)=1 → 不换。
		shuffleQuestionSources(list, seqInt(0, 1))
		for i, want := range []string{"C", "B", "A"} {
			if *list[i].question.Category != want {
				t.Fatalf("list[%d] = %s, want %s", i, *list[i].question.Category, want)
			}
		}
	})
	t.Run("洗牌不增删元素", func(t *testing.T) {
		list := []questionSource{kbSource("A", 0), kbSource("B", 0), kbSource("C", 0), kbSource("D", 0)}
		before := map[string]int{}
		for _, s := range list {
			before[*s.question.Category]++
		}
		shuffleQuestionSources(list, func(origin, bound int) int { return bound - 1 })
		after := map[string]int{}
		for _, s := range list {
			after[*s.question.Category]++
		}
		if len(after) != len(before) {
			t.Fatalf("洗牌改变元素集合: %v → %v", before, after)
		}
	})
}

// 契约：追问严格抽取（Java pickFollowUps 逐字）——count≤0 空列表；池不足 → 3009
// 固定文案；n==池大小时原序拷贝；局部洗牌只动前 n 位且不改原列表。
func TestPickFollowUps严格抽取(t *testing.T) {
	pool := func(n int) []QuestionFollowUpDTO {
		out := make([]QuestionFollowUpDTO, 0, n)
		for i := 0; i < n; i++ {
			q := string("追问" + string(rune('A'+i)))
			out = append(out, QuestionFollowUpDTO{Question: q})
		}
		return out
	}

	t.Run("count 为 0 返回空列表", func(t *testing.T) {
		got, err := pickFollowUps(pool(3), 0, originInt)
		if err != nil || len(got) != 0 {
			t.Fatalf("got = %v, err = %v", got, err)
		}
	})

	t.Run("池不足拒绝组装并给固定文案", func(t *testing.T) {
		_, err := pickFollowUps(pool(2), 3, originInt)
		ae, ok := err.(*apperr.AppError)
		if !ok {
			t.Fatalf("期望 AppError, got %v", err)
		}
		if ae.Code != 3009 || ae.Message != "追问池在组装面试时发生变化，无法严格抽取 3 个追问" {
			t.Fatalf("code/message = [%d %q]", ae.Code, ae.Message)
		}
	})

	t.Run("n 等于池大小时原序拷贝", func(t *testing.T) {
		p := pool(3)
		got, err := pickFollowUps(p, 3, func(origin, bound int) int { return bound - 1 })
		if err != nil || len(got) != 3 {
			t.Fatalf("got = %v, err = %v", got, err)
		}
		for i := range p {
			if got[i].Question != p[i].Question {
				t.Fatalf("got[%d] = %s, want %s", i, got[i].Question, p[i].Question)
			}
		}
		// 拷贝语义：改返回值不影响原池。
		got[0].Question = "改"
		if p[0].Question == "改" {
			t.Fatal("返回值应为基础类型切片拷贝，不共享存储")
		}
	})

	t.Run("局部洗牌只取前 n 个且原列表不变", func(t *testing.T) {
		p := pool(4)
		// i=0 取 j=2 → A 与 C 换位；i=1 取 j=3 → B 与 D 换位。前 2 位 = C, D。
		got, err := pickFollowUps(p, 2, seqInt(2, 3))
		if err != nil || len(got) != 2 {
			t.Fatalf("got = %v, err = %v", got, err)
		}
		if got[0].Question != "追问C" || got[1].Question != "追问D" {
			t.Fatalf("got = [%s %s], want [追问C 追问D]", got[0].Question, got[1].Question)
		}
		// 原列表不变（Java：避免改动原列表顺序）。
		for i, want := range []string{"追问A", "追问B", "追问C", "追问D"} {
			if p[i].Question != want {
				t.Fatalf("原列表被改动: p[%d] = %s, want %s", i, p[i].Question, want)
			}
		}
	})
}

// 契约：容量分类选项——只计非空方向、可用题数降序、同数按方向名字典序升序。
func TestCalculateCategoryOptions排序(t *testing.T) {
	sources := []questionSource{
		kbSource("Java", 0), kbSource("Java", 0),
		kbSource("Redis", 0),
		kbSource("MySQL", 0), kbSource("MySQL", 0),
		kbSource("", 0), // 空方向不计
	}
	options := calculateCategoryOptions(sources)
	if len(options) != 3 {
		t.Fatalf("options = %v, want 3 项", options)
	}
	want := []struct {
		category string
		count    int
	}{{"Java", 2}, {"MySQL", 2}, {"Redis", 1}}
	for i, w := range want {
		if options[i].Category != w.category || options[i].AvailableQuestionCount != w.count {
			t.Fatalf("options[%d] = %+v, want %s/%d", i, options[i], w.category, w.count)
		}
	}
}

// 契约：追问档位 0-5 的可用量与可选性（selectable = 主问题数 > 0 且可用量 ≥ 主问题数）。
func TestCalculateFollowUpOptions档位与可选性(t *testing.T) {
	scoped := []questionSource{kbSource("A", 3), kbSource("B", 1), kbSource("C", 0)}
	options := calculateFollowUpOptions(scoped, 2)
	if len(options) != 6 {
		t.Fatalf("options = %d 项, want 6", len(options))
	}
	wantAvailable := []int{3, 2, 1, 1, 0, 0} // 池 ≥ n 的题数（A=3、B=1、C=0）
	for i, opt := range options {
		if opt.FollowUpCount != i || opt.AvailableQuestionCount != wantAvailable[i] {
			t.Fatalf("options[%d] = %+v, want count=%d available=%d", i, opt, i, wantAvailable[i])
		}
		if opt.Selectable != (wantAvailable[i] >= 2) {
			t.Fatalf("options[%d].Selectable = %v, want %v", i, opt.Selectable, wantAvailable[i] >= 2)
		}
	}

	// 主问题数为 0 时全部不可选（Java mainQuestionCount > 0 守卫）。
	for _, opt := range calculateFollowUpOptions(scoped, 0) {
		if opt.Selectable {
			t.Fatal("主问题数为 0 时 selectable 应恒为 false")
		}
	}
}

// 契约：追问池过滤——池 ≥ followUpCount 的题入选。
func TestFilterByFollowUpPool(t *testing.T) {
	sources := []questionSource{kbSource("A", 2), kbSource("B", 1), kbSource("C", 0), kbSource("D", 3)}
	cases := []struct {
		followUpCount int
		want          []string
	}{
		{0, []string{"A", "B", "C", "D"}},
		{1, []string{"A", "B", "D"}},
		{2, []string{"A", "D"}},
		{3, []string{"D"}},
		{4, nil},
	}
	for _, tc := range cases {
		got := filterByFollowUpPool(sources, tc.followUpCount)
		if len(got) != len(tc.want) {
			t.Fatalf("followUpCount=%d got %d 项, want %d", tc.followUpCount, len(got), len(tc.want))
		}
		for i, w := range tc.want {
			if *got[i].question.Category != w {
				t.Errorf("followUpCount=%d got[%d] = %s, want %s", tc.followUpCount, i, *got[i].question.Category, w)
			}
		}
	}
}

// 契约：题目组装——主问题携带参考基线与来源上下文；追问指向主问题、基线来自追问池、
// 来源上下文沿用主问题实体；方向缺省回退（Java defaultString 逐字）。
func TestBuildInterviewQuestions组装(t *testing.T) {
	ref := "参考答案"
	rubric := "评分规则"
	entity := KnowledgeBaseQuestionEntity{
		Question:        "主问题",
		Category:        stringPtr("Java"),
		TopicSummary:    stringPtr("摘要"),
		ReferenceAnswer: &ref,
		ScoringRubric:   &rubric,
		SourceContext:   stringPtr("片段"),
	}
	source := questionSource{
		question:  entity,
		keyPoints: []string{"要点1", "要点2"},
		followUps: []QuestionFollowUpDTO{
			{Question: "追问A", ReferenceAnswer: stringPtr("追问参考A"), KeyPoints: []string{"追问题点"}},
			{Question: "追问B"},
		},
	}
	questions, err := buildInterviewQuestions([]questionSource{source}, 2, originInt)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if len(questions) != 3 {
		t.Fatalf("questions = %d 条, want 3", len(questions))
	}
	main := questions[0]
	if main.QuestionIndex != 0 || main.IsFollowUp || main.ParentQuestionIndex != nil {
		t.Fatalf("主问题形状不符: %+v", main)
	}
	if main.ReferenceAnswer == nil || *main.ReferenceAnswer != "参考答案" ||
		main.ScoringRubric == nil || *main.ScoringRubric != "评分规则" {
		t.Fatalf("主问题参考基线不符: %+v", main)
	}
	if len(main.KeyPoints) != 2 || main.KeyPoints[0] != "要点1" {
		t.Fatalf("主问题要点不符: %v", main.KeyPoints)
	}
	if main.SourceContext == nil || *main.SourceContext != "片段" {
		t.Fatalf("主问题来源上下文不符: %v", main.SourceContext)
	}
	for i, wantQ := range []string{"追问A", "追问B"} {
		f := questions[i+1]
		if !f.IsFollowUp || f.ParentQuestionIndex == nil || *f.ParentQuestionIndex != 0 {
			t.Fatalf("追问 %d 追问标记不符: %+v", i, f)
		}
		if f.Question != wantQ {
			t.Fatalf("追问 %d 题干 = %s, want %s", i, f.Question, wantQ)
		}
		if f.QuestionIndex != i+1 {
			t.Fatalf("追问 %d 索引 = %d", i, f.QuestionIndex)
		}
		if f.SourceContext == nil || *f.SourceContext != "片段" {
			t.Fatalf("追问 %d 来源上下文应沿用主问题实体", i)
		}
		if f.Category == nil || *f.Category != "Java" {
			t.Fatalf("追问 %d 分类应沿用实体方向原值: %v", i, f.Category)
		}
	}
	if questions[1].ReferenceAnswer == nil || *questions[1].ReferenceAnswer != "追问参考A" {
		t.Fatalf("追问 1 参考答案应来自追问池: %v", questions[1].ReferenceAnswer)
	}
	if questions[2].ReferenceAnswer != nil {
		t.Fatalf("追问 2 无参考答案应保持 null: %v", questions[2].ReferenceAnswer)
	}
}

// 契约：实体方向为空白时主问题回退「知识库」、追问回退「知识库追问」（Java 缺省分支）。
func TestBuildInterviewQuestions方向缺省回退(t *testing.T) {
	source := questionSource{
		question:  KnowledgeBaseQuestionEntity{Question: "主问题"}, // 无方向
		keyPoints: []string{},
		followUps: []QuestionFollowUpDTO{{Question: "追问A"}},
	}
	questions, err := buildInterviewQuestions([]questionSource{source}, 1, originInt)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if questions[0].Category == nil || *questions[0].Category != "知识库" {
		t.Fatalf("主问题分类缺省 = %v, want 知识库", questions[0].Category)
	}
	if questions[1].Category == nil || *questions[1].Category != "知识库追问" {
		t.Fatalf("追问分类缺省 = %v, want 知识库追问", questions[1].Category)
	}
	if questions[0].Type == nil || *questions[0].Type != "KNOWLEDGE_BASE" {
		t.Fatalf("type 缺省 = %v, want KNOWLEDGE_BASE", questions[0].Type)
	}
}

// 契约：缺口明细消息逐字（方向空 → 全部方向；含难度与追问档位）。
func TestBuildInsufficientMessage逐字(t *testing.T) {
	cases := []struct {
		name      string
		required  int
		available int
		category  *string
		want      string
	}{
		{"方向为空显示全部方向", 3, 1, nil,
			"需要 3 道主问题，但只有 1 道同时满足：方向=全部方向、难度=mid、每题至少 2 个追问"},
		{"方向原值进消息", 5, 0, stringPtr("Java"),
			"需要 5 道主问题，但只有 0 道同时满足：方向=Java、难度=mid、每题至少 2 个追问"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildInsufficientMessage(tc.required, tc.available, tc.category, "mid", 2)
			if got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

// 契约：难度归一（空白 → mid，其余 trim）；方向空白归 null。
func TestNormalizeInterviewDifficultyAndCategory(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want string
	}{
		{"nil 默认 mid", nil, "mid"},
		{"空白默认 mid", stringPtr("  "), "mid"},
		{"原值 trim", stringPtr(" senior "), "senior"},
		{"自由文本原样透传", stringPtr("hard"), "hard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeInterviewDifficulty(tc.in); got != tc.want {
				t.Fatalf("difficulty = %q, want %q", got, tc.want)
			}
		})
	}
	if got := trimToNullPtr(stringPtr(" Java ")); got == nil || *got != "Java" {
		t.Fatalf("trimToNullPtr = %v", got)
	}
	if got := trimToNullPtr(stringPtr("   ")); got != nil {
		t.Fatalf("空白应归 null: %v", got)
	}
}

// 契约：可用追问池解析——剔除空白题干、题干 trim、保留参考基线。
func TestReadUsableFollowUps解析过滤(t *testing.T) {
	raw := stringPtr(`[
		{"question":"  ","referenceAnswer":"丢弃"},
		{"question":" 追问A ","referenceAnswer":"参考A","keyPoints":["要点"],"scoringRubric":"规则"},
		{"referenceAnswer":"缺题干"},
		{"question":"追问B"}
	]`)
	got := readUsableFollowUps(raw)
	if len(got) != 2 {
		t.Fatalf("got = %d 条, want 2", len(got))
	}
	if got[0].Question != "追问A" || got[0].ReferenceAnswer == nil || *got[0].ReferenceAnswer != "参考A" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if len(got[0].KeyPoints) != 1 || got[0].ScoringRubric == nil {
		t.Fatalf("got[0] 基线不符: %+v", got[0])
	}
	if got[1].Question != "追问B" {
		t.Fatalf("got[1] = %+v", got[1])
	}

	if empty := readUsableFollowUps(nil); len(empty) != 0 {
		t.Fatalf("nil 输入应为空池")
	}
	if empty := readUsableFollowUps(stringPtr("not-json")); len(empty) != 0 {
		t.Fatalf("损坏 JSON 应降级空池")
	}
}

// 契约：可用性过滤后的组装修复防御——候选阶段池被过滤后不足 → 3009 透出
// （createSession 的 candidates 过滤已覆盖；此处锁 pickFollowUps 的防御上抛链路）。
func TestBuildInterviewQuestions追问池竞态防御(t *testing.T) {
	source := questionSource{
		question:  KnowledgeBaseQuestionEntity{Question: "主问题"},
		keyPoints: []string{},
		followUps: []QuestionFollowUpDTO{{Question: "仅有的追问"}},
	}
	_, err := buildInterviewQuestions([]questionSource{source}, 2, originInt)
	if err == nil {
		t.Fatal("池不足应报错")
	}
	if !apperr.Is(err, 3009) || !strings.Contains(err.Error(), "无法严格抽取 2 个追问") {
		t.Fatalf("err = %v", err)
	}
}
