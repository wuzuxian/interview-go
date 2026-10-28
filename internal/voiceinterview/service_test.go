// 会话域行为单测（spec 测试决策：只测外部行为，不起真 DB/Redis；接缝替身
// 参考 internal/interview 的 EvalStore/fakeEvalStream 做法）：
// 创建默认值、会话/缓存/评估流的读写路径与顺序、pause/resume 守卫、级联删除、
// 阶段切换、评估流消息字段、apperr 语音错误码。契约来源：Java
// VoiceInterviewService / VoiceEvaluateStreamProducer / ErrorCode 原文逐字。
package voiceinterview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ===== 共享事件记录器（断言跨替身的调用顺序，如 事务提交 → 失效缓存 → 投递）=====

type svcRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *svcRecorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *svcRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *svcRecorder) has(suffix string) bool {
	for _, e := range r.list() {
		if strings.HasSuffix(e, suffix) {
			return true
		}
	}
	return false
}

// ===== 替身：会话存储（内存表，CreateSession 镜像 Java @PrePersist 补时间戳并分配 ID）=====

type fakeStore struct {
	rec *svcRecorder

	mu           sync.Mutex
	sessions     map[int64]*SessionEntity
	nextID       int64
	dialogues    map[int64][]MessageEntity
	findErr      error
	endErr       error
	evalUpdErr   error
	updatePause  []pauseCall
	updateResume []resumeCall
	updateEnd    []*SessionEntity
	updatePhase  []phaseCall
	evalUpdates  []evalUpdate
	deleteOrder  []string // 级联删除顺序：eval / messages / session
	cascadeErr   error
}

type pauseCall struct {
	id       int64
	pausedAt time.Time
}

type resumeCall struct {
	id        int64
	resumedAt time.Time
}

type phaseCall struct {
	id    int64
	phase VoicePhase
}

type evalUpdate struct {
	id     int64
	status VoiceEvaluateStatus
	err    *string
}

func newFakeStore(rec *svcRecorder) *fakeStore {
	return &fakeStore{rec: rec, sessions: map[int64]*SessionEntity{}, nextID: 1, dialogues: map[int64][]MessageEntity{}}
}

func (s *fakeStore) CreateSession(ctx context.Context, e *SessionEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("create")
	now := time.Now()
	if e.CreatedAt == nil {
		e.CreatedAt = &now
	}
	if e.UpdatedAt == nil {
		e.UpdatedAt = &now
	}
	if e.StartTime == nil {
		e.StartTime = &now
	}
	e.ID = s.nextID
	s.nextID++
	s.sessions[e.ID] = e
	return nil
}

func (s *fakeStore) FindSessionByID(ctx context.Context, sessionID int64) (*SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("find")
	if s.findErr != nil {
		return nil, s.findErr
	}
	e, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return e, nil
}

func (s *fakeStore) UpdatePause(ctx context.Context, sessionID int64, pausedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-pause")
	s.updatePause = append(s.updatePause, pauseCall{sessionID, pausedAt})
	// 镜像真实 UPDATE（status='PAUSED' + paused_at），断开自动结束路径依赖该状态。
	if e, ok := s.sessions[sessionID]; ok {
		paused := VoiceStatusPaused
		e.Status = &paused
		e.PausedAt = &pausedAt
	}
	return nil
}

func (s *fakeStore) UpdateResume(ctx context.Context, sessionID int64, resumedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-resume")
	s.updateResume = append(s.updateResume, resumeCall{sessionID, resumedAt})
	return nil
}

func (s *fakeStore) UpdateEnd(ctx context.Context, e *SessionEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-end")
	if s.endErr != nil {
		return s.endErr
	}
	cp := *e
	s.updateEnd = append(s.updateEnd, &cp)
	return nil
}

func (s *fakeStore) UpdatePhase(ctx context.Context, sessionID int64, phase VoicePhase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-phase")
	s.updatePhase = append(s.updatePhase, phaseCall{sessionID, phase})
	return nil
}

func (s *fakeStore) UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-eval:" + string(status))
	if s.evalUpdErr != nil {
		return s.evalUpdErr
	}
	s.evalUpdates = append(s.evalUpdates, evalUpdate{sessionID, status, evalErr})
	if e, ok := s.sessions[sessionID]; ok {
		e.EvaluateStatus = &status
		e.EvaluateError = evalErr
	}
	return nil
}

func (s *fakeStore) ListByUser(ctx context.Context, userID string, status *VoiceSessionStatus) ([]SessionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("list:" + userID)
	var out []SessionEntity
	for _, e := range s.sessions {
		if userID != "" && e.UserID != nil && *e.UserID != userID {
			continue
		}
		if status != nil && (e.Status == nil || *e.Status != *status) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *fakeStore) CountDialogueMessages(ctx context.Context, sessionID int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.dialogues[sessionID])), nil
}

func (s *fakeStore) DeleteSessionCascade(ctx context.Context, sessionID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("delete-cascade")
	if s.cascadeErr != nil {
		return s.cascadeErr
	}
	if _, ok := s.sessions[sessionID]; !ok {
		return ErrSessionNotFound
	}
	s.deleteOrder = append(s.deleteOrder, "eval", "messages", "session")
	delete(s.sessions, sessionID)
	return nil
}

