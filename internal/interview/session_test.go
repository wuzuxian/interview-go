// 场次创建链路纯逻辑单测（spec 测试决策：DB/HTTP 层不写自动化集成测试）。
// 覆盖：requestId/sessionId 生成与校验、兜底题数据形状与 questions_json 序列化契约、
// 复用判定纯逻辑、错误码消息、DTO 契约序列化。
package interview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/config"
	"github.com/wuzuxian/interview/internal/apperr"
)

// ===== requestId 归一化与校验（对齐 Java normalizeRequestId）=====

// 契约：空白视为未传（完全跳过幂等）；8-64 位 [A-Za-z0-9_-] 之外一律 400
// "requestId 格式不正确"；先 trim 再校验。
func TestNormalizeRequestID(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantNil bool
		want    string
		wantErr bool
	}{
		{"空串跳过幂等", "", true, "", false},
		{"纯空白跳过幂等", "   ", true, "", false},
		{"两侧空白被 trim", "  abc-12345_x  ", false, "abc-12345_x", false},
		{"最短 8 位合法", "abcd1234", false, "abcd1234", false},
		{"最长 64 位合法", strings.Repeat("a", 64), false, strings.Repeat("a", 64), false},
		{"7 位非法", "abcd123", true, "", true},
		{"65 位非法", strings.Repeat("a", 65), true, "", true},
		{"含感叹号非法", "abcd123!", true, "", true},
		{"含中文非法", "abcd1234中", true, "", true},
		{"含内部空格非法", "abcd 1234", true, "", true},
		{"含点号非法", "abcd.1234", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRequestID(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，got nil")
				}
				ae := apperr.From(err)
				if ae.Code != apperr.CodeBadRequest || ae.Message != "requestId 格式不正确" {
					t.Fatalf("错误码/消息不符: got [%d %s]", ae.Code, ae.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错: %v", err)
			}
			if tc.wantNil && got != nil {
				t.Fatalf("期望 nil，got %v", *got)
			}
			if !tc.wantNil {
				if got == nil || *got != tc.want {
					t.Fatalf("期望 %q，got %v", tc.want, got)
				}
			}
		})
	}
}

// 契约：去连字符 UUID 的前 16 个十六进制字符；多次生成不重复。
func TestNewSessionID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewSessionID()
		if len(id) != 16 {
			t.Fatalf("长度应为 16，got %q (%d)", id, len(id))
		}
		for _, r := range id {
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
			if !isHex {
				t.Fatalf("应仅含小写十六进制字符，got %q", id)
			}
		}
		if seen[id] {
			t.Fatalf("sessionId 不应重复: %q", id)
		}
		seen[id] = true
	}
}

// ===== 兜底题（笔记 02 逐字移植）=====

// 契约：无分类时用 GENERIC_FALLBACK_QUESTIONS 前 min(count,6) 条，题目/分类逐字一致。
func TestFallbackGenericQuestionsVerbatim(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 3, 0)
	if len(questions) != 3 {
		t.Fatalf("期望 3 题，got %d", len(questions))
	}
	wantFirst := "请描述一个你主导解决的技术难题，你的分析思路是什么？"
	if questions[0].Question != wantFirst {
		t.Fatalf("第一条题目与 Java 逐字不一致:\n got %q\nwant %q", questions[0].Question, wantFirst)
	}
	if *questions[0].Type != "GENERAL" || *questions[0].Category != "综合能力" {
		t.Fatalf("分类 key/label 不符: %v/%v", *questions[0].Type, *questions[0].Category)
	}
	wantThird := "请分享一次你处理线上故障的经历，从发现到修复的完整过程。"
	if questions[2].Question != wantThird {
		t.Fatalf("第三条题目与 Java 逐字不一致: got %q", questions[2].Question)
	}
	for i, q := range questions {
		if q.QuestionIndex != i {
			t.Fatalf("questionIndex 应连续: got %d at %d", q.QuestionIndex, i)
		}
		if q.IsFollowUp || q.ParentQuestionIndex != nil {
			t.Fatalf("followUpCount=0 时不应有追问: %+v", q)
		}
	}
}

// 契约：count 超过 6 条时只生成前 6 条（min(count, GENERIC.length)）。
func TestFallbackGenericCappedAtSix(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 20, 0)
	if len(questions) != 6 {
		t.Fatalf("分类为空时应封顶 6 条，got %d", len(questions))
	}
	wantLast := "你在团队协作中遇到过最大的分歧是什么？如何解决的？"
	if questions[5].Question != wantLast {
		t.Fatalf("第 6 条与 Java 不一致: got %q", questions[5].Question)
	}
}

