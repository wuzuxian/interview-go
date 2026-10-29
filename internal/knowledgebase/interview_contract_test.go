// 知识库面试接口 HTTP 契约缝（主缝，票 05）：httptest 真路由 + 依赖全 fake，
// 锁 POST /api/knowledgebase-interviews/sessions 与 GET /api/knowledgebase/{id}/
// interview-capacity 的 Result JSON 字段名、错误码与容量计算语义。契约来源：
// Java KnowledgeBaseInterviewController + KnowledgeBaseInterviewService +
// InterviewSessionService.createSessionFromQuestions 逐字。
// 「从题目集合创建场次」接缝注入 fake 记录器：锁 skillId/difficulty/llmProvider/
// knowledgeBaseId/interviewCategory 的透传与题目组装的参考基线四字段。
package knowledgebase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wuzuxian/interview/internal/interview"
	"github.com/wuzuxian/interview/internal/result"
)

// fakeSessionCreator interview.SessionFromQuestionsCreator 的 fake：记录透传参数，
// 返回固定形状的 SessionDTO（落库/缓存由 interview 侧单测与 smoke 真库覆盖）。
type fakeSessionCreator struct {
	mu                sync.Mutex
	calls             int
	questions         []interview.QuestionDTO
	llmProvider       *string
	skillID           *string
	difficulty        *string
	knowledgeBaseID   int64
	interviewCategory *string
}

func (f *fakeSessionCreator) CreateSessionFromQuestions(ctx context.Context, questions []interview.QuestionDTO,
	llmProvider, skillID, difficulty *string, knowledgeBaseID int64, interviewCategory *string) (*interview.SessionDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.questions = questions
	f.llmProvider = llmProvider
	f.skillID = skillID
	f.difficulty = difficulty
	f.knowledgeBaseID = knowledgeBaseID
	f.interviewCategory = interviewCategory
	kbID := knowledgeBaseID
	return &interview.SessionDTO{
		SessionID: "fixed0123456789ab", TotalQuestions: len(questions), CurrentQuestionIndex: 0,
		Questions: questions, Status: interview.StatusCreated,
		KnowledgeBaseID: &kbID, InterviewCategory: interviewCategory,
	}, nil
}

func (f *fakeSessionCreator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// interviewFixture 知识库面试契约测试夹具：contractFixture + 面试路由与 fake 接缝。
type interviewFixture struct {
	*contractFixture
	sessions *fakeSessionCreator
	svc      *InterviewService
}

func newInterviewFixture(t *testing.T) *interviewFixture {
	t.Helper()
	h := &interviewFixture{contractFixture: newContractFixture(t), sessions: &fakeSessionCreator{}}
	h.svc = NewInterviewService(h.f.store, h.sessions, discardLogger())
	// 确定性随机源（恒取下界 = 不换位）：洗牌后顺序即落库顺序、追问取池首 n 条，
	// 契约断言不抖动；洗牌/局部洗牌语义本身由纯函数表驱动测试覆盖。
	h.svc.randomInt = func(origin, bound int) int { return origin }
	NewInterviewController(h.svc).RegisterRoutes(h.engine.Group("/api"))
	return h
}

// seedVectorizedKB 落一个向量化完成的知识库。
func seedVectorizedKB(t *testing.T, h *interviewFixture) int64 {
	t.Helper()
	return seedKB(t, h.contractFixture, func(kb *KnowledgeBaseEntity) { kb.VectorStatus = VectorCompleted })
}

// seedQuestion 落一行题目（可定制字段），返回 ID。
func seedQuestion(t *testing.T, h *interviewFixture, kbID int64, mutate func(*KnowledgeBaseQuestionEntity)) int64 {
	t.Helper()
	q := &KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: kbID,
		SkillID:         QuestionSkillID,
		Difficulty:      stringPtr("mid"),
		Category:        stringPtr("Java"),
		Question:        "什么是 JVM 内存模型？",
		Status:          QuestionActive,
	}
	if mutate != nil {
		mutate(q)
	}
	return h.f.store.addQuestion(q)
}

