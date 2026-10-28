// 评估链路单测的内存/脚本化依赖（spec 测试决策：stub 引擎 + 内存依赖，
// 不 mock 内部步骤、不断言实现细节，只测外部行为契约）。
package interview

import (
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

// evalTestRecorder 共享事件日志（跨 fake 记录顺序，验证"先置 PENDING 再 XADD"等顺序契约）。
type evalTestRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *evalTestRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *evalTestRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// eventBefore 断言事件 a 先于事件 b 发生。
func eventBefore(events []string, a, b string) bool {
	ia, ib := -1, -1
	for i, e := range events {
		if e == a && ia < 0 {
			ia = i
		}
		if e == b && ib < 0 {
			ib = i
		}
	}
	return ia >= 0 && ib >= 0 && ia < ib
}

// ===== 内存 EvalStore =====

type evalStatusUpdate struct {
	SessionID string
	Status    EvaluateStatus
	Error     *string
}

type fakeEvalStore struct {
	rec *evalTestRecorder

	mu            sync.Mutex
	sessions      map[string]*SessionEntity
	answers       map[int64][]AnswerEntity
	resumes       map[int64]string
	evalUpdates   []evalStatusUpdate
	reportWrites  []map[string]any
	answerWrites  []answerEvalWrite
	failEvalUpd   error // UpdateEvaluateStatus 注入失败
	failReportCol error // UpdateReportColumns 注入失败
}

func newFakeEvalStore(rec *evalTestRecorder) *fakeEvalStore {
	return &fakeEvalStore{
		rec:      rec,
		sessions: map[string]*SessionEntity{},
		answers:  map[int64][]AnswerEntity{},
		resumes:  map[int64]string{},
	}
}

func (s *fakeEvalStore) addSession(e *SessionEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[e.SessionID] = e
}

func (s *fakeEvalStore) setAnswers(rowID int64, answers []AnswerEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[rowID] = answers
}

func (s *fakeEvalStore) FindByID(ctx context.Context, id int64) (*SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.sessions {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *fakeEvalStore) FindBySessionID(ctx context.Context, sessionID string) (*SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[sessionID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return e, nil
}

func (s *fakeEvalStore) AnswersBySessionID(ctx context.Context, sessionRowID int64) ([]AnswerEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answers[sessionRowID], nil
}

func (s *fakeEvalStore) ResumeTextByID(ctx context.Context, resumeID int64) (*string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.resumes[resumeID]
	if !ok {
		return nil, nil
	}
	return &text, nil
}

func (s *fakeEvalStore) UpdateEvaluateStatus(ctx context.Context, sessionID string, status EvaluateStatus, evalError *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEvalUpd != nil {
		return s.failEvalUpd
	}
	s.evalUpdates = append(s.evalUpdates, evalStatusUpdate{SessionID: sessionID, Status: status, Error: evalError})
	s.rec.add("evaluate:" + string(status))
	if e, ok := s.sessions[sessionID]; ok {
		e.EvaluateStatus = &status
		e.EvaluateError = evalError
	}
	return nil
}

func (s *fakeEvalStore) UpdateStatus(ctx context.Context, sessionID string, status SessionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.sessions[sessionID]; ok {
		e.Status = &status
	}
	return nil
}

func (s *fakeEvalStore) UpdateReportColumns(ctx context.Context, sessionID string, columns map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failReportCol != nil {
		return s.failReportCol
	}
	s.reportWrites = append(s.reportWrites, columns)
	s.rec.add("report-columns")
	return nil
}

func (s *fakeEvalStore) UpsertAnswerEval(ctx context.Context, w answerEvalWrite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answerWrites = append(s.answerWrites, w)
	s.rec.add("answer-upsert")
	return nil
}

// ===== 脚本化 Stream =====

type fakeEvalStream struct {
	rec *evalTestRecorder

	mu     sync.Mutex
	adds   []map[string]any
	addErr error
	acked  []string
}

func newFakeEvalStream(rec *evalTestRecorder) *fakeEvalStream {
	return &fakeEvalStream{rec: rec}
}

func (s *fakeEvalStream) EnsureGroup(ctx context.Context) error { return nil }

func (s *fakeEvalStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("xadd")
	if s.addErr != nil {
		return "", s.addErr
	}
	s.adds = append(s.adds, values)
	return "1-0", nil
}

func (s *fakeEvalStream) XAck(ctx context.Context, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("ack")
	s.acked = append(s.acked, messageID)
	return nil
}

func (s *fakeEvalStream) XReadNew(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error) {
	return nil, nil
}

func (s *fakeEvalStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error) {
	return nil, nil
}

func (s *fakeEvalStream) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

func (s *fakeEvalStream) addAt(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adds[i]
}

// ===== stub 引擎 =====

type stubEvalRunner struct {
	mu          sync.Mutex
	report      *ReportDTO
	err         error
	calls       int
	lastInput   EvaluateInput
	lastProvide *string
}

func (r *stubEvalRunner) Evaluate(ctx context.Context, input EvaluateInput, providerID *string) (*ReportDTO, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastInput = input
	r.lastProvide = providerID
	if r.err != nil {
		return nil, r.err
	}
	return r.report, nil
}

func (r *stubEvalRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ===== 参考段 / 缓存状态替身 =====

type fakeRefSource struct {
	text      string
	lastSkill string
}

func (f *fakeRefSource) BuildEvaluationReferenceSectionSafe(skillID string) string {
	f.lastSkill = skillID
	return f.text
}

type fakeStatusUpdater struct {
	mu    sync.Mutex
	calls []SessionStatus
	err   error
}

func (f *fakeStatusUpdater) UpdateSessionStatus(ctx context.Context, sessionID string, status SessionStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, status)
	return f.err
}

// ===== 共享小工具 =====

// evalTestSession 构造一行场次实体。
func evalTestSession(id int64, sessionID string, status SessionStatus, evaluate *EvaluateStatus) *SessionEntity {
	total := 2
	llm := "test-provider"
	skill := "java-backend"
	resumeID := int64(7)
	return &SessionEntity{
		ID:             id,
		SessionID:      sessionID,
		SkillID:        &skill,
		ResumeID:       &resumeID,
		TotalQuestions: &total,
		Status:         &status,
		LLMProvider:    &llm,
		EvaluateStatus: evaluate,
		CreatedAt:      time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC),
	}
}

// evalSampleQuestions 两道题（第二题未回答）；Category 为分类 key（JAVA/MYSQL）。
func evalSampleQuestions() []QuestionDTO {
	q1 := CreateFallback(0, "题目一", "java", "JAVA", nil, false, nil)
	q2 := CreateFallback(1, "题目二", "mysql", "MYSQL", nil, false, nil)
	return []QuestionDTO{q1, q2}
}

// evalSampleQuestionsJSON 序列化题目。
func evalSampleQuestionsJSON(t *testing.T) string {
	t.Helper()
	raw, err := SerializeQuestions(evalSampleQuestions())
	if err != nil {
		t.Fatalf("序列化题目失败: %v", err)
	}
	return raw
}

// evalSampleReport 构造一份报告（与 evalSampleQuestions 对应）。
func evalSampleReport() *ReportDTO {
	return &ReportDTO{
		SessionID:      "s1",
		TotalQuestions: 2,
		OverallScore:   70,
		QuestionDetails: []QuestionEvaluationDTO{
			{QuestionIndex: 0, Question: "题目一", Category: "JAVA", UserAnswer: stringPtr("A1"), Score: 80, Feedback: "不错"},
			{QuestionIndex: 1, Question: "题目二", Category: "MYSQL", UserAnswer: nil, Score: 60, Feedback: "一般"},
		},
		OverallFeedback: "综合评价",
		Strengths:       []string{"优势1"},
		Improvements:    []string{"改进1"},
		ReferenceAnswers: []ReferenceAnswerDTO{
			{QuestionIndex: 0, Question: "题目一", ReferenceAnswer: "参考1", KeyPoints: []string{"要点1", "要点2"}},
			{QuestionIndex: 1, Question: "题目二", ReferenceAnswer: "参考2", KeyPoints: []string{}},
		},
	}
}