func (s *fakeStore) FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("find-dialogues")
	return s.dialogues[sessionID], nil
}

func (s *fakeStore) addSession(e *SessionEntity) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	e.ID = id
	now := time.Now()
	if e.CreatedAt == nil {
		e.CreatedAt = &now
	}
	if e.UpdatedAt == nil {
		e.UpdatedAt = &now
	}
	if e.StartTime == nil {
		e.StartTime = &now
	}
	s.sessions[id] = e
	return id
}

func (s *fakeStore) setDialogues(sessionID int64, msgs []MessageEntity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dialogues[sessionID] = msgs
}

// ===== 替身：会话缓存 =====

type fakeCache struct {
	rec *svcRecorder

	mu          sync.Mutex
	sessions    map[int64]CachedSession
	saves       []int64
	invalidates []int64
	getErr      error
	saveErr     error
}

func newFakeCache(rec *svcRecorder) *fakeCache {
	return &fakeCache{rec: rec, sessions: map[int64]CachedSession{}}
}

func (c *fakeCache) Get(ctx context.Context, sessionID int64) (*CachedSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec.add("cache-get")
	if c.getErr != nil {
		return nil, c.getErr
	}
	s, ok := c.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (c *fakeCache) Save(ctx context.Context, s CachedSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec.add("cache-save")
	if c.saveErr != nil {
		return c.saveErr
	}
	c.sessions[s.ID] = s
	c.saves = append(c.saves, s.ID)
	return nil
}

func (c *fakeCache) Invalidate(ctx context.Context, sessionID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec.add("cache-invalidate")
	delete(c.sessions, sessionID)
	c.invalidates = append(c.invalidates, sessionID)
	return nil
}

// ===== 替身：评估流 =====

type fakeStream struct {
	rec *svcRecorder

	mu     sync.Mutex
	adds   []map[string]any
	addErr error
}

func newFakeStream(rec *svcRecorder) *fakeStream {
	return &fakeStream{rec: rec}
}

func (s *fakeStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("xadd")
	if s.addErr != nil {
		return "", s.addErr
	}
	s.adds = append(s.adds, values)
	return "1-1", nil
}

func (s *fakeStream) addAt(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adds[i]
}

// ===== 组装 =====

type svcFixture struct {
	svc    *Service
	rec    *svcRecorder
	store  *fakeStore
	cache  *fakeCache
	stream *fakeStream
}

func newFixture() svcFixture {
	rec := &svcRecorder{}
	store := newFakeStore(rec)
	cache := newFakeCache(rec)
	stream := newFakeStream(rec)
	svc := NewService(ServiceDeps{Repo: store, Cache: cache, Stream: stream, Logger: discardLogger()})
	return svcFixture{svc: svc, rec: rec, store: store, cache: cache, stream: stream}
}

func inProgressSession(startAgo time.Duration) *SessionEntity {
	start := time.Now().Add(-startAgo)
	status := VoiceStatusInProgress
	phase := PhaseTech
	return &SessionEntity{
		UserID:          stringPtr(DefaultUserID),
		RoleType:        "java-backend",
		CurrentPhase:    &phase,
		Status:          &status,
		StartTime:       &start,
		PlannedDuration: intPtr(30),
	}
}

// ===== 创建：默认值与响应契约 =====

// 契约（Java createSession L72-102 + CreateSessionRequest @Builder.Default）：
// skillId 缺省 java-backend（roleType 同值）、difficulty 缺省 mid、intro 缺省 false、
// tech/project/hr 缺省 true、plannedDuration 缺省 30、llmProvider 缺省 null、
// userId 固定 default、status IN_PROGRESS；首个启用阶段 = INTRO 优先。
func TestCreateSessionDefaults(t *testing.T) {
	f := newFixture()
	resp, err := f.svc.CreateSession(context.Background(), CreateSessionRequest{})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	if resp.SessionID != 1 {
		t.Fatalf("首个会话 ID 应为 1: %d", resp.SessionID)
	}
	if resp.RoleType != "java-backend" {
		t.Fatalf("roleType 缺省应为 java-backend: %s", resp.RoleType)
	}
	if resp.Status != "IN_PROGRESS" {
		t.Fatalf("status 应为 IN_PROGRESS: %s", resp.Status)
	}
	if resp.CurrentPhase != "TECH" {
		t.Fatalf("intro 缺省 false 时首阶段应为 TECH: %s", resp.CurrentPhase)
	}
	if resp.PlannedDuration == nil || *resp.PlannedDuration != 30 {
		t.Fatalf("plannedDuration 缺省应为 30: %v", resp.PlannedDuration)
	}
	if resp.WebSocketURL != "/ws/voice-interview/1" {
		t.Fatalf("webSocketUrl 应为相对路径 /ws/voice-interview/1: %s", resp.WebSocketURL)
	}
	if resp.StartTime == nil {
		t.Fatal("startTime 应在创建时写入（Java @PrePersist）")
	}

	e := f.store.sessions[1]
	if e == nil {
		t.Fatal("会话应已落库")
	}
	if e.UserID == nil || *e.UserID != "default" {
		t.Fatalf("userId 应固定 default: %v", e.UserID)
	}
	if e.SkillID == nil || *e.SkillID != "java-backend" {
		t.Fatalf("skillId 缺省应为 java-backend: %v", e.SkillID)
	}
	if e.Difficulty == nil || *e.Difficulty != "mid" {
		t.Fatalf("difficulty 缺省应为 mid: %v", e.Difficulty)
	}
	if e.IntroEnabled == nil || *e.IntroEnabled {
		t.Fatal("introEnabled 缺省应为 false")
	}
	if e.TechEnabled == nil || !*e.TechEnabled || e.ProjectEnabled == nil || !*e.ProjectEnabled || e.HrEnabled == nil || !*e.HrEnabled {
		t.Fatal("tech/project/hrEnabled 缺省应为 true")
	}
	if e.LLMProvider != nil {
		t.Fatalf("llmProvider 缺省应为 null: %v", *e.LLMProvider)
	}
	if e.EvaluateStatus != nil {
		t.Fatalf("新建会话 evaluateStatus 应为 null: %v", e.EvaluateStatus)
	}

	// 创建后写缓存（对齐 Java cacheSession(saved)）。
	if events := f.cacheRecents(); len(events) != 1 || events[0] != 1 {
		t.Fatalf("创建后应恰好写一次缓存: %v", events)
	}
}

func (f *svcFixture) cacheRecents() []int64 {
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()
	return append([]int64(nil), f.cache.saves...)
}

// 契约：显式值逐一透传；roleType 为向后兼容入参，落库 roleType 恒取 skillId
// （Java .roleType(effectiveSkillId) 忽略请求 roleType）；llmProvider 空白视为
// 未传（isBlank → null）；首个启用阶段按 intro 优先。
func TestCreateSessionExplicitValues(t *testing.T) {
	f := newFixture()
	jd := "资深后端 JD"
	resumeID := int64(9)
	planned := 45
	glm := "glm"
	blank := "   "
	resp, err := f.svc.CreateSession(context.Background(), CreateSessionRequest{
		RoleType:        stringPtr("legacy-role"),
		SkillID:         stringPtr("bytedance-backend"),
		Difficulty:      stringPtr("senior"),
		CustomJdText:    &jd,
		ResumeID:        &resumeID,
		IntroEnabled:    boolPtr(true),
		TechEnabled:     boolPtr(false),
		PlannedDuration: &planned,
		LLMProvider:     &glm,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if resp.RoleType != "bytedance-backend" {
		t.Fatalf("落库 roleType 应取 skillId 而非请求 roleType: %s", resp.RoleType)
	}
	if resp.CurrentPhase != "INTRO" {
		t.Fatalf("intro 开启时首阶段应为 INTRO: %s", resp.CurrentPhase)
	}
	e := f.store.sessions[1]
	if e.SkillID == nil || *e.SkillID != "bytedance-backend" {
		t.Fatalf("skillId 应透传: %v", e.SkillID)
	}
	if e.Difficulty == nil || *e.Difficulty != "senior" {
		t.Fatalf("difficulty 应透传: %v", e.Difficulty)
	}
	if e.CustomJdText == nil || *e.CustomJdText != jd {
		t.Fatalf("customJdText 应透传: %v", e.CustomJdText)
	}
	if e.ResumeID == nil || *e.ResumeID != resumeID {
		t.Fatalf("resumeId 应透传: %v", e.ResumeID)
	}
	if e.TechEnabled == nil || *e.TechEnabled {
		t.Fatal("techEnabled=false 应透传")
	}
	if e.PlannedDuration == nil || *e.PlannedDuration != 45 {
		t.Fatalf("plannedDuration 应透传: %v", e.PlannedDuration)
	}
	if e.LLMProvider == nil || *e.LLMProvider != "glm" {
		t.Fatalf("llmProvider 应透传: %v", e.LLMProvider)
	}

	// llmProvider 空白 → null；difficulty 缺省仍 mid。
	f2 := newFixture()
	if _, err := f2.svc.CreateSession(context.Background(), CreateSessionRequest{LLMProvider: &blank}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if e := f2.store.sessions[1]; e.LLMProvider != nil {
		t.Fatalf("空白 llmProvider 应落 null: %v", *e.LLMProvider)
	}
}

// ===== 查询：缓存优先、DB 兜底、不回填缓存（对齐 Java getSession）=====

func TestGetSessionDTOReadPaths(t *testing.T) {
	ctx := context.Background()

	// 缓存命中：直接出响应，不读 DB。
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Minute))
	if err := f.cache.Save(ctx, entityToCached(f.store.sessions[id])); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}
	f.rec.events = nil
	resp, err := f.svc.GetSessionDTO(ctx, id)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if resp == nil || resp.SessionID != id || resp.WebSocketURL != "/ws/voice-interview/"+strconv.FormatInt(id, 10) {
		t.Fatalf("缓存命中响应不符: %+v", resp)
	}
	if f.rec.has("find") {
		t.Fatal("缓存命中不应回源 DB")
	}

	// 缓存未命中：回源 DB，且对齐 Java 不回填缓存。
	f2 := newFixture()
	id2 := f2.store.addSession(inProgressSession(time.Minute))
	f2.rec.events = nil
	resp2, err := f2.svc.GetSessionDTO(ctx, id2)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if resp2 == nil || resp2.SessionID != id2 {
		t.Fatalf("DB 兜底响应不符: %+v", resp2)
	}
	if !f2.rec.has("find") {
		t.Fatal("缓存未命中应回源 DB")
	}
	if f2.rec.has("cache-save") {
		t.Fatal("DB 兜底不应回填缓存（对齐 Java getSession）")
	}

	// 不存在：(nil, nil)，由控制器转 "Session not found: {id}"。
	f3 := newFixture()
	resp3, err := f3.svc.GetSessionDTO(ctx, 999)
	if err != nil {
		t.Fatalf("不存在不应报错: %v", err)
	}
	if resp3 != nil {
		t.Fatalf("不存在应返回 nil: %+v", resp3)
	}
}

// ===== 结束：置 COMPLETED + PENDING，提交后投递（顺序与失败路径）=====

// 契约（Java endSession L126-152）：endTime/COMPLETED/currentPhase=COMPLETED/
// actualDuration 秒/evaluateStatus=PENDING；先失效缓存再在事务提交后 XADD；
// 消息字段 voiceSessionId/retryCount="0" 逐字。
func TestEndSessionFlow(t *testing.T) {
	f := newFixture()
	id := f.store.addSession(inProgressSession(90 * time.Second))
	f.rec.events = nil

	if err := f.svc.EndSession(context.Background(), id); err != nil {
		t.Fatalf("结束失败: %v", err)
	}

	events := f.rec.list()
	endIdx := indexOf(events, "update-end")
	invIdx := indexOf(events, "cache-invalidate")
	addIdx := indexOf(events, "xadd")
	if endIdx < 0 || invIdx < endIdx || addIdx < invIdx {
		t.Fatalf("顺序应为 更新落库 → 失效缓存 → 投递评估流: %v", events)
	}

	ended := f.store.updateEnd[0]
	if ended.Status == nil || *ended.Status != VoiceStatusCompleted {
		t.Fatalf("结束后状态应为 COMPLETED: %v", ended.Status)
	}
	if ended.CurrentPhase == nil || *ended.CurrentPhase != PhaseCompleted {
		t.Fatalf("结束后阶段应为 COMPLETED: %v", ended.CurrentPhase)
	}
	if ended.EvaluateStatus == nil || *ended.EvaluateStatus != VoiceEvaluatePending {
		t.Fatalf("评估状态应置 PENDING: %v", ended.EvaluateStatus)
	}
	if ended.EndTime == nil {
		t.Fatal("endTime 应写入")
	}
	if ended.ActualDuration == nil || *ended.ActualDuration < 89 || *ended.ActualDuration > 91 {
		t.Fatalf("actualDuration 应为约 90 秒: %v", ended.ActualDuration)
	}

	if len(f.stream.adds) != 1 {
		t.Fatalf("应恰投递一条评估消息: %d", len(f.stream.adds))
	}
	msg := f.stream.addAt(0)
	if msg[VoiceEvalFieldSessionID] != strconv.FormatInt(id, 10) {
		t.Fatalf("消息 voiceSessionId 应为字符串化会话 ID: %v", msg[VoiceEvalFieldSessionID])
	}
	if msg[VoiceEvalFieldRetryCount] != "0" {
		t.Fatalf("首条消息 retryCount 应为 \"0\": %v", msg[VoiceEvalFieldRetryCount])
	}
}

// 契约：会话不存在 → 仅告警返回成功（Java endSession L131-134 warn + return），
// 不落库不投递。
func TestEndSessionMissingSilentSuccess(t *testing.T) {
	f := newFixture()
	if err := f.svc.EndSession(context.Background(), 404); err != nil {
		t.Fatalf("不存在时应静默成功: %v", err)
	}
	if len(f.store.updateEnd) != 0 || len(f.stream.adds) != 0 {
		t.Fatal("不应有任何副作用")
	}
}

// 契约：XADD 失败对齐 Java VoiceEvaluateStreamProducer.onSendFailed ——
// 置 FAILED + evaluate_error="任务入队失败"，且不向调用方报错。
func TestEndSessionXAddFailureMarksFailed(t *testing.T) {
	f := newFixture()
	f.stream.addErr = errors.New("redis down")
	id := f.store.addSession(inProgressSession(time.Second))

	if err := f.svc.EndSession(context.Background(), id); err != nil {
		t.Fatalf("投递失败不应向调用方报错: %v", err)
	}
	if len(f.store.evalUpdates) != 1 {
		t.Fatalf("应恰一次 FAILED 状态写入: %+v", f.store.evalUpdates)
	}
	upd := f.store.evalUpdates[0]
	if upd.status != VoiceEvaluateFailed {
		t.Fatalf("应置 FAILED: %s", upd.status)
	}
	if upd.err == nil || *upd.err != "任务入队失败" {
		t.Fatalf("evaluate_error 应为 任务入队失败: %v", upd.err)
	}
	if upd.id != id {
		t.Fatalf("状态写入应定位到该会话: %d", upd.id)
	}
}

// ===== 暂停/恢复：守卫、落库列、缓存读写路径 =====

func TestPauseSession(t *testing.T) {
	ctx := context.Background()

	// IN_PROGRESS 可暂停：落 pausedAt、失效缓存、不断连（无连接操作副作用）。
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))
	f.rec.events = nil
	if err := f.svc.PauseSession(ctx, id, "user_initiated"); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	events := f.rec.list()
	if indexOf(events, "update-pause") < 0 || indexOf(events, "cache-invalidate") < indexOf(events, "update-pause") {
		t.Fatalf("顺序应为 落库 → 失效缓存: %v", events)
	}
	if len(f.store.updatePause) != 1 || f.store.updatePause[0].id != id {
		t.Fatalf("应写入暂停时间: %+v", f.store.updatePause)
	}

	// 非 IN_PROGRESS → 400 "会话状态为 PAUSED，无法暂停"（Java L401-405 逐字）。
	f2 := newFixture()
	paused := inProgressSession(time.Hour)
	pausedStatus := VoiceStatusPaused
	paused.Status = &pausedStatus
	id2 := f2.store.addSession(paused)
	err := f2.svc.PauseSession(ctx, id2, "user_initiated")
	ae := apperr.From(err)
	if ae.Code != apperr.CodeBadRequest || ae.Message != "会话状态为 PAUSED，无法暂停" {
		t.Fatalf("错误码/消息不符: [%d %s]", ae.Code, ae.Message)
	}

	// 不存在 → 404 "会话不存在: 5"（Java BusinessException(NOT_FOUND) 逐字）。
	f3 := newFixture()
	err = f3.svc.PauseSession(ctx, 5, "user_initiated")
	ae = apperr.From(err)
	if ae.Code != apperr.CodeNotFound || ae.Message != "会话不存在: 5" {
		t.Fatalf("错误码/消息不符: [%d %s]", ae.Code, ae.Message)
	}
}