// followUpsJSON 构造追问池 JSON（n 个，含参考基线）。
func followUpsJSON(n int) *string {
	followUps := make([]QuestionFollowUpDTO, 0, n)
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("追问参考答案 %d", i+1)
		rubric := fmt.Sprintf("追问评分规则 %d", i+1)
		followUps = append(followUps, QuestionFollowUpDTO{
			Question:        fmt.Sprintf("追问 %d：请举例说明", i+1),
			ReferenceAnswer: &ref,
			KeyPoints:       []string{fmt.Sprintf("追问要点 %d", i+1)},
			ScoringRubric:   &rubric,
		})
	}
	b, err := json.Marshal(followUps)
	if err != nil {
		panic(err)
	}
	return stringPtr(string(b))
}

// ===== DTO 黄金键集 =====

// TestInterviewCapacityDTO契约字段名与Java逐字 KnowledgeBaseInterviewCapacityResponse
// 6 字段 + CategoryOption 2 字段 + FollowUpOption 3 字段。
func TestInterviewCapacityDTO契约字段名与Java逐字(t *testing.T) {
	assertKeys(t, jsonKeys(t, KnowledgeBaseInterviewCapacityResponse{}), sortedKeys(
		"knowledgeBaseId", "category", "difficulty", "mainQuestionCount", "categories", "followUpOptions",
	))
	assertKeys(t, jsonKeys(t, CapacityCategoryOption{}), sortedKeys("category", "availableQuestionCount"))
	assertKeys(t, jsonKeys(t, CapacityFollowUpOption{}), sortedKeys(
		"followUpCount", "availableQuestionCount", "selectable"))
}

// ===== 创建场次 =====

