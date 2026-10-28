// 评估恢复巡检与 REST 评估接口单测：PENDING 超 3min 补投、PROCESSING 超 30min
// 置败（时钟注入）、GET evaluation 三分支、POST evaluation 重试三分支。
// 契约来源：Java VoiceInterviewService.cleanupStaleSessions（L738-785）、
// VoiceInterviewController.getEvaluation / generateEvaluation（L156-222）逐字。
package voiceinterview

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// recoveryFixture 恢复巡检夹具（Service + fakeStore + fakeStream）。
type recoveryFixture struct {
	rec    *svcRecorder
	store  *fakeStore
	stream *fakeStream
	svc    *Service
	now    time.Time
}

func newRecoveryFixture() *recoveryFixture {
	rec := &svcRecorder{}
	store := newFakeStore(rec)
	cache := newFakeCache(rec)
	stream := newFakeStream(rec)
	svc := NewService(ServiceDeps{
		Repo:     store,
		Cache:    cache,
		Stream:   stream,
		Logger:   discardLogger(),
		Recovery: EvalRecoveryParams{PendingRequeueAfter: 3 * time.Minute, ProcessingFailAfter: 30 * time.Minute},
	})
	return &recoveryFixture{rec: rec, store: store, stream: stream, svc: svc, now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (f *recoveryFixture) addStaleSession(id int64, status VoiceSessionStatus, evalStatus VoiceEvaluateStatus, updatedAgo time.Duration) int64 {
	e := evalChainSession(id, evalPtr(evalStatus))
	st := status
	e.Status = &st
	updated := f.now.Add(-updatedAgo)
	e.UpdatedAt = &updated
	return f.store.addSession(e) // fakeStore 自行分配主键，返回实际 ID
}

func TestRecoverStalePendingRequeues(t *testing.T) {
	f := newRecoveryFixture()
	staleID := f.addStaleSession(1, VoiceStatusCompleted, VoiceEvaluatePending, 5*time.Minute)
	f.addStaleSession(2, VoiceStatusCompleted, VoiceEvaluatePending, 1*time.Minute) // 未超阈不补投

	cleaned, err := f.svc.RecoverStaleEvaluations(context.Background(), f.now)
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应补投 1 条, got %d", cleaned)
	}
	if len(f.stream.adds) != 1 {
		t.Fatalf("应 XADD 一条补投消息, got %d", len(f.stream.adds))
	}
	added := f.stream.addAt(0)
	if added[VoiceEvalFieldSessionID] != strconv.FormatInt(staleID, 10) {
		t.Errorf("补投消息 voiceSessionId = %v, want %q", added[VoiceEvalFieldSessionID], strconv.FormatInt(staleID, 10))
	}
	if added[VoiceEvalFieldRetryCount] != "0" {
		t.Errorf("补投消息 retryCount = %v, want \"0\"（Java buildMessage）", added[VoiceEvalFieldRetryCount])
	}
	// 时间戳按注入时钟推进、错误清空（Java L762-764）。
	e := f.store.sessions[staleID]
	if e.UpdatedAt == nil || !e.UpdatedAt.Equal(f.now) {
		t.Errorf("补投后 updated_at 应推进到注入时钟, got %v", e.UpdatedAt)
	}
	if e.EvaluateError != nil {
		t.Errorf("补投后 evaluate_error 应清空, got %v", *e.EvaluateError)
	}
}

func TestRecoverStaleProcessingFails(t *testing.T) {
	f := newRecoveryFixture()
	staleID := f.addStaleSession(7, VoiceStatusCompleted, VoiceEvaluateProcess, 31*time.Minute)
	f.addStaleSession(8, VoiceStatusCompleted, VoiceEvaluateProcess, 10*time.Minute) // 未超阈不动

	cleaned, err := f.svc.RecoverStaleEvaluations(context.Background(), f.now)
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应置败 1 条, got %d", cleaned)
	}
	if len(f.stream.adds) != 0 {
		t.Errorf("置败路径不应补投, got %d 条", len(f.stream.adds))
	}
	e := f.store.sessions[staleID]
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluateFailed {
		t.Errorf("卡死 PROCESSING 应置 FAILED, got %v", e.EvaluateStatus)
	}
	if e.EvaluateError == nil || *e.EvaluateError != "评估超时，请重新触发" {
		t.Errorf("evaluate_error 逐字不符: %v", e.EvaluateError)
	}
}

