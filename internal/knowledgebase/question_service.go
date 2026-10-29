// 知识库题库服务（票 04，Java KnowledgeBaseQuestionService 逐字对齐）：
// 列表筛选（status/category/difficulty/keyword）、方向统计、手动建题、更新、
// 改状态、删除 + 出题任务提交（taskId 幂等）与 generation-status 轮询。
package knowledgebase

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/wuzuxian/interview/internal/apperr"
)

// 出题默认值（Java KnowledgeBaseQuestionService 常量）。
const (
	defaultFollowUpCount = 2
	defaultCategoryLimit = 3
)

// QuestionService 题库服务。
type QuestionService struct {
	store    Store
	producer *QuestionGenProducer
	logger   *slog.Logger
}

// NewQuestionService 创建题库服务。
func NewQuestionService(store Store, producer *QuestionGenProducer, logger *slog.Logger) *QuestionService {
	return &QuestionService{store: store, producer: producer, logger: logger}
}

// GenerateQuestionsRequest POST /questions/generate 请求体
// （Java GenerateKnowledgeBaseQuestionsRequest；followUpCount/categoryLimit 可空）。
type GenerateQuestionsRequest struct {
	Difficulty    *string `json:"difficulty"`
	QuestionCount *int    `json:"questionCount"`
	FollowUpCount *int    `json:"followUpCount"`
	CategoryLimit *int    `json:"categoryLimit"`
	LLMProvider   *string `json:"llmProvider"`
}

// CreateQuestionRequest POST /questions 请求体（Java CreateKnowledgeBaseQuestionRequest）。
type CreateQuestionRequest struct {
	Difficulty      *string               `json:"difficulty"`
	Type            *string               `json:"type"`
	Category        *string               `json:"category"`
	Question        *string               `json:"question"`
	TopicSummary    *string               `json:"topicSummary"`
	ReferenceAnswer *string               `json:"referenceAnswer"`
	KeyPoints       []string              `json:"keyPoints"`
	ScoringRubric   *string               `json:"scoringRubric"`
	FollowUps       []QuestionFollowUpDTO `json:"followUps"`
	SourceContext   *string               `json:"sourceContext"`
	Status          *string               `json:"status"`
}

// UpdateQuestionRequest PUT /questions/{id} 请求体（Java UpdateKnowledgeBaseQuestionRequest；
// 各字段 nil = 不更新）。
type UpdateQuestionRequest struct {
	Difficulty      *string               `json:"difficulty"`
	Type            *string               `json:"type"`
	Category        *string               `json:"category"`
	Question        *string               `json:"question"`
	TopicSummary    *string               `json:"topicSummary"`
	ReferenceAnswer *string               `json:"referenceAnswer"`
	KeyPoints       []string              `json:"keyPoints"`
	ScoringRubric   *string               `json:"scoringRubric"`
	FollowUps       []QuestionFollowUpDTO `json:"followUps"`
	SourceContext   *string               `json:"sourceContext"`
	Status          *string               `json:"status"`
}

// ===== 出题任务（Java submitGenerationTask / getGenerationStatus）=====

// SubmitGenerationTask 提交异步出题任务：归一化参数 → createTask（taskId 幂等，
// QUEUED + 配置快照）→ 投递 Redis Stream；投递失败如实回退 getStatus
// （Java sent=false 分支）。
func (s *QuestionService) SubmitGenerationTask(ctx context.Context, kbID int64, req GenerateQuestionsRequest) (*QuestionGenStatusResponse, error) {
	cfg := QuestionGenerationConfig{
		Difficulty:    normalizeDifficulty(deref(req.Difficulty)),
		QuestionCount: maxInt(1, derefIntKb(req.QuestionCount)),
		FollowUpCount: defaultFollowUpCount,
		CategoryLimit: defaultCategoryLimit,
		LLMProvider:   trimToNilPtr(req.LLMProvider),
	}
	if req.FollowUpCount != nil {
		cfg.FollowUpCount = clampInt(*req.FollowUpCount, 0, 5)
	}
	if req.CategoryLimit != nil {
		cfg.CategoryLimit = clampInt(*req.CategoryLimit, 1, 5)
	}
	configJSON, err := writeQuestionGenConfig(cfg)
	if err != nil {
		return nil, err
	}
	taskID := uuid.NewString()
	kb, err := s.store.CreateQuestionGenTask(ctx, kbID, taskID, configJSON, time.Now())
	if err != nil {
		return nil, err
	}
	response := toQuestionGenStatusResponse(kb, &cfg)
	if sent := s.producer.SendGenerateTask(ctx, kbID, taskID, 0); !sent {
		return s.GetGenerationStatus(ctx, kbID)
	}
	s.logger.Info("出题任务已提交",
		slog.Int64("kbId", kbID), slog.String("taskId", taskID), slog.String("difficulty", cfg.Difficulty))
	return response, nil
}