// TestCreateKnowledgeBaseInterview契约 成功创建：响应与 Java InterviewSessionDTO
// 逐字（sessionId/resumeText=""/totalQuestions/currentQuestionIndex/questions/status/
// knowledgeBaseId/interviewCategory），题目携带参考答案/要点/评分规则/来源上下文，
// 接缝透传 skillId=knowledge-base + sourceType 语义参数。
func TestCreateKnowledgeBaseInterview契约(t *testing.T) {
	h := newInterviewFixture(t)
	kbID := seedVectorizedKB(t, h)
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Question = "什么是 JVM 内存模型？"
		q.Category = stringPtr("Java")
		q.ReferenceAnswer = stringPtr("JMM 定义了线程与主内存的交互规则")
		q.KeyPointsJSON = stringPtr(`["可见性","有序性"]`)
		q.ScoringRubric = stringPtr("答出 happe-before 给分")
		q.SourceContext = stringPtr("片段：JVM 章节……")
		q.FollowUpsJSON = followUpsJSON(2)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Question = "GC 分代收集的依据是什么？"
		q.Category = stringPtr("Java")
		q.FollowUpsJSON = followUpsJSON(2)
	})

	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":`+itoa(kbID)+`,"category":"Java","difficulty":"mid","mainQuestionCount":2,"followUpCount":1,"llmProvider":"dashscope"}`))
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	body := dataMap(t, data)
	if body["sessionId"] != "fixed0123456789ab" {
		t.Errorf("sessionId = %v", body["sessionId"])
	}
	if body["resumeText"] != "" {
		t.Errorf("resumeText = %v, want 空串", body["resumeText"])
	}
	if body["totalQuestions"] != float64(4) || body["currentQuestionIndex"] != float64(0) {
		t.Errorf("totalQuestions/currentQuestionIndex = %v/%v, want 4/0", body["totalQuestions"], body["currentQuestionIndex"])
	}
	if body["status"] != "CREATED" {
		t.Errorf("status = %v, want CREATED", body["status"])
	}
	if body["knowledgeBaseId"] != float64(kbID) {
		t.Errorf("knowledgeBaseId = %v, want %d", body["knowledgeBaseId"], kbID)
	}
	if body["interviewCategory"] != "Java" {
		t.Errorf("interviewCategory = %v, want Java", body["interviewCategory"])
	}

	questions, ok := body["questions"].([]any)
	if !ok || len(questions) != 4 {
		t.Fatalf("questions = %v, want 4 条（2 主问题各带 1 追问）", body["questions"])
	}
	// 主问题（index 0）：参考基线四字段接真值。
	main := questions[0].(map[string]any)
	if main["questionIndex"] != float64(0) || main["isFollowUp"] != false {
		t.Errorf("主问题 questionIndex/isFollowUp = %v/%v", main["questionIndex"], main["isFollowUp"])
	}
	if main["referenceAnswer"] != "JMM 定义了线程与主内存的交互规则" {
		t.Errorf("referenceAnswer = %v", main["referenceAnswer"])
	}
	if keys, ok := main["keyPoints"].([]any); !ok || len(keys) != 2 || keys[0] != "可见性" {
		t.Errorf("keyPoints = %v", main["keyPoints"])
	}
	if main["scoringRubric"] != "答出 happe-before 给分" || main["sourceContext"] != "片段：JVM 章节……" {
		t.Errorf("scoringRubric/sourceContext = %v/%v", main["scoringRubric"], main["sourceContext"])
	}
	// 追问（index 1）：isFollowUp/parentQuestionIndex 指向主问题，参考基线来自追问池。
	followUp := questions[1].(map[string]any)
	if followUp["questionIndex"] != float64(1) || followUp["isFollowUp"] != true || followUp["parentQuestionIndex"] != float64(0) {
		t.Errorf("追问 questionIndex/isFollowUp/parentQuestionIndex = %v/%v/%v",
			followUp["questionIndex"], followUp["isFollowUp"], followUp["parentQuestionIndex"])
	}
	if followUp["referenceAnswer"] != "追问参考答案 1" || followUp["sourceContext"] != "片段：JVM 章节……" {
		t.Errorf("追问 referenceAnswer/sourceContext = %v/%v", followUp["referenceAnswer"], followUp["sourceContext"])
	}
	// 追问分类沿用实体方向原值（Java defaultString(entity.getCategory(), "知识库追问")，
	// 方向非空时不回退缺省）。
	if followUp["category"] != "Java" {
		t.Errorf("追问 category = %v, want Java", followUp["category"])
	}

	// 接缝透传：skillId 固定 knowledge-base、difficulty/llmProvider/category 透传。
	if h.sessions.skillID == nil || *h.sessions.skillID != "knowledge-base" {
		t.Errorf("接缝 skillID = %v, want knowledge-base", h.sessions.skillID)
	}
	if h.sessions.difficulty == nil || *h.sessions.difficulty != "mid" {
		t.Errorf("接缝 difficulty = %v, want mid", h.sessions.difficulty)
	}
	if h.sessions.llmProvider == nil || *h.sessions.llmProvider != "dashscope" {
		t.Errorf("接缝 llmProvider = %v, want dashscope", h.sessions.llmProvider)
	}
	if h.sessions.knowledgeBaseID != kbID || h.sessions.interviewCategory == nil || *h.sessions.interviewCategory != "Java" {
		t.Errorf("接缝 knowledgeBaseID/interviewCategory = %d/%v", h.sessions.knowledgeBaseID, h.sessions.interviewCategory)
	}
}

