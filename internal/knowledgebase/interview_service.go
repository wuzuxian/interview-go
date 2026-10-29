// 知识库面试服务（票 05，Java KnowledgeBaseInterviewService 逐字移植）：
// 按 category/difficulty 取 ACTIVE 题目、过滤追问池 ≥ followUpCount 的题，
// 不足报 3009（消息含缺口明细）；主问题整体洗牌抽取、追问 Fisher-Yates 局部洗牌；
// 题目组装携带 referenceAnswer/keyPoints/scoringRubric/sourceContext，经
// interview.SessionFromQuestionsCreator 接缝落统一场次。容量接口返回各分类可用题数、
// 各追问档位可用量与可选性。
package knowledgebase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/interview"
)

// maxFollowUpCount 追问档位上限（Java MAX_FOLLOW_UP_COUNT）。
const maxFollowUpCount = 5

// InterviewService 知识库面试服务。
type InterviewService struct {
	store    Store
	sessions interview.SessionFromQuestionsCreator
	logger   *slog.Logger
	// randomInt 随机源接缝（Java ThreadLocalRandom.nextInt(origin, bound) 等价形态），
	// 测试注入确定性函数；nil 时用生产默认。
	randomInt func(origin, bound int) int
}

// NewInterviewService 创建服务。sessions 为文字面试模块的「从题目集合创建场次」接缝。
func NewInterviewService(store Store, sessions interview.SessionFromQuestionsCreator, logger *slog.Logger) *InterviewService {
	return &InterviewService{store: store, sessions: sessions, logger: logger, randomInt: newRandomInt()}
}

// CreateSession POST /api/knowledgebase-interviews/sessions（Java createSession :57-98）。
func (s *InterviewService) CreateSession(ctx context.Context, req CreateInterviewRequest) (*interview.SessionDTO, error) {
	kbID := int64ValueOf(req.KnowledgeBaseID)
	if err := s.requireKnowledgeBase(ctx, kbID); err != nil {
		return nil, err
	}

	category := trimToNullPtr(req.Category)
	difficulty := normalizeInterviewDifficulty(req.Difficulty)
	mainCount := intValueOf(req.MainQuestionCount)
	followUpCount := intValueOf(req.FollowUpCount)

	raw, err := s.selectActiveQuestions(ctx, kbID, category, difficulty)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询知识库题目失败", err)
	}
	sources := toQuestionSources(raw)

	candidates := filterByFollowUpPool(sources, followUpCount)
	if len(candidates) < mainCount {
		return nil, apperr.New(apperr.CodeInterviewQuestionInsufficient,
			buildInsufficientMessage(mainCount, len(candidates), category, difficulty, followUpCount))
	}

	nextInt := s.randomInt
	shuffleQuestionSources(candidates, nextInt)
	questions, err := buildInterviewQuestions(candidates[:mainCount], followUpCount, nextInt)
	if err != nil {
		return nil, err
	}

	s.logger.Info("创建知识库面试",
		slog.Int64("kbId", kbID),
		slog.String("category", valueOrEmpty(category)),
		slog.String("difficulty", difficulty),
		slog.Int("mainQuestions", mainCount),
		slog.Int("totalQuestions", len(questions)))

	// skillId 已不再有业务含义，统一用固定值回填到面试会话表（Java DEFAULT_SKILL_ID）。
	return s.sessions.CreateSessionFromQuestions(ctx, questions, req.LLMProvider,
		stringPtr(QuestionSkillID), &difficulty, kbID, category)
}

