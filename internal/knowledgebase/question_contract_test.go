// 题库接口 HTTP 契约缝（主缝，票 04）：httptest 真路由 + 依赖全 fake，锁
// generate/generation-status/questions 列表/分类统计/建题/更新/改状态/删除 的
// Result JSON 字段名与错误码。契约来源：Java KnowledgeBaseInterviewController +
// KnowledgeBaseQuestionService + QuestionGenerationStateService 逐字。
package knowledgebase

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// errAddTask 脚本化 XADD 失败（投递失败回退状态链路）。
var errAddTask = errors.New("xadd 投递失败")

// questionFixture 题库接口契约测试夹具：复用 contractFixture 的引擎与 fake 依赖，
// 追加题库路由与出题服务。
type questionFixture struct {
	*contractFixture
	svc *QuestionService
}

func newQuestionFixture(t *testing.T) *questionFixture {
	t.Helper()
	h := &questionFixture{contractFixture: newContractFixture(t)}
	h.svc = NewQuestionService(h.f.store, NewQuestionGenProducer(h.f.qstream, h.f.store, discardLogger()),
		discardLogger())
	NewQuestionController(h.svc, ratelimit.New(errScripter{}, discardLogger())).RegisterRoutes(h.engine.Group("/api"))
	return h
}

// seedCompletedKB 落一个向量化完成的知识库。
func seedCompletedKB(t *testing.T, h *questionFixture) int64 {
	t.Helper()
	return seedKB(t, h.contractFixture, func(kb *KnowledgeBaseEntity) { kb.VectorStatus = VectorCompleted })
}

// dataMap Result.data 解析为对象。
func dataMap(t *testing.T, data json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 data 对象失败: %v, data=%s", err, data)
	}
	return out
}

// ===== DTO 黄金键集 =====

// TestQuestionDTO契约字段名与Java逐字 KnowledgeBaseQuestionDTO 17 字段 +
// QuestionGenStatusResponse 9 字段 + QuestionGenerationConfig 5 字段 +
// KnowledgeBaseQuestionFollowUpDTO 4 字段 + CategoryCount 2 字段。
func TestQuestionDTO契约字段名与Java逐字(t *testing.T) {
	assertKeys(t, jsonKeys(t, KnowledgeBaseQuestionDTO{}), sortedKeys(
		"id", "knowledgeBaseId", "knowledgeBaseName", "skillId", "difficulty", "type",
		"category", "question", "topicSummary", "referenceAnswer", "keyPoints",
		"scoringRubric", "followUps", "sourceContext", "status", "createdAt", "updatedAt",
	))
	assertKeys(t, jsonKeys(t, QuestionGenStatusResponse{}), sortedKeys(
		"knowledgeBaseId", "questionGenStatus", "questionGenTaskId", "questionGenConfig",
		"savedCount", "skippedCount", "message", "error", "updatedAt",
	))
	assertKeys(t, jsonKeys(t, QuestionGenerationConfig{}), sortedKeys(
		"difficulty", "questionCount", "followUpCount", "categoryLimit", "llmProvider",
	))
	assertKeys(t, jsonKeys(t, QuestionFollowUpDTO{}), sortedKeys(
		"question", "referenceAnswer", "keyPoints", "scoringRubric",
	))
	assertKeys(t, jsonKeys(t, CategoryCount{}), sortedKeys("category", "count"))
}

// ===== generate =====