// TestCreateKnowledgeBaseInterview题目不足3009 缺口明细消息逐字
// （方向为空 → 「全部方向」；有方向 → 原值）。
func TestCreateKnowledgeBaseInterview题目不足3009(t *testing.T) {
	h := newInterviewFixture(t)
	kbID := seedVectorizedKB(t, h)
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) { q.FollowUpsJSON = followUpsJSON(2) })

	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":`+itoa(kbID)+`,"mainQuestionCount":3,"followUpCount":1}`))
	code, message, _ := h.resultRaw(t)
	if code != 3009 {
		t.Fatalf("code = %d, want 3009", code)
	}
	if message != "需要 3 道主问题，但只有 1 道同时满足：方向=全部方向、难度=mid、每题至少 1 个追问" {
		t.Fatalf("message = %q", message)
	}
	if h.sessions.callCount() != 0 {
		t.Errorf("不足时不应触达场次接缝，calls = %d", h.sessions.callCount())
	}

	// 带方向 + 追问档位提升后的缺口（追问池不足同样计入）。
	h.recorder = httptest.NewRecorder()
	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":`+itoa(kbID)+`,"category":"Java","difficulty":"senior","mainQuestionCount":2,"followUpCount":3}`))
	_, message, _ = h.resultRaw(t)
	if message != "需要 2 道主问题，但只有 0 道同时满足：方向=Java、难度=senior、每题至少 3 个追问" {
		t.Fatalf("message = %q", message)
	}
}

// TestCreateKnowledgeBaseInterview追问池解析过滤 空白题干追问被剔除后才计入追问池
// （Java readUsableFollowUs 过滤 + trim 语义）：过滤后池不足 → 3009 缺口明细；
// 恰好满足时成功且追问题干已 trim。
func TestCreateKnowledgeBaseInterview追问池解析过滤(t *testing.T) {
	h := newInterviewFixture(t)
	kbID := seedVectorizedKB(t, h)
	// 追问池 3 条，其中 1 条空白题干被过滤 → 可用 2 条；followUpCount=3 时不足 → 3009。
	q := &KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: kbID, SkillID: QuestionSkillID, Difficulty: stringPtr("mid"),
		Category: stringPtr("Java"), Question: "主问题", Status: QuestionActive,
		FollowUpsJSON: stringPtr(`[{"question":"  ","referenceAnswer":"x"},{"question":"追问1"},{"question":"追问2"}]`),
	}
	h.f.store.addQuestion(q)

	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":`+itoa(kbID)+`,"mainQuestionCount":1,"followUpCount":3}`))
	code, message, _ := h.resultRaw(t)
	if code != 3009 {
		t.Fatalf("code = %d, want 3009", code)
	}
	if message != "需要 1 道主问题，但只有 0 道同时满足：方向=全部方向、难度=mid、每题至少 3 个追问" {
		t.Fatalf("message = %q", message)
	}

	// 追问池恰为 2（空白过滤后）+ followUpCount=2 → 成功且题目带追问。
	h.recorder = httptest.NewRecorder()
	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":`+itoa(kbID)+`,"mainQuestionCount":1,"followUpCount":2}`))
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	body := dataMap(t, data)
	questions := body["questions"].([]any)
	if len(questions) != 3 {
		t.Fatalf("questions = %d 条, want 3（1 主 + 2 追问）", len(questions))
	}
	if got := questions[1].(map[string]any)["question"]; got != "追问1" && got != "追问2" && got != "追问3" {
		t.Errorf("追问题干未 trim/过滤异常: %v", got)
	}
}

// TestCreateKnowledgeBaseInterview知识库不存在6001.
func TestCreateKnowledgeBaseInterview知识库不存在6001(t *testing.T) {
	h := newInterviewFixture(t)
	h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions",
		[]byte(`{"knowledgeBaseId":999,"mainQuestionCount":1,"followUpCount":0}`))
	code, message, _ := h.resultRaw(t)
	if code != 6001 || message != "知识库不存在" {
		t.Fatalf("code/message = [%d %q], want [6001 知识库不存在]", code, message)
	}
}