// GetCapacity GET /api/knowledgebase/{id}/interview-capacity（Java getCapacity :100-140）：
// allSources 恒按全方向取（容量分布不随请求方向收缩），仅追问档位可用量按方向过滤。
func (s *InterviewService) GetCapacity(ctx context.Context, kbID int64, category *string, difficulty string, mainQuestionCount int) (*KnowledgeBaseInterviewCapacityResponse, error) {
	if err := s.requireKnowledgeBase(ctx, kbID); err != nil {
		return nil, err
	}

	normalizedCategory := trimToNullPtr(category)
	normalizedDifficulty := normalizeInterviewDifficulty(&difficulty)
	allSources, err := s.questionSources(ctx, kbID, nil, normalizedDifficulty)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询知识库题目失败", err)
	}
	scoped := allSources
	if normalizedCategory != nil {
		scoped = filterByCategory(allSources, *normalizedCategory)
	}

	return &KnowledgeBaseInterviewCapacityResponse{
		KnowledgeBaseID:   kbID,
		Category:          normalizedCategory,
		Difficulty:        normalizedDifficulty,
		MainQuestionCount: mainQuestionCount,
		Categories:        calculateCategoryOptions(allSources),
		FollowUpOptions:   calculateFollowUpOptions(scoped, mainQuestionCount),
	}, nil
}

// requireKnowledgeBase 知识库存在性校验：不存在 → 6001（Java BusinessException 逐字）。
func (s *InterviewService) requireKnowledgeBase(ctx context.Context, kbID int64) error {
	if _, err := s.store.FindByID(ctx, kbID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || apperr.Is(err, apperr.CodeKnowledgeBaseNotFound) {
			return ErrKnowledgeBaseNotFound
		}
		return apperr.Wrap(apperr.CodeInternalError, "查询知识库失败", err)
	}
	return nil
}

// selectActiveQuestions 按 category 过滤候选题；category 为空时跨所有方向（Java 逐字）。
func (s *InterviewService) selectActiveQuestions(ctx context.Context, kbID int64, category *string, difficulty string) ([]KnowledgeBaseQuestionEntity, error) {
	if category == nil {
		return s.store.FindQuestionsByKBAndDifficultyAndStatusOrderByUpdatedAtDesc(ctx, kbID, difficulty, QuestionActive)
	}
	return s.store.FindQuestionsByKBAndDifficultyAndCategoryAndStatusOrderByUpdatedAtDesc(ctx, kbID, difficulty, *category, QuestionActive)
}

// questionSources 取题并解析追问池/要点（容量路径的 allSources 组装）。
func (s *InterviewService) questionSources(ctx context.Context, kbID int64, category *string, difficulty string) ([]questionSource, error) {
	raw, err := s.selectActiveQuestions(ctx, kbID, category, difficulty)
	if err != nil {
		return nil, err
	}
	return toQuestionSources(raw), nil
}

// questionSource 候选题与其解析出的要点/可用追问池（Java record QuestionSource）。
type questionSource struct {
	question  KnowledgeBaseQuestionEntity
	keyPoints []string
	followUps []QuestionFollowUpDTO
}

// toQuestionSources 实体列表 → 候选题列表（要点复用题库的 readStringList，
// 追问池走 interview 侧的可用性过滤）。
func toQuestionSources(questions []KnowledgeBaseQuestionEntity) []questionSource {
	sources := make([]questionSource, 0, len(questions))
	for _, q := range questions {
		sources = append(sources, questionSource{
			question:  q,
			keyPoints: readStringList(q.KeyPointsJSON),
			followUps: readUsableFollowUps(q.FollowUpsJSON),
		})
	}
	return sources
}

// ===== 纯逻辑（表驱动单测覆盖）=====

// filterByFollowUpPool 过滤追问池 ≥ followUpCount 的候选题。
func filterByFollowUpPool(sources []questionSource, followUpCount int) []questionSource {
	candidates := make([]questionSource, 0, len(sources))
	for _, source := range sources {
		if len(source.followUps) >= followUpCount {
			candidates = append(candidates, source)
		}
	}
	return candidates
}