// GetGenerationStatus 出题状态轮询（Java getStatus：NONE 初始、计数 0 兜底）。
func (s *QuestionService) GetGenerationStatus(ctx context.Context, kbID int64) (*QuestionGenStatusResponse, error) {
	kb, err := s.store.FindByID(ctx, kbID)
	if err != nil {
		return nil, err
	}
	cfg, err := readQuestionGenConfig(kb.QuestionGenConfig)
	if err != nil {
		return nil, err
	}
	return toQuestionGenStatusResponse(kb, cfg), nil
}

// toQuestionGenStatusResponse 实体 → 状态响应（Java toResponse）。
func toQuestionGenStatusResponse(kb *KnowledgeBaseEntity, cfg *QuestionGenerationConfig) *QuestionGenStatusResponse {
	status := string(kb.QuestionGenStatus)
	if status == "" {
		status = string(QuestionGenNone)
	}
	return &QuestionGenStatusResponse{
		KnowledgeBaseID:   kb.ID,
		QuestionGenStatus: status,
		QuestionGenTaskID: kb.QuestionGenTaskID,
		QuestionGenConfig: cfg,
		SavedCount:        kb.QuestionGenSavedCount,
		SkippedCount:      kb.QuestionGenSkippedCount,
		Message:           kb.QuestionGenMessage,
		Error:             kb.QuestionGenError,
		UpdatedAt:         kb.QuestionGenUpdatedAt,
	}
}

// ===== 题库 CRUD =====

// ListQuestions 列表筛选（Java listQuestions：先按 status 取数，其余内存过滤）。
func (s *QuestionService) ListQuestions(ctx context.Context, kbID int64, status *QuestionBankStatus, category, difficulty, keyword *string) ([]KnowledgeBaseQuestionDTO, error) {
	var (
		questions []KnowledgeBaseQuestionEntity
		err       error
	)
	if status != nil {
		questions, err = s.store.FindQuestionsByKBAndStatusOrderByUpdatedAtDesc(ctx, kbID, *status)
	} else {
		questions, err = s.store.FindQuestionsByKBOrderByUpdatedAtDesc(ctx, kbID)
	}
	if err != nil {
		return nil, err
	}
	categoryFilter := trimToNull(deref(category))
	difficultyFilter := trimToNull(deref(difficulty))
	keywordFilter := trimToNull(deref(keyword))
	out := make([]KnowledgeBaseQuestionDTO, 0, len(questions))
	for i := range questions {
		q := &questions[i]
		if categoryFilter != nil && (q.Category == nil || *q.Category != *categoryFilter) {
			continue
		}
		if difficultyFilter != nil && (q.Difficulty == nil || *q.Difficulty != *difficultyFilter) {
			continue
		}
		if keywordFilter != nil && !questionContainsKeyword(q, strings.ToLower(*keywordFilter)) {
			continue
		}
		out = append(out, toQuestionDTO(q, nil))
	}
	return out, nil
}

// ListCategories 方向统计（Java listCategories）。
func (s *QuestionService) ListCategories(ctx context.Context, kbID int64) ([]CategoryCount, error) {
	return s.store.FindCategoryCounts(ctx, kbID)
}