// 契约：追问文案与分类后缀逐字（buildDefaultFollowUp / buildFollowUpCategory）。
func TestFallbackFollowUpVerbatim(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 1, 2)
	if len(questions) != 3 {
		t.Fatalf("1 主 + 2 追问 = 3 题，got %d", len(questions))
	}
	main := questions[0]
	if main.IsFollowUp {
		t.Fatal("第 0 条应是主问题")
	}
	f1, f2 := questions[1], questions[2]
	wantF1 := "基于\"" + main.Question + "\"，请结合你亲自做过的一个真实场景展开说明。"
	wantF2 := "基于\"" + main.Question + "\"，如果线上出现异常，你会如何定位并给出修复方案？"
	if f1.Question != wantF1 {
		t.Fatalf("追问1文案不符:\n got %q\nwant %q", f1.Question, wantF1)
	}
	if f2.Question != wantF2 {
		t.Fatalf("追问2文案不符:\n got %q\nwant %q", f2.Question, wantF2)
	}
	if !f1.IsFollowUp || !f2.IsFollowUp {
		t.Fatal("追问 isFollowUp 应为 true")
	}
	if f1.ParentQuestionIndex == nil || *f1.ParentQuestionIndex != 0 ||
		f2.ParentQuestionIndex == nil || *f2.ParentQuestionIndex != 0 {
		t.Fatalf("追问应指向主问题 0: %v %v", f1.ParentQuestionIndex, f2.ParentQuestionIndex)
	}
	if *f1.Category != "综合能力（追问1）" || *f2.Category != "综合能力（追问2）" {
		t.Fatalf("追问分类后缀不符: %q %q", *f1.Category, *f2.Category)
	}
	if *f1.Type != "GENERAL" {
		t.Fatalf("追问 type 应沿用主问题分类 key: %q", *f1.Type)
	}
}

// 契约：有分类时按分类模板轮转（generated % len(categories)），模板句式逐字。
func TestFallbackCategoryTemplateCycling(t *testing.T) {
	categories := []CategoryDTO{
		{Key: "JAVA", Label: "Java"},
		{Key: "MYSQL", Label: "MySQL"},
	}
	questions := GenerateFallbackQuestions(categories, 5, 0)
	if len(questions) != 5 {
		t.Fatalf("期望 5 题，got %d", len(questions))
	}
	wantCycle := []CategoryDTO{categories[0], categories[1], categories[0], categories[1], categories[0]}
	for i, q := range questions {
		wantCat := wantCycle[i]
		wantQuestion := "请谈谈你在\"" + wantCat.Label + "\"方向的技术理解和实践经验。"
		if q.Question != wantQuestion {
			t.Fatalf("模板句式不符 at %d:\n got %q\nwant %q", i, q.Question, wantQuestion)
		}
		if *q.Type != wantCat.Key || *q.Category != wantCat.Label {
			t.Fatalf("分类轮转不符 at %d: %v/%v", i, *q.Type, *q.Category)
		}
	}
}

// 契约：有分类的分类模板题同样按主问题+追问成组，追问指向自己的主问题。
func TestFallbackCategoryWithFollowUps(t *testing.T) {
	categories := []CategoryDTO{{Key: "JAVA", Label: "Java"}}
	questions := GenerateFallbackQuestions(categories, 2, 1)
	if len(questions) != 4 {
		t.Fatalf("2 主 + 2 追问 = 4 题，got %d", len(questions))
	}
	if questions[1].ParentQuestionIndex == nil || *questions[1].ParentQuestionIndex != 0 {
		t.Fatalf("追问1应指向主问题0: %v", questions[1].ParentQuestionIndex)
	}
	if questions[3].ParentQuestionIndex == nil || *questions[3].ParentQuestionIndex != 2 {
		t.Fatalf("追问2应指向主问题2: %v", questions[3].ParentQuestionIndex)
	}
	if questions[3].QuestionIndex != 3 {
		t.Fatalf("questionIndex 应连续: %d", questions[3].QuestionIndex)
	}
	if *questions[3].Category != "Java（追问1）" {
		t.Fatalf("追问分类不符: %q", *questions[3].Category)
	}
}