// filterByCategory 按方向精确匹配过滤（容量追问档位的作用域收缩，Java equals 逐字）。
func filterByCategory(sources []questionSource, category string) []questionSource {
	scoped := make([]questionSource, 0, len(sources))
	for _, source := range sources {
		if source.question.Category != nil && *source.question.Category == category {
			scoped = append(scoped, source)
		}
	}
	return scoped
}

// shuffleQuestionSources 主问题整体洗牌（Java Collections.shuffle：末端 Fisher-Yates，
// nextInt(0, i+1) 含头不含尾）。
func shuffleQuestionSources(list []questionSource, nextInt func(origin, bound int) int) {
	for i := len(list) - 1; i > 0; i-- {
		j := nextInt(0, i+1)
		list[i], list[j] = list[j], list[i]
	}
}

// newRandomInt 生产用随机源（Java ThreadLocalRandom.nextInt(origin, bound) 等价形态）。
func newRandomInt() func(origin, bound int) int {
	return func(origin, bound int) int {
		if bound <= origin {
			return origin
		}
		return origin + rand.IntN(bound-origin)
	}
}

// buildInterviewQuestions 组装面试题目（Java buildQuestions :156-195 逐字）：
// 主问题携带参考基线与来源上下文；追问在可用池里随机抽 followUpCount 个，
// 指向主问题索引，参考基线来自追问自身、来源上下文沿用主问题实体。
func buildInterviewQuestions(selected []questionSource, followUpCount int,
	nextInt func(origin, bound int) int) ([]interview.QuestionDTO, error) {
	questions := make([]interview.QuestionDTO, 0, len(selected)*(1+followUpCount))
	for _, source := range selected {
		entity := source.question
		mainIndex := len(questions)
		questions = append(questions, interview.QuestionDTO{
			QuestionIndex:   mainIndex,
			Question:        entity.Question,
			Type:            defaultStringPtr(entity.Type, "KNOWLEDGE_BASE"),
			Category:        defaultStringPtr(entity.Category, "知识库"),
			TopicSummary:    entity.TopicSummary,
			ReferenceAnswer: entity.ReferenceAnswer,
			KeyPoints:       source.keyPoints,
			ScoringRubric:   entity.ScoringRubric,
			SourceContext:   entity.SourceContext,
		})

		picked, err := pickFollowUps(source.followUps, followUpCount, nextInt)
		if err != nil {
			return nil, err
		}
		for _, followUp := range picked {
			questions = append(questions, interview.QuestionDTO{
				QuestionIndex:       len(questions),
				Question:            followUp.Question,
				Type:                defaultStringPtr(entity.Type, "KNOWLEDGE_BASE"),
				Category:            defaultStringPtr(entity.Category, "知识库追问"),
				TopicSummary:        entity.TopicSummary,
				IsFollowUp:          true,
				ParentQuestionIndex: intPtr(mainIndex),
				ReferenceAnswer:     followUp.ReferenceAnswer,
				KeyPoints:           followUp.KeyPoints,
				ScoringRubric:       followUp.ScoringRubric,
				SourceContext:       entity.SourceContext,
			})
		}
	}
	return questions, nil
}

// pickFollowUps 在追问池里随机抽取严格的 count 个（Java :201-225 逐字）：
// 池容量不足时拒绝继续组装（3009）；n == 池大小时原序拷贝返回；
// 否则 Fisher-Yates 局部洗牌取前 n 个，不改原列表顺序。
func pickFollowUps(pool []QuestionFollowUpDTO, count int, nextInt func(origin, bound int) int) ([]QuestionFollowUpDTO, error) {
	if count <= 0 {
		return []QuestionFollowUpDTO{}, nil
	}
	if len(pool) < count {
		return nil, apperr.New(apperr.CodeInterviewQuestionInsufficient,
			fmt.Sprintf("追问池在组装面试时发生变化，无法严格抽取 %d 个追问", count))
	}
	copied := make([]QuestionFollowUpDTO, len(pool))
	copy(copied, pool)
	if count == len(pool) {
		return copied, nil
	}
	for i := 0; i < count; i++ {
		j := nextInt(i, len(copied))
		copied[i], copied[j] = copied[j], copied[i]
	}
	return copied[:count], nil
}