func TestResumeSession(t *testing.T) {
	ctx := context.Background()

	// PAUSED 可恢复：落 resumedAt、重建缓存、返回含 webSocketUrl 的完整响应。
	f := newFixture()
	paused := inProgressSession(time.Hour)
	pausedStatus := VoiceStatusPaused
	paused.Status = &pausedStatus
	id := f.store.addSession(paused)
	f.rec.events = nil
	resp, err := f.svc.ResumeSession(ctx, id)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if resp == nil || resp.WebSocketURL != "/ws/voice-interview/"+strconv.FormatInt(id, 10) || resp.Status != "IN_PROGRESS" {
		t.Fatalf("恢复响应不符: %+v", resp)
	}
	events := f.rec.list()
	if indexOf(events, "update-resume") < 0 || indexOf(events, "cache-save") < indexOf(events, "update-resume") {
		t.Fatalf("顺序应为 落库 → 重建缓存: %v", events)
	}
	if len(f.store.updateResume) != 1 || f.store.updateResume[0].id != id {
		t.Fatalf("应写入恢复时间: %+v", f.store.updateResume)
	}
	if _, ok := f.cache.sessions[id]; !ok {
		t.Fatal("恢复后应重建缓存")
	}

	// 非 PAUSED → 400 "会话状态为 IN_PROGRESS，无法恢复"。
	f2 := newFixture()
	id2 := f2.store.addSession(inProgressSession(time.Hour))
	ae := apperr.From(f2.resumeErrHelper(ctx, id2))
	if ae.Code != apperr.CodeBadRequest || ae.Message != "会话状态为 IN_PROGRESS，无法恢复" {
		t.Fatalf("错误码/消息不符: [%d %s]", ae.Code, ae.Message)
	}

	// 不存在 → 404。
	f3 := newFixture()
	ae = apperr.From(f3.resumeErrHelper(ctx, 6))
	if ae.Code != apperr.CodeNotFound || ae.Message != "会话不存在: 6" {
		t.Fatalf("错误码/消息不符: [%d %s]", ae.Code, ae.Message)
	}
}