// TestGenerate契约字段名与Queued快照 成功提交返回 QUEUED + taskId + 配置快照，
// Redis Stream 消息字段对齐 Java 常量（kbId/taskId/retryCount）。
func TestGenerate契约字段名与Queued快照(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)

	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions/generate",
		[]byte(`{"difficulty":"senior","questionCount":10,"followUpCount":2,"categoryLimit":3,"llmProvider":"p1"}`))
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	body := dataMap(t, data)
	if body["knowledgeBaseId"] != float64(id) {
		t.Errorf("knowledgeBaseId = %v, want %v", body["knowledgeBaseId"], id)
	}
	if body["questionGenStatus"] != "QUEUED" {
		t.Errorf("questionGenStatus = %v, want QUEUED", body["questionGenStatus"])
	}
	taskID, _ := body["questionGenTaskId"].(string)
	if len(taskID) != 36 {
		t.Errorf("questionGenTaskId = %v, want 36 位 UUID", body["questionGenTaskId"])
	}
	cfg, ok := body["questionGenConfig"].(map[string]any)
	if !ok {
		t.Fatalf("questionGenConfig 缺失: %v", body)
	}
	if cfg["difficulty"] != "senior" || cfg["questionCount"] != float64(10) ||
		cfg["followUpCount"] != float64(2) || cfg["categoryLimit"] != float64(3) ||
		cfg["llmProvider"] != "p1" {
		t.Errorf("配置快照不符: %v", cfg)
	}
	if body["savedCount"] != float64(0) || body["skippedCount"] != float64(0) {
		t.Errorf("初始计数应归零: %v", body)
	}
	if v, exists := body["message"]; !exists || v != nil {
		t.Errorf("message = %v, want JSON null 键存在", v)
	}

	// Stream 消息字段（Java QuestionGenStreamProducer.buildMessage：kbId/taskId/retryCount）。
	if h.f.qstream.addCount() != 1 {
		t.Fatalf("XADD 次数 = %d, want 1", h.f.qstream.addCount())
	}
	msg := h.f.qstream.addAt(0)
	if msg[FieldKBID] != itoa(id) {
		t.Errorf("消息 kbId = %v, want %d", msg[FieldKBID], id)
	}
	if msg[QuestionFieldTaskID] != taskID {
		t.Errorf("消息 taskId = %v, want %v", msg[QuestionFieldTaskID], taskID)
	}
	if msg[FieldRetryCount] != "0" {
		t.Errorf("消息 retryCount = %v, want 0", msg[FieldRetryCount])
	}
}

// TestGenerate校验消息逐字 对齐 Java @Valid 注解消息，多违规按字段声明序以 ", " 连接。
func TestGenerate校验消息逐字(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"难度不合法", `{"difficulty":"expert","questionCount":5,"categoryLimit":3}`, "题目难度不合法"},
		{"题数过小", `{"questionCount":0,"categoryLimit":3}`, "题目数量最少1题"},
		{"题数过大", `{"questionCount":31,"categoryLimit":3}`, "题目数量最多30题"},
		{"追问过小", `{"questionCount":5,"followUpCount":-1,"categoryLimit":3}`, "追问数量不能小于0"},
		{"追问过大", `{"questionCount":5,"followUpCount":6,"categoryLimit":3}`, "每题追问最多5个"},
		{"方向数为0", `{"questionCount":5,"categoryLimit":0}`, "方向数最少1个"},
		{"方向数缺失", `{"questionCount":5}`, "方向数量不能为空"},
		{"提供商过长", `{"questionCount":5,"categoryLimit":3,"llmProvider":"` + strings.Repeat("x", 65) + `"}`, "模型提供商标识过长"},
		{"多违规按声明序连接", `{"questionCount":0,"followUpCount":9}`, "题目数量最少1题, 每题追问最多5个, 方向数量不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions/generate", []byte(tc.body))
			code, message, _ := h.resultRaw(t)
			if code != apperr.CodeBadRequest || message != tc.want {
				t.Fatalf("code = %d, message = %q, want 400 %q", code, message, tc.want)
			}
		})
	}
}

// TestGenerate幂等与前置状态 QUEUED/PROCESSING 中重复提交 400；
// 未完成向量化 400；知识库不存在 6001。
func TestGenerate幂等与前置状态(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)

	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions/generate",
		[]byte(`{"questionCount":5,"categoryLimit":3}`))
	if code, _, _ := h.resultRaw(t); code != result.CodeSuccess {
		t.Fatalf("首次提交 code = %d", code)
	}
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions/generate",
		[]byte(`{"questionCount":5,"categoryLimit":3}`))
	code, message, _ := h.resultRaw(t)
	if code != apperr.CodeBadRequest || message != "知识库问题正在生成中，请勿重复提交" {
		t.Fatalf("重复提交 code = %d, message = %q", code, message)
	}

	// 未完成向量化。
	other := seedKB(t, h.contractFixture, nil)
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(other)+"/questions/generate",
		[]byte(`{"questionCount":5,"categoryLimit":3}`))
	code, message, _ = h.resultRaw(t)
	if code != apperr.CodeBadRequest || message != "知识库尚未完成向量化" {
		t.Fatalf("未完成向量化 code = %d, message = %q", code, message)
	}

	// 知识库不存在。
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/999/questions/generate",
		[]byte(`{"questionCount":5,"categoryLimit":3}`))
	code, message, _ = h.resultRaw(t)
	if code != apperr.CodeKnowledgeBaseNotFound || message != "知识库不存在" {
		t.Fatalf("不存在 code = %d, message = %q", code, message)
	}
}