func TestRecoverRequeueSendFailureMarksFailed(t *testing.T) {
	f := newRecoveryFixture()
	staleID := f.addStaleSession(1, VoiceStatusCompleted, VoiceEvaluatePending, 5*time.Minute)
	f.stream.addErr = errors.New("redis down")

	cleaned, err := f.svc.RecoverStaleEvaluations(context.Background(), f.now)
	if err != nil {
		t.Fatalf("巡检不应上抛投递失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("cleaned = %d, want 1", cleaned)
	}
	e := f.store.sessions[staleID]
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluateFailed {
		t.Errorf("投递失败应置 FAILED（onSendFailed）, got %v", e.EvaluateStatus)
	}
	if e.EvaluateError == nil || *e.EvaluateError != "任务入队失败" {
		t.Errorf("evaluate_error = %v, want %q", e.EvaluateError, "任务入队失败")
	}
}

func TestRecoverNoStaleSessions(t *testing.T) {
	f := newRecoveryFixture()
	f.addStaleSession(1, VoiceStatusCompleted, VoiceEvaluatePending, 30*time.Second)
	f.addStaleSession(2, VoiceStatusCompleted, VoiceEvaluateCompleted, 2*time.Hour)

	cleaned, err := f.svc.RecoverStaleEvaluations(context.Background(), f.now)
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if cleaned != 0 {
		t.Errorf("无滞留场次应清理 0 条, got %d", cleaned)
	}
}

// ===== REST GET /sessions/{id}/evaluation 三分支 =====

type apiFixture struct {
	*recoveryFixture
	evalStore *fakeVoiceEvalStore
}

func newAPIFixture() *apiFixture {
	f := &apiFixture{recoveryFixture: newRecoveryFixture()}
	f.evalStore = newFakeVoiceEvalStore(f.rec)
	f.svc = NewService(ServiceDeps{
		Repo:   f.store,
		Cache:  newFakeCache(f.rec),
		Stream: f.stream,
		Logger: discardLogger(),
		EvalDetail: NewEvaluationService(EvaluationServiceDeps{
			Store:  f.evalStore,
			Eval:   &stubVoiceRunner{},
			Logger: discardLogger(),
		}),
		Recovery: EvalRecoveryParams{PendingRequeueAfter: 3 * time.Minute, ProcessingFailAfter: 30 * time.Minute},
	})
	return f
}

func TestGetEvaluationStatusBranches(t *testing.T) {
	t.Run("会话不存在返回10001", func(t *testing.T) {
		f := newAPIFixture()
		_, err := f.svc.GetEvaluationStatus(context.Background(), 9)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != apperr.CodeVoiceSessionNotFound {
			t.Fatalf("应返回 10001, got %v", err)
		}
		if ae.Message != "会话不存在: 9" {
			t.Errorf("消息 = %q, want %q", ae.Message, "会话不存在: 9")
		}
	})

	t.Run("PROCESSING 原样返回不带评估", func(t *testing.T) {
		f := newAPIFixture()
		errMsg := "上次的错误"
		e := evalChainSession(5, evalPtr(VoiceEvaluateProcess))
		e.EvaluateError = &errMsg
		id := f.store.addSession(e)

		dto, err := f.svc.GetEvaluationStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if dto.EvaluateStatus == nil || *dto.EvaluateStatus != "PROCESSING" {
			t.Errorf("evaluateStatus = %v, want PROCESSING", dto.EvaluateStatus)
		}
		if dto.EvaluateError == nil || *dto.EvaluateError != "上次的错误" {
			t.Errorf("evaluateError = %v", dto.EvaluateError)
		}
		if dto.EvaluateStatusUpdatedAt == nil {
			t.Error("evaluateStatusUpdatedAt 应取场次 updated_at")
		}
		if dto.Evaluation != nil {
			t.Errorf("非 COMPLETED 不应内嵌评估, got %+v", dto.Evaluation)
		}
	})

	t.Run("COMPLETED 内嵌报告", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateCompleted)))
		f.evalStore.evaluations[id] = &EvaluationEntity{
			SessionID:               int64Ptr(id),
			OverallScore:            nil,
			OverallFeedback:         stringPtr("总评"),
			QuestionEvaluationsJSON: stringPtr("[]"),
			StrengthsJSON:           stringPtr("[]"),
			ImprovementsJSON:        stringPtr("[]"),
			ReferenceAnswersJSON:    stringPtr("[]"),
		}

		dto, err := f.svc.GetEvaluationStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if dto.Evaluation == nil {
			t.Fatal("COMPLETED 应内嵌 evaluation")
		}
		if dto.Evaluation.TotalQuestions != 0 {
			t.Errorf("totalQuestions = %d, want 0", dto.Evaluation.TotalQuestions)
		}
		if dto.Evaluation.OverallFeedback == nil || *dto.Evaluation.OverallFeedback != "总评" {
			t.Errorf("evaluation.overallFeedback = %v", dto.Evaluation.OverallFeedback)
		}
	})

	t.Run("COMPLETED 但评估行缺失返回10006", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateCompleted)))

		_, err := f.svc.GetEvaluationStatus(context.Background(), id)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != apperr.CodeVoiceEvaluationNotFound {
			t.Fatalf("应返回 10006, got %v", err)
		}
	})

	t.Run("评估状态为空时输出 null", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, nil))

		dto, err := f.svc.GetEvaluationStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if dto.EvaluateStatus != nil {
			t.Errorf("evaluateStatus = %v, want nil", dto.EvaluateStatus)
		}
	})
}