func (f *svcFixture) resumeErrHelper(ctx context.Context, id int64) error {
	_, err := f.svc.ResumeSession(ctx, id)
	return err
}

// ===== 列表：userId 缺省 default、status 过滤归一、meta 十字段映射 =====

func TestGetAllSessions(t *testing.T) {
	ctx := context.Background()

	f := newFixture()
	id1 := f.store.addSession(inProgressSession(time.Hour))
	completed := inProgressSession(2 * time.Hour)
	completedStatus := VoiceStatusCompleted
	completed.Status = &completedStatus
	completed.EvaluateStatus = evalPtr(VoiceEvaluatePending)
	id2 := f.store.addSession(completed)
	f.store.setDialogues(id1, []MessageEntity{{ID: 1, SessionID: id1, MessageType: MessageDialogue}})

	// userId 缺省 → default 透传存储层。
	if _, err := f.svc.GetAllSessions(ctx, "", ""); err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if !f.rec.has("list:default") {
		t.Fatalf("userId 缺省应固定 default: %v", f.rec.list())
	}

	// status 过滤大小写归一。
	f.rec.events = nil
	list, err := f.svc.GetAllSessions(ctx, "default", "completed")
	if err != nil {
		t.Fatalf("过滤列表失败: %v", err)
	}
	if len(list) != 1 || list[0].SessionID != id2 {
		t.Fatalf("completed 过滤应只命中一场: %+v", list)
	}

	// meta 映射：messageCount 来自消息计数、evaluateStatus 枚举名、evaluateError null。
	meta := list[0]
	if meta.EvaluateStatus == nil || *meta.EvaluateStatus != "PENDING" {
		t.Fatalf("evaluateStatus 应为 PENDING: %v", meta.EvaluateStatus)
	}
	if meta.EvaluateError != nil {
		t.Fatalf("evaluateError 应为 null: %v", meta.EvaluateError)
	}
	if meta.Status != "COMPLETED" || meta.CurrentPhase != "TECH" || meta.RoleType != "java-backend" {
		t.Fatalf("meta 枚举字段不符: %+v", meta)
	}
	if _, err := f.svc.GetAllSessions(ctx, "", "in_progress"); err != nil {
		t.Fatalf("小写过滤失败: %v", err)
	}

	// 全量列表：两场都返回，messageCount 各自统计。
	all, err := f.svc.GetAllSessions(ctx, "default", "")
	if err != nil {
		t.Fatalf("全量列表失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应返回两场: %d", len(all))
	}
	for _, m := range all {
		want := int64(0)
		if m.SessionID == id1 {
			want = 1
		}
		if m.MessageCount != want {
			t.Fatalf("会话 %d messageCount 应为 %d: %d", m.SessionID, want, m.MessageCount)
		}
	}

	// 非法过滤值 → 400（Java valueOf IllegalArgumentException → 400）。
	f2 := newFixture()
	ae := apperr.From(f2.listErrHelper(ctx, "nope"))
	if ae.Code != apperr.CodeBadRequest {
		t.Fatalf("非法过滤值应 400: [%d %s]", ae.Code, ae.Message)
	}
}