// TestGenerate投递失败如实回退状态 Stream 投递失败时返回 getStatus
// （FAILED + 安全文案，Java submitGenerationTask 的 sent=false 分支）。
func TestGenerate投递失败如实回退状态(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	h.f.qstream.addErr = errAddTask

	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions/generate",
		[]byte(`{"questionCount":5,"categoryLimit":3}`))
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d, want 200（Java 返回 getStatus）", code)
	}
	body := dataMap(t, data)
	if body["questionGenStatus"] != "FAILED" {
		t.Fatalf("questionGenStatus = %v, want FAILED", body["questionGenStatus"])
	}
	if body["error"] != safeFailureMessage {
		t.Fatalf("error = %v, want %q", body["error"], safeFailureMessage)
	}
}

// ===== generation-status =====

// TestGenerationStatus轮询 NONE 初始（计数 0、可空键 null），生成中可轮询快照。
func TestGenerationStatus轮询(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions/generation-status", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	body := dataMap(t, data)
	if body["questionGenStatus"] != "NONE" {
		t.Fatalf("初始状态 = %v, want NONE", body["questionGenStatus"])
	}
	if body["savedCount"] != float64(0) || body["skippedCount"] != float64(0) {
		t.Fatalf("初始计数 = %v/%v, want 0/0", body["savedCount"], body["skippedCount"])
	}
	if v, exists := body["questionGenTaskId"]; !exists || v != nil {
		t.Fatalf("questionGenTaskId = %v, want null 键存在", v)
	}
	if v, exists := body["questionGenConfig"]; !exists || v != nil {
		t.Fatalf("questionGenConfig = %v, want null 键存在", v)
	}
}

// ===== 题库列表 =====

// TestQuestions列表与筛选 status/category/difficulty/keyword 过滤，
// keyword 不区分大小写命中题干/参考答案/评分规则/摘要/方向（Java containsKeyword）。
func TestQuestions列表与筛选(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	stem := "什么是 JWT 的签名机制"
	h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Difficulty: stringPtr("mid"), Category: stringPtr("安全"),
		Question: stem, ReferenceAnswer: stringPtr("HMAC 或 RSA"),
		TopicSummary: stringPtr("令牌签名"), Status: QuestionActive,
		CreatedAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC),
	})
	h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Difficulty: stringPtr("senior"), Category: stringPtr("存储"),
		Question: "Redis 持久化 RDB 与 AOF 的取舍", Status: QuestionDraft,
		CreatedAt: time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC),
	})

	// 全量：updatedAt 倒序。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	items := dataItems(t, data)
	if len(items) != 2 {
		t.Fatalf("列表长度 = %d, want 2", len(items))
	}
	if items[0]["question"] != "Redis 持久化 RDB 与 AOF 的取舍" {
		t.Errorf("updatedAt 倒序不符: %v", items[0]["question"])
	}
	first := items[0]
	if first["skillId"] != QuestionSkillID {
		t.Errorf("skillId = %v, want %q", first["skillId"], QuestionSkillID)
	}
	if v, exists := first["topicSummary"]; !exists || v != nil {
		t.Errorf("topicSummary = %v, want null 键存在", v)
	}
	if kp, ok := first["keyPoints"].([]any); !ok || len(kp) != 0 {
		t.Errorf("keyPoints = %v, want 空数组", first["keyPoints"])
	}

	// status 过滤。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions?status=ACTIVE", nil)
	_, _, data = h.resultRaw(t)
	if items = dataItems(t, data); len(items) != 1 || items[0]["question"] != stem {
		t.Fatalf("status 过滤 = %v", items)
	}

	// difficulty 过滤。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions?difficulty=senior", nil)
	_, _, data = h.resultRaw(t)
	if items = dataItems(t, data); len(items) != 1 || items[0]["category"] != "存储" {
		t.Fatalf("difficulty 过滤 = %v", items)
	}

	// category 过滤。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions?category=安全", nil)
	_, _, data = h.resultRaw(t)
	if items = dataItems(t, data); len(items) != 1 || items[0]["question"] != stem {
		t.Fatalf("category 过滤 = %v", items)
	}

	// keyword 命中参考答案（大小写不敏感）。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions?keyword=hmac", nil)
	_, _, data = h.resultRaw(t)
	if items = dataItems(t, data); len(items) != 1 || items[0]["question"] != stem {
		t.Fatalf("keyword 过滤 = %v", items)
	}

	// 非法 status 枚举。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions?status=BOGUS", nil)
	code, message, _ := h.resultRaw(t)
	if code == result.CodeSuccess || !strings.Contains(message, "无效的题目状态") {
		t.Fatalf("非法 status code = %d, message = %q, want 含「无效的题目状态」", code, message)
	}
}

