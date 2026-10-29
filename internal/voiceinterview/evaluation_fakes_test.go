// 语音评估链路单测的内存/脚本化依赖（先例 internal/interview/evaluation_fakes_test.go）：
// 内存 EvalStore、全功能 Stream 替身、脚本化 EvaluationRunner、参考段/明细替身。
// spec 测试决策：只测外部行为，不 mock 内部步骤、不断言实现细节。
package voiceinterview

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"time"

	"github.com/wuzuxian/interview/internal/interview"
)

// ===== 内存 EvalStore（语音评估链路数据面）=====

type requeueCall struct {
	id int64
	at time.Time
}

type staleQuery struct {
	status VoiceEvaluateStatus
	before time.Time
}

type fakeVoiceEvalStore struct {
	rec *svcRecorder

	mu          sync.Mutex
	sessions    map[int64]*SessionEntity
	dialogues   map[int64][]MessageEntity
	evaluations map[int64]*EvaluationEntity
	evalUpdates []evalUpdate
	saves       []*EvaluationEntity
	emptySaves  []*EvaluationEntity
	requeues    []requeueCall
	staleCalls  []staleQuery
	saveErr     error
	// findFailAfter 前 N 次 FindSessionByID 正常返回，其后返回 ErrSessionNotFound
	//（模拟"跳过检查后、业务执行前会话被删除"的窗口）。
	findFailAfter int
	findCalls     int
	// findErr 非 nil 时 FindSessionByID 恒返回该错误（模拟 DB 故障等非「会话不存在」
	// 的读错误；区别于 findFailAfter 的 ErrSessionNotFound）。
	findErr error
}

func newFakeVoiceEvalStore(rec *svcRecorder) *fakeVoiceEvalStore {
	return &fakeVoiceEvalStore{
		rec:         rec,
		sessions:    map[int64]*SessionEntity{},
		dialogues:   map[int64][]MessageEntity{},
		evaluations: map[int64]*EvaluationEntity{},
	}
}

func (s *fakeVoiceEvalStore) addSession(e *SessionEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[e.ID] = e
}

func (s *fakeVoiceEvalStore) setDialogues(sessionID int64, msgs []MessageEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dialogues[sessionID] = msgs
}

func (s *fakeVoiceEvalStore) FindSessionByID(ctx context.Context, sessionID int64) (*SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findCalls++
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.findFailAfter > 0 && s.findCalls > s.findFailAfter {
		return nil, ErrSessionNotFound
	}
	e, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return e, nil
}

func (s *fakeVoiceEvalStore) FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("find-dialogues")
	return s.dialogues[sessionID], nil
}

func (s *fakeVoiceEvalStore) UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-eval:" + string(status))
	s.evalUpdates = append(s.evalUpdates, evalUpdate{sessionID, status, evalErr})
	if e, ok := s.sessions[sessionID]; ok {
		e.EvaluateStatus = &status
		e.EvaluateError = evalErr
	}
	return nil
}

func (s *fakeVoiceEvalStore) FindEvaluationBySessionID(ctx context.Context, sessionID int64) (*EvaluationEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.evaluations[sessionID]
	if !ok {
		return nil, ErrEvaluationNotFound
	}
	cp := *e
	return &cp, nil
}

func (s *fakeVoiceEvalStore) SaveEvaluation(ctx context.Context, e *EvaluationEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("save-eval")
	if s.saveErr != nil {
		return s.saveErr
	}
	cp := *e
	s.saves = append(s.saves, &cp)
	s.evaluations[*e.SessionID] = &cp
	return nil
}

func (s *fakeVoiceEvalStore) SaveEmptyEvaluation(ctx context.Context, e *EvaluationEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("save-empty-eval")
	if s.saveErr != nil {
		return s.saveErr
	}
	cp := *e
	s.emptySaves = append(s.emptySaves, &cp)
	s.evaluations[*e.SessionID] = &cp
	return nil
}