func (f *svcFixture) listErrHelper(ctx context.Context, status string) error {
	_, err := f.svc.GetAllSessions(ctx, "", status)
	return err
}

// ===== 删除：级联顺序 eval → messages → session；不存在 → 10001 =====

func TestDeleteSessionCascade(t *testing.T) {
	ctx := context.Background()

	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))
	if err := f.svc.DeleteSession(ctx, id); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	want := []string{"eval", "messages", "session"}
	if len(f.store.deleteOrder) != 3 {
		t.Fatalf("级联删除应覆盖 评估→消息→会话: %v", f.store.deleteOrder)
	}
	for i, w := range want {
		if f.store.deleteOrder[i] != w {
			t.Fatalf("级联删除顺序应为 评估→消息→会话: %v", f.store.deleteOrder)
		}
	}

	// 不存在 → 10001 "会话不存在: 7"（Java VOICE_SESSION_NOT_FOUND + 自定义消息逐字）。
	ae := apperr.From(f.svc.DeleteSession(ctx, 7))
	if ae == nil {
		t.Fatal("删除不存在会话应报错")
	}
	if ae.Code != apperr.CodeVoiceSessionNotFound || ae.Message != "会话不存在: 7" {
		t.Fatalf("错误码/消息不符: [%d %s]", ae.Code, ae.Message)
	}
}