// calculateCategoryOptions 各分类可用题数（Java calculateCategoryOptions）：
// 只计方向非空的题，可用题数降序、同数按方向名字典序升序。
func calculateCategoryOptions(sources []questionSource) []CapacityCategoryOption {
	counts := map[string]int{}
	for _, source := range sources {
		category := trimToNullPtr(source.question.Category)
		if category != nil {
			counts[*category]++
		}
	}
	options := make([]CapacityCategoryOption, 0, len(counts))
	for category, count := range counts {
		options = append(options, CapacityCategoryOption{Category: category, AvailableQuestionCount: count})
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].AvailableQuestionCount != options[j].AvailableQuestionCount {
			return options[i].AvailableQuestionCount > options[j].AvailableQuestionCount
		}
		return options[i].Category < options[j].Category
	})
	return options
}

// calculateFollowUpOptions 追问档位 0-5 的可用量与可选性（Java IntStream.rangeClosed 逐字）。
func calculateFollowUpOptions(scoped []questionSource, mainQuestionCount int) []CapacityFollowUpOption {
	options := make([]CapacityFollowUpOption, 0, maxFollowUpCount+1)
	for count := 0; count <= maxFollowUpCount; count++ {
		availableCount := 0
		for _, source := range scoped {
			if len(source.followUps) >= count {
				availableCount++
			}
		}
		options = append(options, CapacityFollowUpOption{
			FollowUpCount:          count,
			AvailableQuestionCount: availableCount,
			Selectable:             mainQuestionCount > 0 && availableCount >= mainQuestionCount,
		})
	}
	return options
}

// buildInsufficientMessage 题量不足的缺口明细（Java :252-264 逐字；
// 方向为空显示「全部方向」）。
func buildInsufficientMessage(requiredCount, availableCount int, category *string, difficulty string, followUpCount int) string {
	direction := "全部方向"
	if category != nil {
		direction = *category
	}
	return "需要 " + strconv.Itoa(requiredCount) + " 道主问题，但只有 " + strconv.Itoa(availableCount) +
		" 道同时满足：方向=" + direction +
		"、难度=" + difficulty +
		"、每题至少 " + strconv.Itoa(followUpCount) + " 个追问"
}

// readUsableFollowUps 解析可用追问池：剔除空题干（trim 后）、题干 trim
// （Java KnowledgeBaseInterviewService.readUsableFollowUps :278-303 逐字）。
func readUsableFollowUps(value *string) []QuestionFollowUpDTO {
	followUps := readFollowUps(value)
	out := make([]QuestionFollowUpDTO, 0, len(followUps))
	for _, followUp := range followUps {
		if strings.TrimSpace(followUp.Question) == "" {
			continue
		}
		out = append(out, QuestionFollowUpDTO{
			Question:        strings.TrimSpace(followUp.Question),
			ReferenceAnswer: followUp.ReferenceAnswer,
			KeyPoints:       followUp.KeyPoints,
			ScoringRubric:   followUp.ScoringRubric,
		})
	}
	return out
}

// normalizeInterviewDifficulty 难度归一：空白 → "mid"（Java InterviewDefaults.DIFFICULTY），
// 其余 trim。
func normalizeInterviewDifficulty(difficulty *string) string {
	if difficulty == nil || strings.TrimSpace(*difficulty) == "" {
		return interview.DefaultDifficulty
	}
	return strings.TrimSpace(*difficulty)
}

// trimToNullPtr 空白归 null（Java trimToNull）。
func trimToNullPtr(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

// defaultStringPtr 空白回退默认值（Java defaultString；非空原值返回，不 trim）。
func defaultStringPtr(value *string, fallback string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return stringPtr(fallback)
	}
	return value
}

func intValueOf(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func int64ValueOf(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