// TestQuestions分类统计 非空方向按计数降序、同数按方向名升序（Java JPQL）。
func TestQuestions分类统计(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	for _, c := range []string{"Redis", "Redis", "JVM", "Redis", "JVM", "网络"} {
		h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
			KnowledgeBaseID: id, SkillID: QuestionSkillID, Category: stringPtr(c),
			Question: "题干-" + c, Status: QuestionDraft,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
	}
	h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Question: "无方向题干", Status: QuestionDraft,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/questions/categories", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	items := dataItems(t, data)
	want := []map[string]any{
		{"category": "Redis", "count": float64(3)},
		{"category": "JVM", "count": float64(2)},
		{"category": "网络", "count": float64(1)},
	}
	if len(items) != len(want) {
		t.Fatalf("分类数 = %d, want %d: %v", len(items), len(want), items)
	}
	for i := range want {
		if items[i]["category"] != want[i]["category"] || items[i]["count"] != want[i]["count"] {
			t.Errorf("items[%d] = %v, want %v", i, items[i], want[i])
		}
	}
}

// ===== 手动建题/更新/改状态/删除 =====

// TestCreateQuestion契约 校验、默认 DRAFT、skill_id 固定、方向空时兜底未分类。
func TestCreateQuestion契约(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)

	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions",
		[]byte(`{"category":"Redis","question":" Redis 缓存穿透怎么应对 ","difficulty":"mid","keyPoints":["布隆过滤器","空值缓存",""],"followUps":[{"question":"布隆过滤器误判怎么办","keyPoints":[null,""]},{"question":""}],"status":"ACTIVE"}`))
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	body := dataMap(t, data)
	if body["question"] != "Redis 缓存穿透怎么应对" {
		t.Errorf("question 应 trim: %v", body["question"])
	}
	if body["status"] != "ACTIVE" {
		t.Errorf("status = %v, want ACTIVE（显式指定）", body["status"])
	}
	if body["skillId"] != QuestionSkillID {
		t.Errorf("skillId = %v", body["skillId"])
	}
	if body["knowledgeBaseName"] != "面试笔记" {
		t.Errorf("knowledgeBaseName = %v", body["knowledgeBaseName"])
	}
	if _, exists := body["kbContentHash"]; exists {
		t.Errorf("DTO 不应透出 kbContentHash: %v", body["kbContentHash"])
	}
	if kps, ok := body["keyPoints"].([]any); !ok || len(kps) != 2 {
		t.Errorf("keyPoints 空白项应被过滤: %v", body["keyPoints"])
	}
	if fus, ok := body["followUps"].([]any); !ok || len(fus) != 1 {
		t.Fatalf("followUps 空题干项应被过滤: %v", body["followUps"])
	}
	fu := body["followUps"].([]any)[0].(map[string]any)
	if fu["question"] != "布隆过滤器误判怎么办" {
		t.Errorf("followUp question = %v", fu["question"])
	}
	if kps, ok := fu["keyPoints"].([]any); !ok || len(kps) != 0 {
		t.Errorf("followUp keyPoints 空白项应被过滤为空数组: %v", fu["keyPoints"])
	}

	// 校验缺失。
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions",
		[]byte(`{"question":"只有题干"}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeBadRequest || message != "面试方向不能为空" {
		t.Fatalf("缺方向 code = %d, message = %q", code, message)
	}
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions",
		[]byte(`{"category":"Redis"}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeBadRequest || message != "题干不能为空" {
		t.Fatalf("缺题干 code = %d, message = %q", code, message)
	}

	// 未指定状态默认 DRAFT。
	h.doMethod(t, http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/questions",
		[]byte(`{"category":"JVM","question":"G1 何时触发 Full GC"}`))
	_, _, data = h.resultRaw(t)
	if body = dataMap(t, data); body["status"] != "DRAFT" {
		t.Fatalf("默认 status = %v, want DRAFT", body["status"])
	}
}