// CreateQuestion 手动建题（Java createQuestion：默认 DRAFT、skill_id 固定、
// kbContentHash 快照文件哈希）。
func (s *QuestionService) CreateQuestion(ctx context.Context, kbID int64, req CreateQuestionRequest) (*KnowledgeBaseQuestionDTO, error) {
	kb, err := s.store.FindByID(ctx, kbID)
	if err != nil {
		return nil, err
	}
	if isBlank(deref(req.Category)) {
		return nil, apperr.New(apperr.CodeBadRequest, "面试方向不能为空")
	}
	if req.Question == nil || strings.TrimSpace(*req.Question) == "" {
		return nil, apperr.New(apperr.CodeBadRequest, "题干不能为空")
	}
	now := time.Now()
	q := &KnowledgeBaseQuestionEntity{
		KnowledgeBaseID: kb.ID,
		SkillID:         QuestionSkillID,
		Difficulty:      trimToNull(normalizeDifficulty(deref(req.Difficulty))),
		Type:            trimToNull(deref(req.Type)),
		Category:        trimToNull(normalizeCategory(deref(req.Category), "")),
		Question:        strings.TrimSpace(*req.Question),
		TopicSummary:    trimToNull(deref(req.TopicSummary)),
		ReferenceAnswer: trimToNull(deref(req.ReferenceAnswer)),
		ScoringRubric:   trimToNull(deref(req.ScoringRubric)),
		SourceContext:   trimToNull(deref(req.SourceContext)),
		KBContentHash:   stringPtr(kb.FileHash),
		Status:          QuestionDraft,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	q.KeyPointsJSON = stringPtr(marshalStringList(req.KeyPoints))
	q.FollowUpsJSON = stringPtr(marshalFollowUps(req.FollowUps, -1))
	if req.Status != nil {
		st, perr := parseQuestionStatus(*req.Status)
		if perr != nil {
			return nil, perr
		}
		q.Status = st
	}
	if err := s.store.InsertQuestion(ctx, q); err != nil {
		return nil, err
	}
	dto := toQuestionDTO(q, stringPtr(kb.Name))
	return &dto, nil
}

// UpdateQuestion 更新题目（Java updateQuestion：nil 不更新、空白必填 400）。
func (s *QuestionService) UpdateQuestion(ctx context.Context, questionID int64, req UpdateQuestionRequest) (*KnowledgeBaseQuestionDTO, error) {
	q, err := s.store.FindQuestionByID(ctx, questionID)
	if err != nil {
		return nil, err
	}
	if req.Difficulty != nil {
		q.Difficulty = trimToNull(normalizeDifficulty(*req.Difficulty))
	}
	if req.Type != nil {
		q.Type = trimToNull(*req.Type)
	}
	if req.Category != nil {
		if strings.TrimSpace(*req.Category) == "" {
			return nil, apperr.New(apperr.CodeBadRequest, "面试方向不能为空")
		}
		q.Category = trimToNull(*req.Category)
	}
	if req.Question != nil {
		if strings.TrimSpace(*req.Question) == "" {
			return nil, apperr.New(apperr.CodeBadRequest, "题干不能为空")
		}
		q.Question = strings.TrimSpace(*req.Question)
	}
	if req.TopicSummary != nil {
		q.TopicSummary = trimToNull(*req.TopicSummary)
	}
	if req.ReferenceAnswer != nil {
		q.ReferenceAnswer = trimToNull(*req.ReferenceAnswer)
	}
	if req.KeyPoints != nil {
		q.KeyPointsJSON = stringPtr(marshalStringList(req.KeyPoints))
	}
	if req.ScoringRubric != nil {
		q.ScoringRubric = trimToNull(*req.ScoringRubric)
	}
	if req.FollowUps != nil {
		q.FollowUpsJSON = stringPtr(marshalFollowUps(req.FollowUps, -1))
	}
	if req.SourceContext != nil {
		q.SourceContext = trimToNull(*req.SourceContext)
	}
	if req.Status != nil {
		st, perr := parseQuestionStatus(*req.Status)
		if perr != nil {
			return nil, perr
		}
		q.Status = st
	}
	q.UpdatedAt = time.Now()
	if err := s.store.UpdateQuestion(ctx, q); err != nil {
		return nil, err
	}
	dto := toQuestionDTO(q, nil)
	return &dto, nil
}

// UpdateStatus 改状态（Java updateStatus）。
func (s *QuestionService) UpdateStatus(ctx context.Context, questionID int64, status QuestionBankStatus) (*KnowledgeBaseQuestionDTO, error) {
	q, err := s.store.FindQuestionByID(ctx, questionID)
	if err != nil {
		return nil, err
	}
	q.Status = status
	q.UpdatedAt = time.Now()
	if err := s.store.UpdateQuestion(ctx, q); err != nil {
		return nil, err
	}
	dto := toQuestionDTO(q, nil)
	return &dto, nil
}

// DeleteQuestion 删除题目（Java deleteQuestion：不存在 3003）。
func (s *QuestionService) DeleteQuestion(ctx context.Context, questionID int64) error {
	if _, err := s.store.FindQuestionByID(ctx, questionID); err != nil {
		return err
	}
	return s.store.DeleteQuestionByID(ctx, questionID)
}

// ===== 实体/DTO 转换与序列化（Java toDTO/readStringList/readFollowUps/writeXxx）=====

// toQuestionDTO 实体 → DTO（Java toDTO；keyPoints/followUps 无数据输出空数组）。
func toQuestionDTO(q *KnowledgeBaseQuestionEntity, kbName *string) KnowledgeBaseQuestionDTO {
	return KnowledgeBaseQuestionDTO{
		ID:                q.ID,
		KnowledgeBaseID:   q.KnowledgeBaseID,
		KnowledgeBaseName: kbName,
		SkillID:           q.SkillID,
		Difficulty:        q.Difficulty,
		Type:              q.Type,
		Category:          q.Category,
		Question:          q.Question,
		TopicSummary:      q.TopicSummary,
		ReferenceAnswer:   q.ReferenceAnswer,
		KeyPoints:         readStringList(q.KeyPointsJSON),
		ScoringRubric:     q.ScoringRubric,
		FollowUps:         readFollowUps(q.FollowUpsJSON),
		SourceContext:     q.SourceContext,
		Status:            string(q.Status),
		CreatedAt:         q.CreatedAt,
		UpdatedAt:         q.UpdatedAt,
	}
}

// questionContainsKeyword 关键词命中（Java containsKeyword：题干/参考答案/评分规则/
// 摘要/方向，大小写不敏感；调用方已 lowercase 关键词）。
func questionContainsKeyword(q *KnowledgeBaseQuestionEntity, keyword string) bool {
	return strings.Contains(strings.ToLower(q.Question), keyword) ||
		(q.ReferenceAnswer != nil && strings.Contains(strings.ToLower(*q.ReferenceAnswer), keyword)) ||
		(q.ScoringRubric != nil && strings.Contains(strings.ToLower(*q.ScoringRubric), keyword)) ||
		(q.TopicSummary != nil && strings.Contains(strings.ToLower(*q.TopicSummary), keyword)) ||
		(q.Category != nil && strings.Contains(strings.ToLower(*q.Category), keyword))
}

// parseQuestionStatus 状态解析（枚举值 DRAFT/ACTIVE/ARCHIVED/STALE）。
func parseQuestionStatus(raw string) (QuestionBankStatus, error) {
	switch st := QuestionBankStatus(strings.TrimSpace(raw)); st {
	case QuestionDraft, QuestionActive, QuestionArchived, QuestionStale:
		return st, nil
	default:
		return "", apperr.New(apperr.CodeBadRequest, "无效的题目状态: "+raw)
	}
}

// readStringList 解析题目字符串列表（Java readStringList：损坏降级空列表）。
func readStringList(raw *string) []string {
	if raw == nil || *raw == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// readFollowUps 解析追问列表（Java readFollowUps：损坏降级空列表）。
func readFollowUps(raw *string) []QuestionFollowUpDTO {
	if raw == nil || *raw == "" {
		return []QuestionFollowUpDTO{}
	}
	var out []QuestionFollowUpDTO
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return []QuestionFollowUpDTO{}
	}
	if out == nil {
		out = []QuestionFollowUpDTO{}
	}
	return out
}

// marshalStringList 序列化评分要点（Java writeStringList：过滤空白、trim）。
func marshalStringList(values []string) string {
	sanitized := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			sanitized = append(sanitized, strings.TrimSpace(v))
		}
	}
	b, err := json.Marshal(sanitized)
	if err != nil {
		panic(err) // []string 序列化恒成功
	}
	return string(b)
}