// ===== 消息列表：DTO 八字段映射（SUMMARY 排除为 repo SQL 语义，见 repo.go 注释）=====

func TestGetConversationHistoryDTOMapping(t *testing.T) {
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))
	ts := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	seq := 1
	phase := PhaseTech
	userText := "你好"
	aiText := "请自我介绍"
	f.store.setDialogues(id, []MessageEntity{{
		ID:                 11,
		SessionID:          id,
		MessageType:        MessageDialogue,
		Phase:              &phase,
		UserRecognizedText: &userText,
		AiGeneratedText:    &aiText,
		Timestamp:          &ts,
		SequenceNum:        &seq,
	}})

	list, err := f.svc.GetConversationHistoryDTO(context.Background(), id)
	if err != nil {
		t.Fatalf("查询消息失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应返回一条消息: %d", len(list))
	}
	m := list[0]
	if m.ID != 11 || m.SessionID != id || m.MessageType != "DIALOGUE" {
		t.Fatalf("标识字段不符: %+v", m)
	}
	if m.Phase == nil || *m.Phase != "TECH" {
		t.Fatalf("phase 应为枚举名 TECH: %v", m.Phase)
	}
	if m.UserRecognizedText == nil || *m.UserRecognizedText != userText || m.AiGeneratedText == nil || *m.AiGeneratedText != aiText {
		t.Fatalf("文本字段不符: %+v", m)
	}
	if m.Timestamp == nil || !m.Timestamp.Equal(ts) {
		t.Fatalf("timestamp 不符: %v", m.Timestamp)
	}
	if m.SequenceNum == nil || *m.SequenceNum != 1 {
		t.Fatalf("sequenceNum 不符: %v", m.SequenceNum)
	}
}

// ===== 阶段切换（WS 网关/阶段机的前置会话服务，票 04/05 消费）=====