// TestCreateKnowledgeBaseInterview校验消息逐字 @Valid 注解消息与字段声明序。
func TestCreateKnowledgeBaseInterview校验消息逐字(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 knowledgeBaseId", `{"mainQuestionCount":1,"followUpCount":0}`, "知识库不能为空"},
		{"主问题数缺失（缺省 0）", `{"knowledgeBaseId":1}`, "主问题数量最少1题"},
		{"主问题数下界", `{"knowledgeBaseId":1,"mainQuestionCount":0}`, "主问题数量最少1题"},
		{"主问题数上界", `{"knowledgeBaseId":1,"mainQuestionCount":21}`, "主问题数量最多20题"},
		{"追问数下界", `{"knowledgeBaseId":1,"mainQuestionCount":1,"followUpCount":-1}`, "追问数量不能小于0"},
		{"追问数上界", `{"knowledgeBaseId":1,"mainQuestionCount":1,"followUpCount":6}`, "每题追问最多5个"},
		{"多违规按声明序连接", `{"mainQuestionCount":21,"followUpCount":6}`, "知识库不能为空, 主问题数量最多20题, 每题追问最多5个"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newInterviewFixture(t)
			h.doMethod(t, http.MethodPost, "/api/knowledgebase-interviews/sessions", []byte(tc.body))
			code, message, _ := h.resultRaw(t)
			if code != 400 || message != tc.want {
				t.Fatalf("code/message = [%d %q], want [400 %q]", code, message, tc.want)
			}
		})
	}
}

// ===== 容量 =====

// TestInterviewCapacity契约与默认值 无 query 参数时 difficulty 默认 mid、
// mainQuestionCount 默认 5；categories 按可用题数降序、同数按方向名字典序；
// followUpOptions 覆盖 0-5 档、selectable = 可用量 ≥ 主问题数。
func TestInterviewCapacity契约与默认值(t *testing.T) {
	h := newInterviewFixture(t)
	kbID := seedVectorizedKB(t, h)
	// Java：3 题、Redis：1 题（mid）；Java 3 追问池、Redis 2 追问池。
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.FollowUpsJSON = followUpsJSON(3)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.FollowUpsJSON = followUpsJSON(3)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.FollowUpsJSON = followUpsJSON(3)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Redis")
		q.FollowUpsJSON = followUpsJSON(2)
	})
	// DRAFT 与 difficulty 不符的题不计入。
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) { q.Category = stringPtr("Java"); q.Status = QuestionDraft })
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.Difficulty = stringPtr("senior")
	})

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(kbID)+"/interview-capacity", nil)
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	body := dataMap(t, data)
	if body["knowledgeBaseId"] != float64(kbID) {
		t.Errorf("knowledgeBaseId = %v", body["knowledgeBaseId"])
	}
	if v, exists := body["category"]; !exists || v != nil {
		t.Errorf("category = %v, want JSON null（未指定方向）", v)
	}
	if body["difficulty"] != "mid" {
		t.Errorf("difficulty = %v, want 默认 mid", body["difficulty"])
	}
	if body["mainQuestionCount"] != float64(5) {
		t.Errorf("mainQuestionCount = %v, want 默认 5", body["mainQuestionCount"])
	}

	categories := body["categories"].([]any)
	if len(categories) != 2 {
		t.Fatalf("categories = %v, want 2 项", categories)
	}
	first := categories[0].(map[string]any)
	second := categories[1].(map[string]any)
	if first["category"] != "Java" || first["availableQuestionCount"] != float64(3) {
		t.Errorf("categories[0] = %v, want Java/3", first)
	}
	if second["category"] != "Redis" || second["availableQuestionCount"] != float64(1) {
		t.Errorf("categories[1] = %v, want Redis/1", second)
	}

	options := body["followUpOptions"].([]any)
	if len(options) != 6 {
		t.Fatalf("followUpOptions = %d 项, want 6（0-5）", len(options))
	}
	wantAvailable := []float64{4, 4, 4, 3, 0, 0} // 追问池 ≥ n 的题数（池 3/3/3/2；scoped 未指定方向=全部）
	for i, item := range options {
		opt := item.(map[string]any)
		if opt["followUpCount"] != float64(i) || opt["availableQuestionCount"] != wantAvailable[i] {
			t.Errorf("followUpOptions[%d] = %v, want count=%d available=%v", i, opt, i, wantAvailable[i])
		}
		wantSelectable := wantAvailable[i] >= 5
		if opt["selectable"] != wantSelectable {
			t.Errorf("followUpOptions[%d].selectable = %v, want %v（默认主问题数 5）", i, opt["selectable"], wantSelectable)
		}
	}
}