// marshalFollowUps 序列化追问（Java writeFollowUps：过滤空题干、清洗字段；
// limit<0 表示不截断）。
func marshalFollowUps(values []QuestionFollowUpDTO, limit int) string {
	sanitized := make([]QuestionFollowUpDTO, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v.Question) == "" {
			continue
		}
		item := QuestionFollowUpDTO{
			Question:        strings.TrimSpace(v.Question),
			ReferenceAnswer: trimToNull(deref(v.ReferenceAnswer)),
			KeyPoints:       sanitizedKeyPoints(v.KeyPoints),
			ScoringRubric:   trimToNull(deref(v.ScoringRubric)),
		}
		sanitized = append(sanitized, item)
	}
	if limit >= 0 && len(sanitized) > limit {
		sanitized = sanitized[:limit]
	}
	b, err := json.Marshal(sanitized)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// sanitizedKeyPoints 追问要点清洗（过滤空白、trim，null → 空数组）。
func sanitizedKeyPoints(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// ===== 归一化小工具（Java normalizeDifficulty/normalizeCategory/trimToNull）=====

// normalizeDifficulty 难度归一化：空白回退 mid（Java InterviewDefaults.DIFFICULTY）。
func normalizeDifficulty(difficulty string) string {
	if strings.TrimSpace(difficulty) == "" {
		return "mid"
	}
	return strings.TrimSpace(difficulty)
}

// normalizeCategory 方向归一化：空白回退 fallback（非空时）否则「未分类」。
func normalizeCategory(category, fallback string) string {
	if strings.TrimSpace(category) == "" {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback)
		}
		return "未分类"
	}
	return strings.TrimSpace(category)
}

// trimToNilPtr *string 版本 trimToNull（Java trimToNull(request.llmProvider())）。
func trimToNilPtr(value *string) *string {
	if value == nil {
		return nil
	}
	return trimToNull(*value)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