// 契约（Java startPhase L199-222）：非法阶段仅记日志不改状态；会话不存在仅告警；
// 合法阶段落库并更新缓存。
func TestStartPhase(t *testing.T) {
	ctx := context.Background()

	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))
	if err := f.svc.StartPhase(ctx, id, "project"); err != nil {
		t.Fatalf("切换阶段失败: %v", err)
	}
	if len(f.store.updatePhase) != 1 || f.store.updatePhase[0].phase != PhaseProject {
		t.Fatalf("应落库 PROJECT: %+v", f.store.updatePhase)
	}
	if _, ok := f.cache.sessions[id]; !ok {
		t.Fatal("切换阶段后应更新缓存")
	}

	// 非法阶段：静默（仅日志）。
	f2 := newFixture()
	id2 := f2.store.addSession(inProgressSession(time.Hour))
	if err := f2.svc.StartPhase(ctx, id2, "NOT_A_PHASE"); err != nil {
		t.Fatalf("非法阶段不应报错: %v", err)
	}
	if len(f2.store.updatePhase) != 0 {
		t.Fatal("非法阶段不应落库")
	}

	// 会话不存在：静默。
	f3 := newFixture()
	if err := f3.svc.StartPhase(ctx, 404, "TECH"); err != nil {
		t.Fatalf("会话不存在不应报错: %v", err)
	}
	if len(f3.store.updatePhase) != 0 {
		t.Fatal("会话不存在不应落库")
	}
}

// 契约（Java getNextPhase L554-572）：按启用阶段顺延，全关则 COMPLETED；
// HR/COMPLETED 之后恒 COMPLETED；currentPhase 为空时取首个启用阶段。
func TestGetNextPhase(t *testing.T) {
	cases := []struct {
		name    string
		current *VoicePhase
		flags   [4]bool // intro/tech/project/hr
		want    VoicePhase
	}{
		{"空阶段取首个启用", nil, [4]bool{true, true, true, true}, PhaseIntro},
		{"空阶段且 intro 关取 TECH", nil, [4]bool{false, true, true, true}, PhaseTech},
		{"INTRO 后取 TECH", phasePtr(PhaseIntro), [4]bool{true, true, true, true}, PhaseTech},
		{"INTRO 后 tech 关取 PROJECT", phasePtr(PhaseIntro), [4]bool{true, false, true, true}, PhaseProject},
		{"INTRO 后仅 hr 开取 HR", phasePtr(PhaseIntro), [4]bool{true, false, false, true}, PhaseHR},
		{"INTRO 后全关取 COMPLETED", phasePtr(PhaseIntro), [4]bool{true, false, false, false}, PhaseCompleted},
		{"TECH 后取 PROJECT", phasePtr(PhaseTech), [4]bool{true, true, true, true}, PhaseProject},
		{"TECH 后 project 关取 HR", phasePtr(PhaseTech), [4]bool{true, true, false, true}, PhaseHR},
		{"TECH 后仅 hr 开取 HR", phasePtr(PhaseTech), [4]bool{true, true, false, true}, PhaseHR},
		{"TECH 后 hr 关取 COMPLETED", phasePtr(PhaseTech), [4]bool{true, true, false, false}, PhaseCompleted},
		{"PROJECT 后取 HR", phasePtr(PhaseProject), [4]bool{true, true, true, true}, PhaseHR},
		{"PROJECT 后 hr 关取 COMPLETED", phasePtr(PhaseProject), [4]bool{true, true, true, false}, PhaseCompleted},
		{"HR 之后恒 COMPLETED", phasePtr(PhaseHR), [4]bool{true, true, true, true}, PhaseCompleted},
		{"COMPLETED 之后恒 COMPLETED", phasePtr(PhaseCompleted), [4]bool{true, true, true, true}, PhaseCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &SessionEntity{
				CurrentPhase:   tc.current,
				IntroEnabled:   boolPtr(tc.flags[0]),
				TechEnabled:    boolPtr(tc.flags[1]),
				ProjectEnabled: boolPtr(tc.flags[2]),
				HrEnabled:      boolPtr(tc.flags[3]),
			}
			if got := getNextPhase(e); got != tc.want {
				t.Fatalf("期望 %s，got %s", tc.want, got)
			}
		})
	}
}

// ===== 评估状态更新：吞错（对齐 Java updateEvaluateStatus try/catch）=====

func TestUpdateEvaluateStatusSwallowsError(t *testing.T) {
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))

	// 成功路径：落库 + 失效缓存。
	f.svc.UpdateEvaluateStatus(context.Background(), id, VoiceEvaluateProcess, nil)
	events := f.rec.list()
	if indexOf(events, "update-eval:PROCESSING") < 0 || indexOf(events, "cache-invalidate") < indexOf(events, "update-eval:PROCESSING") {
		t.Fatalf("顺序应为 落库 → 失效缓存: %v", events)
	}

	// 落库失败：仅记日志，不失效缓存、不上抛。
	f2 := newFixture()
	f2.store.evalUpdErr = errors.New("db down")
	f2.svc.UpdateEvaluateStatus(context.Background(), 1, VoiceEvaluateFailed, stringPtr("x"))
	if f2.rec.has("cache-invalidate") {
		t.Fatal("落库失败不应继续失效缓存（对齐 Java 同一 try 块）")
	}
}

// ===== 纯逻辑与常量 =====

// 契约：缓存键逐字 voice:interview:session:{id}（Java SESSION_CACHE_KEY_PREFIX）。
func TestSessionCacheKeyFormat(t *testing.T) {
	if got := sessionCacheKey(42); got != "voice:interview:session:42" {
		t.Fatalf("缓存键不符: %s", got)
	}
}