// TestInterviewCapacity方向过滤与显式参数 category 过滤只收窄追问档位可用量，
// categories 恒为全方向分布；category 空白归 null、difficulty/mainQuestionCount 显式生效。
func TestInterviewCapacity方向过滤与显式参数(t *testing.T) {
	h := newInterviewFixture(t)
	kbID := seedVectorizedKB(t, h)
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.Difficulty = stringPtr("senior")
		q.FollowUpsJSON = followUpsJSON(2)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Java")
		q.Difficulty = stringPtr("senior")
		q.FollowUpsJSON = followUpsJSON(2)
	})
	seedQuestion(t, h, kbID, func(q *KnowledgeBaseQuestionEntity) {
		q.Category = stringPtr("Redis")
		q.Difficulty = stringPtr("senior")
		q.FollowUpsJSON = followUpsJSON(4)
	})

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(kbID)+"/interview-capacity?category=Redis&difficulty=senior&mainQuestionCount=2", nil)
	_, message, data := h.resultRaw(t)
	if message != "success" {
		t.Fatalf("message = %q", message)
	}
	body := dataMap(t, data)
	if body["category"] != "Redis" || body["difficulty"] != "senior" || body["mainQuestionCount"] != float64(2) {
		t.Fatalf("显式参数透传不符: %v", body)
	}
	categories := body["categories"].([]any)
	if len(categories) != 2 {
		t.Fatalf("categories 应为全方向分布: %v", categories)
	}
	for i, item := range body["followUpOptions"].([]any) {
		opt := item.(map[string]any)
		want := float64(1) // Redis 1 题，追问池 4 全档满足
		if i > 4 {
			want = 0
		}
		if opt["availableQuestionCount"] != want {
			t.Errorf("followUpOptions[%d].availableQuestionCount = %v, want %v", i, opt["availableQuestionCount"], want)
		}
		if opt["selectable"] != (want >= 2) {
			t.Errorf("followUpOptions[%d].selectable = %v（主问题数 2）", i, opt["selectable"])
		}
	}

	// 空白 category 归 null。
	h.recorder = httptest.NewRecorder()
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(kbID)+"/interview-capacity?category=%20", nil)
	_, _, data = h.resultRaw(t)
	body = dataMap(t, data)
	if v, exists := body["category"]; !exists || v != nil {
		t.Errorf("空白 category = %v, want JSON null", v)
	}
}

// TestInterviewCapacity主问题数越界500 mainQuestionCount 越界/非法在 Java 为
// HandlerMethodValidationException/类型转换失败 → 全局兜底 500「系统繁忙，请稍后重试」。
func TestInterviewCapacity主问题数越界500(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"低于下界", "/api/knowledgebase/1/interview-capacity?mainQuestionCount=0"},
		{"高于上界", "/api/knowledgebase/1/interview-capacity?mainQuestionCount=21"},
		{"非整数", "/api/knowledgebase/1/interview-capacity?mainQuestionCount=abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newInterviewFixture(t)
			h.doMethod(t, http.MethodGet, tc.url, nil)
			code, message, _ := h.resultRaw(t)
			if code != 500 || message != "系统繁忙，请稍后重试" {
				t.Fatalf("code/message = [%d %q], want [500 系统繁忙，请稍后重试]", code, message)
			}
		})
	}
}

// TestInterviewCapacity知识库不存在6001.
func TestInterviewCapacity知识库不存在6001(t *testing.T) {
	h := newInterviewFixture(t)
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/999/interview-capacity", nil)
	code, message, _ := h.resultRaw(t)
	if code != 6001 || message != "知识库不存在" {
		t.Fatalf("code/message = [%d %q], want [6001 知识库不存在]", code, message)
	}
}