// 契约：追问数钳制 Math.max(0, Math.min(n, 2))。
func TestClampFollowUpCount(t *testing.T) {
	cases := []struct{ in, want int }{{-1, 0}, {0, 0}, {1, 1}, {2, 2}, {3, 2}, {99, 2}}
	for _, tc := range cases {
		if got := ClampFollowUpCount(tc.in); got != tc.want {
			t.Fatalf("ClampFollowUpCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ===== questions_json 序列化契约（对齐 Java Jackson record 序列化）=====

// 契约：字段名与 Java InterviewQuestionDTO record 组件逐字一致；未赋值字段为 null；
// isFollowUp/score（Java 原生类型语义）恒出现。questions_json 落库后可反序列化还原。
func TestQuestionsJSONContract(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 1, 1)
	raw, err := SerializeQuestions(questions)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("questions_json 应为 JSON 数组: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 条，got %d", len(rows))
	}

	// 与 Java record 组件名逐字一致的完整键集。
	wantKeys := []string{
		"questionIndex", "question", "type", "category", "topicSummary", "userAnswer",
		"score", "feedback", "isFollowUp", "parentQuestionIndex", "referenceAnswer",
		"keyPoints", "scoringRubric", "sourceContext",
	}
	for i, row := range rows {
		if len(row) != len(wantKeys) {
			t.Fatalf("第 %d 条键数量不符: got %d want %d（字段一次建全，多缺都不行）", i, len(row), len(wantKeys))
		}
		for _, k := range wantKeys {
			if _, ok := row[k]; !ok {
				t.Fatalf("第 %d 条缺字段 %q", i, k)
			}
		}
	}

	main := rows[0]
	if main["isFollowUp"] != false {
		t.Fatalf("主问题 isFollowUp 应为 false: %v", main["isFollowUp"])
	}
	if v, ok := main["parentQuestionIndex"]; !ok || v != nil {
		t.Fatalf("主问题 parentQuestionIndex 应存在且为 null: %v %v", ok, v)
	}
	if v, ok := main["userAnswer"]; !ok || v != nil {
		t.Fatalf("未作答 userAnswer 应存在且为 null: %v %v", ok, v)
	}
	follow := rows[1]
	if follow["isFollowUp"] != true {
		t.Fatal("追问 isFollowUp 应为 true")
	}

	// 往返：反序列化后数据无损。
	back, err := DeserializeQuestions(raw)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	again, err := SerializeQuestions(back)
	if err != nil || again != raw {
		t.Fatalf("questions_json 往返应无损:\n got %q\nwant %q (err %v)", again, raw, err)
	}
}

// ===== 复用判定纯逻辑（裁决 #12 + forceCreate 绕过）=====

// 契约：有简历且未显式 forceCreate=true 时复用未完成旧场次；无简历不复用；
// forceCreate=true 绕过复用。
func TestShouldReuseUnfinished(t *testing.T) {
	resumeID := int64(42)
	cases := []struct {
		name        string
		resumeID    *int64
		forceCreate *bool
		want        bool
	}{
		{"无简历不复用", nil, nil, false},
		{"有简历默认复用", &resumeID, nil, true},
		{"有简历 forceCreate=false 复用", &resumeID, boolPtr(false), true},
		{"有简历 forceCreate=true 绕过", &resumeID, boolPtr(true), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldReuseUnfinished(tc.resumeID, tc.forceCreate); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ===== 错误码与消息（笔记 08 错误码表逐字）=====

func TestInterviewErrorCodesAndMessages(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{apperr.CodeInterviewSessionNotFound, "面试会话不存在"},
		{apperr.CodeInterviewSessionExpired, "面试会话已过期"},
		{apperr.CodeInterviewQuestionNotFound, "面试问题不存在"},
		{apperr.CodeInterviewAlreadyCompleted, "面试已完成"},
		{apperr.CodeInterviewEvaluationFailed, "面试评估失败"},
		{apperr.CodeInterviewQuestionGenerationFailed, "面试问题生成失败"},
		{apperr.CodeInterviewNotCompleted, "面试尚未完成"},
		{apperr.CodeInterviewAnswerSaveFailed, "面试答案保存失败"},
		{apperr.CodeInterviewQuestionInsufficient, "符合条件的面试问题不足"},
	}
	for _, tc := range cases {
		ae := apperr.New(tc.code, tc.want)
		if ae.Code != tc.code {
			t.Fatalf("错误码不符: got %d want %d", ae.Code, tc.code)
		}
		if ae.Message != tc.want {
			t.Fatalf("消息不符: got %q want %q", ae.Message, tc.want)
		}
	}

	// 具体业务错误的码值与消息。
	ae := apperr.InterviewSessionNotFound()
	if ae.Code != 3001 || ae.Message != "面试会话不存在" {
		t.Fatalf("InterviewSessionNotFound 不符: [%d %s]", ae.Code, ae.Message)
	}
	ae = apperr.InterviewSessionNotFoundWithMessage("未找到未完成的面试会话")
	if ae.Code != 3001 || ae.Message != "未找到未完成的面试会话" {
		t.Fatalf("unfinished 无记录错误不符: [%d %s]", ae.Code, ae.Message)
	}
	ae = apperr.SkillNotFound("nope")
	if ae.Code != 3101 || ae.Message != "未找到面试主题: nope" {
		t.Fatalf("SkillNotFound 不符: [%d %s]", ae.Code, ae.Message)
	}
}

// ===== DTO 契约序列化（前端零改动切换的根基）=====

// 契约：SessionListItemDTO 字段名与 Java record 逐字一致（笔记 08 §5），字段一次建全。
func TestSessionListItemDTOContract(t *testing.T) {
	b, err := json.Marshal(SessionListItemDTO{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	wantKeys := []string{
		"sessionId", "skillId", "difficulty", "resumeId", "totalQuestions", "status",
		"evaluateStatus", "evaluateError", "overallScore", "sourceType",
		"knowledgeBaseId", "interviewCategory", "createdAt", "completedAt",
	}
	if len(m) != len(wantKeys) {
		t.Fatalf("键数量不符: got %d want %d", len(m), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q，got keys %v", k, m)
		}
	}
}

// 契约：SessionDTO 字段名与 Java InterviewSessionDTO 一致。
func TestSessionDTOContract(t *testing.T) {
	b, _ := json.Marshal(SessionDTO{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	wantKeys := []string{
		"sessionId", "resumeText", "totalQuestions", "currentIndex", "questions",
		"status", "knowledgeBaseId", "interviewCategory",
	}
	if len(m) != len(wantKeys) {
		t.Fatalf("键数量不符: got %d want %d", len(m), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q", k)
		}
	}
}

// 契约：CreateRequest 字段名与 Java CreateInterviewRequest record 一致（一次建全，
// customCategories/jdText 本票接收但票 07 才使用）。
func TestCreateRequestContract(t *testing.T) {
	b, _ := json.Marshal(CreateRequest{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	wantKeys := []string{
		"resumeText", "questionCount", "resumeId", "forceCreate", "llmProvider",
		"skillId", "difficulty", "customCategories", "jdText", "requestId",
	}
	if len(m) != len(wantKeys) {
		t.Fatalf("键数量不符: got %d want %d", len(m), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q", k)
		}
	}
}

// 契约：JSON 反序列化进 CreateRequest 后字段值到位（前端 body → 服务层入参）。
func TestCreateRequestUnmarshal(t *testing.T) {
	body := `{"resumeText":"简历正文","questionCount":5,"resumeId":7,"forceCreate":true,
		"llmProvider":"glm","skillId":"frontend","difficulty":"senior","jdText":"JD 原文",
		"requestId":"req-12345678"}`
	var req CreateRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if req.QuestionCount != 5 || req.ResumeID == nil || *req.ResumeID != 7 ||
		req.ForceCreate == nil || !*req.ForceCreate ||
		req.SkillID == nil || *req.SkillID != "frontend" ||
		req.RequestID != "req-12345678" {
		t.Fatalf("字段绑定不符: %+v", req)
	}
}

// 契约：app.interview 配置段默认值（Java executeWithLock 185s/600s、结果键 1 天、
// 会话缓存 24h、题量 3-20、追问 1）。
func TestInterviewConfigDefaults(t *testing.T) {
	cfg := config.New()
	var c Config
	if err := cfg.UnmarshalKey("interview", &c); err != nil {
		t.Fatalf("绑定 interview 配置失败: %v", err)
	}
	if c.QuestionCountMin != 3 || c.QuestionCountMax != 20 {
		t.Fatalf("题量边界默认值不符: %d-%d", c.QuestionCountMin, c.QuestionCountMax)
	}
	if c.FollowUpCount != 1 {
		t.Fatalf("追问数默认值不符: %d", c.FollowUpCount)
	}
	if c.CreateLockWait != 185*time.Second || c.CreateLockLease != 600*time.Second {
		t.Fatalf("创建锁等待/持有默认值不符: %s / %s", c.CreateLockWait, c.CreateLockLease)
	}
	if c.CreateResultTTL != 24*time.Hour || c.SessionCacheTTL != 24*time.Hour {
		t.Fatalf("结果键/缓存 TTL 默认值不符: %s / %s", c.CreateResultTTL, c.SessionCacheTTL)
	}
}
