// 「从题目集合创建场次」入口单测（票 05）：实体组装默认值、DTO 形状、空题目守卫
// 与跨模块接缝。落库/缓存链路由知识库面试 HTTP 契约测试（fake 接缝）与 cmd/smoke
// 知识库模式真库兜底（spec 测试决策 3/5）。
package interview

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 契约：实体组装默认值与 Java saveSessionInternal 三元表达式逐字——
// llmProvider nil→"default"、skillID nil→java-backend、difficulty nil→mid
// （只判 nil 不判空白）；sourceType 恒 KNOWLEDGE_BASE；requestId/resumeId 为 NULL。
func TestKBSessionEntityDefaults(t *testing.T) {
	cases := []struct {
		name           string
		llmProvider    *string
		skillID        *string
		difficulty     *string
		wantProvider   string
		wantSkill      string
		wantDifficulty string
	}{
		{"全空缺省走默认值", nil, nil, nil, "default", DefaultSkillID, DefaultDifficulty},
		{"显式值原样落列", stringPtr("dashscope"), stringPtr("knowledge-base"), stringPtr("senior"), "dashscope", "knowledge-base", "senior"},
		{"空白串不回退默认（照抄 Java 三元）", stringPtr(""), stringPtr(""), stringPtr(""), "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			category := "Java"
			entity := kbSessionEntity("abc12345", "[]", 6, tc.llmProvider, tc.skillID, tc.difficulty,
				42, &category, time.Unix(1700000000, 0))
			if *entity.LLMProvider != tc.wantProvider {
				t.Errorf("llmProvider = %q, want %q", *entity.LLMProvider, tc.wantProvider)
			}
			if *entity.SkillID != tc.wantSkill {
				t.Errorf("skillID = %q, want %q", *entity.SkillID, tc.wantSkill)
			}
			if *entity.Difficulty != tc.wantDifficulty {
				t.Errorf("difficulty = %q, want %q", *entity.Difficulty, tc.wantDifficulty)
			}
			if entity.SourceType == nil || *entity.SourceType != SourceKnowledgeBase {
				t.Errorf("sourceType = %v, want KNOWLEDGE_BASE", entity.SourceType)
			}
			if entity.RequestID != nil || entity.ResumeID != nil {
				t.Errorf("requestId/resumeId 应为 NULL: %v/%v", entity.RequestID, entity.ResumeID)
			}
			if entity.KnowledgeBaseID == nil || *entity.KnowledgeBaseID != 42 {
				t.Errorf("knowledgeBaseId = %v, want 42", entity.KnowledgeBaseID)
			}
			if entity.InterviewCategory == nil || *entity.InterviewCategory != "Java" {
				t.Errorf("interviewCategory = %v, want Java", entity.InterviewCategory)
			}
			if entity.TotalQuestions == nil || *entity.TotalQuestions != 6 {
				t.Errorf("totalQuestions = %v, want 6", entity.TotalQuestions)
			}
			if entity.Status == nil || *entity.Status != StatusCreated {
				t.Errorf("status = %v, want CREATED", entity.Status)
			}
		})
	}
}

// 契约：题目集合为空 → 3003「面试题目不能为空」（Java BusinessException 逐字）。
func TestCreateSessionFromQuestionsEmptyGuard(t *testing.T) {
	svc := NewSessionService(SessionServiceDeps{Logger: discardTestLogger()})
	for name, questions := range map[string][]QuestionDTO{
		"nil 题目列表": nil,
		"空题目列表":    {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateSessionFromQuestions(context.Background(), questions,
				nil, nil, nil, 1, nil)
			ae, ok := err.(*apperr.AppError)
			if !ok {
				t.Fatalf("期望 AppError, got %T: %v", err, err)
			}
			if ae.Code != apperr.CodeInterviewQuestionNotFound || ae.Message != "面试题目不能为空" {
				t.Fatalf("code/message = [%d %s], want [3003 面试题目不能为空]", ae.Code, ae.Message)
			}
		})
	}
}

// 契约： *SessionService 满足「从题目集合创建场次」接缝（知识库面试依赖注入的编译期保障）。
var _ SessionFromQuestionsCreator = (*SessionService)(nil)

// 契约：SessionDTO（Java InterviewSessionDTO record 8 字段）JSON 键集黄金清单，
// 知识库路径响应与文字面试共用同一 DTO。
func TestKBSessionDTOContract(t *testing.T) {
	category := "Java"
	kbID := int64(7)
	dto := SessionDTO{
		SessionID: "abc12345", TotalQuestions: 2, CurrentQuestionIndex: 0,
		Questions: []QuestionDTO{}, Status: StatusCreated,
		KnowledgeBaseID: &kbID, InterviewCategory: &category,
	}
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	want := []string{"sessionId", "resumeText", "totalQuestions", "currentQuestionIndex",
		"questions", "status", "knowledgeBaseId", "interviewCategory"}
	if len(keys) != len(want) {
		t.Fatalf("键数 = %d, want %d: %v", len(keys), len(want), keys)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("缺少键 %q", k)
		}
	}
}