// TestUpdateQuestion契约 部分更新保留未提及字段；空白必填 400；不存在 3003。
func TestUpdateQuestion契约(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	qid := h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Difficulty: stringPtr("mid"), Category: stringPtr("旧方向"),
		Question: "旧题干", ReferenceAnswer: stringPtr("旧答案"), Status: QuestionDraft,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/"+itoa(qid),
		[]byte(`{"category":"新方向","status":"ACTIVE"}`))
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	body := dataMap(t, data)
	if body["category"] != "新方向" || body["status"] != "ACTIVE" {
		t.Fatalf("更新结果 = %v", body)
	}
	if body["question"] != "旧题干" || body["referenceAnswer"] != "旧答案" {
		t.Fatalf("未提及字段应保留: %v", body)
	}

	// 空白必填。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/"+itoa(qid), []byte(`{"question":"  "}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeBadRequest || message != "题干不能为空" {
		t.Fatalf("空白题干 code = %d, message = %q", code, message)
	}
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/"+itoa(qid), []byte(`{"category":" "}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeBadRequest || message != "面试方向不能为空" {
		t.Fatalf("空白方向 code = %d, message = %q", code, message)
	}

	// 不存在。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/999", []byte(`{"status":"ACTIVE"}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeInterviewQuestionNotFound || message != "面试问题不存在" {
		t.Fatalf("不存在 code = %d, message = %q", code, message)
	}
}

// TestUpdateQuestionStatus契约 改状态 + status 缺失 400。
func TestUpdateQuestionStatus契约(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	qid := h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Category: stringPtr("Redis"), Question: "题干", Status: QuestionDraft,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/"+itoa(qid)+"/status",
		[]byte(`{"status":"ARCHIVED"}`))
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	if body := dataMap(t, data); body["status"] != "ARCHIVED" {
		t.Fatalf("status = %v, want ARCHIVED", body["status"])
	}

	h.doMethod(t, http.MethodPut, "/api/knowledgebase/questions/"+itoa(qid)+"/status", []byte(`{}`))
	if code, message, _ := h.resultRaw(t); code != apperr.CodeBadRequest || message != "题目状态不能为空" {
		t.Fatalf("缺状态 code = %d, message = %q", code, message)
	}
}

// TestDeleteQuestion契约 删除成功 data=null；不存在 3003。
func TestDeleteQuestion契约(t *testing.T) {
	h := newQuestionFixture(t)
	id := seedCompletedKB(t, h)
	qid := h.f.store.addQuestion(&KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: id, SkillID: QuestionSkillID,
		Question: "待删除", Status: QuestionDraft,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/questions/"+itoa(qid), nil)
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" || string(data) != "null" {
		t.Fatalf("删除 code = %d, message = %q, data = %s", code, message, data)
	}
	if h.f.store.questionCount(id) != 0 {
		t.Fatalf("题目应已删除")
	}

	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/questions/999", nil)
	if code, message, _ := h.resultRaw(t); code != apperr.CodeInterviewQuestionNotFound || message != "面试问题不存在" {
		t.Fatalf("不存在 code = %d, message = %q", code, message)
	}
}

// TestQuestionID路径参数解析契约 parseQuestionID 与 parseID 同语义：
// 非数字/负数/int64 溢出 → 400「无效的ID参数」（Long.parseLong 语义 + 负数拒绝）。
func TestQuestionID路径参数解析契约(t *testing.T) {
	h := newQuestionFixture(t)
	cases := []struct {
		name       string
		questionID string
	}{
		{"非数字拒绝", "abc"},
		{"负数拒绝", "-1"},
		{"int64溢出拒绝", "99999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.doMethod(t, http.MethodDelete, "/api/knowledgebase/questions/"+tc.questionID, nil)
			code, message, _ := h.resultRaw(t)
			if code != apperr.CodeBadRequest || message != "无效的ID参数" {
				t.Fatalf("questionId=%s: code = %d, message = %q, want %d/无效的ID参数",
					tc.questionID, code, message, apperr.CodeBadRequest)
			}
		})
	}
}