func (s *fakeVoiceEvalStore) FindStaleEvaluateSessions(ctx context.Context, status VoiceEvaluateStatus, before time.Time) ([]SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleCalls = append(s.staleCalls, staleQuery{status, before})
	var out []SessionEntity
	for _, e := range s.sessions {
		if e.EvaluateStatus == nil || *e.EvaluateStatus != status {
			continue
		}
		if e.UpdatedAt == nil || !e.UpdatedAt.Before(before) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *fakeVoiceEvalStore) UpdateEvaluateRequeue(ctx context.Context, sessionID int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("requeue")
	s.requeues = append(s.requeues, requeueCall{sessionID, at})
	if e, ok := s.sessions[sessionID]; ok {
		e.EvaluateError = nil
		e.UpdatedAt = &at
	}
	return nil
}

// ===== 替身：全功能评估流（消费者消费面）=====

type fakeVoiceEvalStream struct {
	rec    *svcRecorder
	mu     sync.Mutex
	adds   []map[string]any
	addErr error
	acked  []string
}

func (s *fakeVoiceEvalStream) EnsureGroup(ctx context.Context) error { return nil }

func (s *fakeVoiceEvalStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("xadd")
	if s.addErr != nil {
		return "", s.addErr
	}
	s.adds = append(s.adds, values)
	return "1-0", nil
}

func (s *fakeVoiceEvalStream) XAck(ctx context.Context, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("ack")
	s.acked = append(s.acked, messageID)
	return nil
}

func (s *fakeVoiceEvalStream) XReadNew(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error) {
	return nil, nil
}

func (s *fakeVoiceEvalStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error) {
	return nil, nil
}

func (s *fakeVoiceEvalStream) addAt(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adds[i]
}

func (s *fakeVoiceEvalStream) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

// ===== 替身：脚本化评估执行（interview.EvaluationRunner）=====

type stubVoiceRunner struct {
	mu           sync.Mutex
	report       *interview.ReportDTO
	err          error
	calls        int
	lastInput    interview.EvaluateInput
	lastProvider *string
}

func (r *stubVoiceRunner) Evaluate(ctx context.Context, input interview.EvaluateInput, providerID *string) (*interview.ReportDTO, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastInput = input
	r.lastProvider = providerID
	if r.err != nil {
		return nil, r.err
	}
	return r.report, nil
}

// ===== 替身：评估参考段来源（interview.ReferenceSource）=====

type fakeVoiceRefs struct {
	text      string
	lastSkill string
}

func (f *fakeVoiceRefs) BuildEvaluationReferenceSectionSafe(skillID string) string {
	f.lastSkill = skillID
	return f.text
}

// ===== 替身：评估明细读取（EvaluationDetailSource）=====

type stubEvalDetail struct {
	detail *VoiceEvaluationDetailDTO
	err    error
	calls  int
}

func (s *stubEvalDetail) GetEvaluationDetail(ctx context.Context, sessionID int64) (*VoiceEvaluationDetailDTO, error) {
	s.calls++
	return s.detail, s.err
}

// ===== 共享小工具 =====

// int64Ptr 指针构造（评估行 session_id）。
func int64Ptr(v int64) *int64 { return &v }

// evalChainSession 构造一行已结束的场次实体（评估链路前提：COMPLETED + 评估状态可变）。
func evalChainSession(id int64, evalStatus *VoiceEvaluateStatus) *SessionEntity {
	status := VoiceStatusCompleted
	skill := "java-backend"
	role := "java-backend"
	llm := "test-provider"
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	return &SessionEntity{
		ID:             id,
		RoleType:       role,
		SkillID:        &skill,
		LLMProvider:    &llm,
		Status:         &status,
		StartTime:      &start,
		UpdatedAt:      &updated,
		EvaluateStatus: evalStatus,
	}
}

// voiceMsg 构造一行 DIALOGUE 消息；ai/user 传 nil 表示该侧文本缺失。
func voiceMsg(sessionID int64, seq int, ai, user *string) MessageEntity {
	return MessageEntity{
		SessionID:          sessionID,
		MessageType:        MessageDialogue,
		SequenceNum:        &seq,
		AiGeneratedText:    ai,
		UserRecognizedText: user,
	}
}

// sampleVoiceReport 两题报告：第一题已答带参考，第二题未答。
func sampleVoiceReport(sessionID int64) *interview.ReportDTO {
	answer := "我的回答"
	return &interview.ReportDTO{
		SessionID:      strconv.FormatInt(sessionID, 10),
		TotalQuestions: 2,
		OverallScore:   72,
		QuestionDetails: []interview.QuestionEvaluationDTO{
			{QuestionIndex: 0, Question: "介绍一下你的项目", Category: "项目深挖", UserAnswer: &answer, Score: 80, Feedback: "反馈一"},
			{QuestionIndex: 1, Question: "讲讲索引原理", Category: "技术问题", UserAnswer: nil, Score: 64, Feedback: "反馈二"},
		},
		OverallFeedback: "总体反馈",
		Strengths:       []string{"基础扎实"},
		Improvements:    []string{"多加练习"},
		ReferenceAnswers: []interview.ReferenceAnswerDTO{
			{QuestionIndex: 0, Question: "介绍一下你的项目", ReferenceAnswer: "参考一", KeyPoints: []string{"要点一"}},
			{QuestionIndex: 1, Question: "讲讲索引原理", ReferenceAnswer: "参考二", KeyPoints: []string{}},
		},
	}
}

// evalChainFixture 评估服务夹具（store + 脚本 runner + 参考段）。
type evalChainFixture struct {
	rec    *svcRecorder
	store  *fakeVoiceEvalStore
	stream *fakeVoiceEvalStream
	runner *stubVoiceRunner
	refs   *fakeVoiceRefs
	svc    *EvaluationService
}

func newEvalChainFixture() *evalChainFixture {
	rec := &svcRecorder{}
	store := newFakeVoiceEvalStore(rec)
	stream := &fakeVoiceEvalStream{rec: rec}
	runner := &stubVoiceRunner{}
	refs := &fakeVoiceRefs{text: "【方向参考】参考基线内容"}
	svc := NewEvaluationService(EvaluationServiceDeps{
		Store:  store,
		Eval:   runner,
		Refs:   refs,
		Logger: discardLogger(),
	})
	return &evalChainFixture{rec: rec, store: store, stream: stream, runner: runner, refs: refs, svc: svc}
}

var errEvalBoom = errors.New("boom")

// ===== fakeStore 的票 08 扩展（SessionStore 新增恢复巡检两方法）=====

func (s *fakeStore) FindStaleEvaluateSessions(ctx context.Context, status VoiceEvaluateStatus, before time.Time) ([]SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("stale-find")
	var out []SessionEntity
	for _, e := range s.sessions {
		if e.EvaluateStatus == nil || *e.EvaluateStatus != status {
			continue
		}
		if e.UpdatedAt == nil || !e.UpdatedAt.Before(before) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *fakeStore) UpdateEvaluateRequeue(ctx context.Context, sessionID int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("requeue")
	if e, ok := s.sessions[sessionID]; ok {
		e.EvaluateError = nil
		e.UpdatedAt = &at
	}
	return nil
}

// FindStaleInProgressSessions 票 09 僵尸清理数据源（内存表镜像 Java
// findByStatusAndStartTimeBefore：状态 IN_PROGRESS 且 start_time 严格早于 before）。
func (s *fakeStore) FindStaleInProgressSessions(ctx context.Context, before time.Time) ([]SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("stale-inprogress-find")
	var out []SessionEntity
	for _, e := range s.sessions {
		if statusEnum(e.Status) != VoiceStatusInProgress {
			continue
		}
		if e.StartTime == nil || !e.StartTime.Before(before) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}