// 契约：评估流消息字段逐字（Java AsyncTaskStreamConstants L18/L138 + producer buildMessage）。
func TestEvaluateTaskMessageFields(t *testing.T) {
	msg := buildEvaluateTaskMessage(9)
	if len(msg) != 2 {
		t.Fatalf("应恰两字段: %v", msg)
	}
	if msg[VoiceEvalFieldSessionID] != "9" || msg[VoiceEvalFieldRetryCount] != "0" {
		t.Fatalf("字段不符: %v", msg)
	}
	if VoiceEvalStreamKey != "voice:evaluate:stream" {
		t.Fatalf("流键不符: %s", VoiceEvalStreamKey)
	}
	if VoiceEvalStreamMaxLen != 1000 {
		t.Fatalf("MAXLEN 应为 1000: %d", VoiceEvalStreamMaxLen)
	}
}

// 契约：语音错误码对照 Java ErrorCode L69-84 原文（码 + 默认消息逐字）。
func TestVoiceApperrCodes(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		wantMsg string
	}{
		{"会话不存在 10001", apperr.CodeVoiceSessionNotFound, "语音面试会话不存在"},
		{"评估失败 10004", apperr.CodeVoiceEvaluationFailed, "语音面试评估失败"},
		{"评估结果不存在 10006", apperr.CodeVoiceEvaluationNotFound, "语音面试评估结果不存在"},
		{"语音配置读取失败 11009", apperr.CodeVoiceReadFailed, "读取语音服务配置失败"},
		{"语音配置写入失败 11010", apperr.CodeVoiceWriteFailed, "写入语音服务配置失败"},
		{"语音配置测试失败 11011", apperr.CodeVoiceConfigTestFailed, "语音服务连通性测试失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := apperr.New(tc.code, tc.wantMsg)
			if ae.Code != tc.code || ae.Message != tc.wantMsg {
				t.Fatalf("码/消息不符: [%d %s]", ae.Code, ae.Message)
			}
		})
	}
	if got := apperr.VoiceSessionNotFound(); got.Code != 10001 || got.Message != "语音面试会话不存在" {
		t.Fatalf("默认构造不符: [%d %s]", got.Code, got.Message)
	}
	if got := apperr.VoiceSessionNotFoundWithMessage("会话不存在: 7"); got.Code != 10001 || got.Message != "会话不存在: 7" {
		t.Fatalf("自定义消息构造不符: [%d %s]", got.Code, got.Message)
	}
}

// 契约：暂停/恢复守卫（Java pauseSession/resumeSession 状态分支逐字）。
func TestPauseResumeGuards(t *testing.T) {
	if err := pauseGuard(VoiceStatusInProgress); err != nil {
		t.Fatalf("IN_PROGRESS 应可暂停: %v", err)
	}
	ae := apperr.From(pauseGuard(VoiceStatusCompleted))
	if ae == nil || ae.Code != apperr.CodeBadRequest || ae.Message != "会话状态为 COMPLETED，无法暂停" {
		t.Fatalf("暂停守卫不符: [%v]", ae)
	}
	if err := resumeGuard(VoiceStatusPaused); err != nil {
		t.Fatalf("PAUSED 应可恢复: %v", err)
	}
	ae = apperr.From(resumeGuard(VoiceStatusFailed))
	if ae == nil || ae.Code != apperr.CodeBadRequest || ae.Message != "会话状态为 FAILED，无法恢复" {
		t.Fatalf("恢复守卫不符: [%v]", ae)
	}
}

// 契约：缓存实体 JSON 往返无损（Redis 存取路径的序列化保真）。
func TestCachedSessionJSONRoundTrip(t *testing.T) {
	id := int64(3)
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	phase := PhaseHR
	status := VoiceStatusPaused
	eval := VoiceEvaluatePending
	e := &SessionEntity{
		ID: id, UserID: stringPtr("default"), RoleType: "java-backend",
		SkillID: stringPtr("java-backend"), Difficulty: stringPtr("mid"),
		ResumeID: &id, IntroEnabled: boolPtr(false), TechEnabled: boolPtr(true),
		ProjectEnabled: boolPtr(true), HrEnabled: boolPtr(true),
		CurrentPhase: &phase, Status: &status, PlannedDuration: intPtr(30),
		StartTime: &start, EvaluateStatus: &eval, EvaluateError: stringPtr("e"),
	}
	b, err := json.Marshal(entityToCached(e))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := &CachedSession{}
	if err := json.Unmarshal(b, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back := cachedToEntity(got)
	if back.ID != id || back.RoleType != "java-backend" || back.CurrentPhase == nil || *back.CurrentPhase != PhaseHR ||
		back.Status == nil || *back.Status != VoiceStatusPaused || back.EvaluateStatus == nil || *back.EvaluateStatus != VoiceEvaluatePending ||
		back.PlannedDuration == nil || *back.PlannedDuration != 30 || back.ResumeID == nil || *back.ResumeID != id {
		t.Fatalf("往返后字段不符: %+v", back)
	}
	if back.StartTime == nil || !back.StartTime.Equal(start) {
		t.Fatalf("时间往返不符: %v", back.StartTime)
	}
}

func indexOf(list []string, target string) int {
	for i, v := range list {
		if v == target {
			return i
		}
	}
	return -1
}