// ===== REST POST /sessions/{id}/evaluation 三分支 =====

func TestRetryEvaluationBranches(t *testing.T) {
	t.Run("会话不存在返回10001", func(t *testing.T) {
		f := newAPIFixture()
		_, err := f.svc.RetryEvaluation(context.Background(), 9)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != apperr.CodeVoiceSessionNotFound {
			t.Fatalf("应返回 10001, got %v", err)
		}
	})

	t.Run("COMPLETED 返回缓存且不入队", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateCompleted)))
		f.evalStore.evaluations[id] = &EvaluationEntity{
			SessionID:               int64Ptr(id),
			QuestionEvaluationsJSON: stringPtr("[]"),
			StrengthsJSON:           stringPtr("[]"),
			ImprovementsJSON:        stringPtr("[]"),
			ReferenceAnswersJSON:    stringPtr("[]"),
		}

		dto, err := f.svc.RetryEvaluation(context.Background(), id)
		if err != nil {
			t.Fatalf("重试失败: %v", err)
		}
		if dto.EvaluateStatus == nil || *dto.EvaluateStatus != "COMPLETED" {
			t.Errorf("evaluateStatus = %v, want COMPLETED", dto.EvaluateStatus)
		}
		if dto.Evaluation == nil {
			t.Error("COMPLETED 分支应返回缓存的评估结果")
		}
		if dto.EvaluateStatusUpdatedAt == nil {
			t.Error("evaluateStatusUpdatedAt 应取场次 updated_at")
		}
		events := f.rec.list()
		for _, e := range events {
			if containsEvent([]string{"xadd", "update-eval:PENDING", "update-eval:FAILED", "cache-invalidate", "cache-save"}, e) {
				t.Errorf("COMPLETED 分支不应有状态写入/入队动作, got %v", events)
			}
		}
	})

	t.Run("PROCESSING 原样返回", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateProcess)))

		dto, err := f.svc.RetryEvaluation(context.Background(), id)
		if err != nil {
			t.Fatalf("重试失败: %v", err)
		}
		if dto.EvaluateStatus == nil || *dto.EvaluateStatus != "PROCESSING" {
			t.Errorf("evaluateStatus = %v, want PROCESSING", dto.EvaluateStatus)
		}
		if dto.Evaluation != nil {
			t.Error("PROCESSING 分支不应内嵌评估")
		}
		if containsEvent(f.rec.list(), "xadd") {
			t.Error("PROCESSING 分支不应入队")
		}
	})

	t.Run("FAILED 重试置 PENDING 并入队", func(t *testing.T) {
		f := newAPIFixture()
		errMsg := "语音面试评估 failed after retry 3"
		e := evalChainSession(5, evalPtr(VoiceEvaluateFailed))
		e.EvaluateError = &errMsg
		id := f.store.addSession(e)

		dto, err := f.svc.RetryEvaluation(context.Background(), id)
		if err != nil {
			t.Fatalf("重试失败: %v", err)
		}
		if dto.EvaluateStatus == nil || *dto.EvaluateStatus != "PENDING" {
			t.Errorf("evaluateStatus = %v, want PENDING", dto.EvaluateStatus)
		}
		if dto.EvaluateStatusUpdatedAt == nil {
			t.Error("PENDING 分支应返回当前时间戳")
		}
		if dto.Evaluation != nil {
			t.Error("PENDING 分支不应内嵌评估")
		}
		// 顺序契约：先置 PENDING 再入队（先例 evaluation_fakes 顺序断言）。
		assertEventOrder(t, f.rec.list(), "update-eval:PENDING", "xadd")
		added := f.stream.addAt(0)
		if added[VoiceEvalFieldSessionID] != strconv.FormatInt(id, 10) || added[VoiceEvalFieldRetryCount] != "0" {
			t.Errorf("入队消息 = %v", added)
		}
	})

	t.Run("PENDING 重复触发同样入队", func(t *testing.T) {
		f := newAPIFixture()
		id := f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluatePending)))

		dto, err := f.svc.RetryEvaluation(context.Background(), id)
		if err != nil {
			t.Fatalf("重试失败: %v", err)
		}
		if dto.EvaluateStatus == nil || *dto.EvaluateStatus != "PENDING" {
			t.Errorf("evaluateStatus = %v, want PENDING", dto.EvaluateStatus)
		}
		if !containsEvent(f.rec.list(), "xadd") {
			t.Error("PENDING 重触发应入队（POST 是显式重试）")
		}
	})
}
