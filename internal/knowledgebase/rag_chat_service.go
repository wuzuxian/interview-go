// RAG 会话服务（Java RagChatSessionService 逐字对齐）：
// 创建（kbIds 必填校验、标题自动生成）、列表（置顶优先 + updated_at 倒序）、
// 详情（会话 + 消息两段查询）、改标题、置顶切换、换绑知识库、删除、
// 发消息流式（先同步落库用户消息 + AI 占位，流末回写全文）。
package knowledgebase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// RagChatSessionDTO 创建会话响应（Java SessionDTO record）。
type RagChatSessionDTO struct {
	ID               int64     `json:"id"`
	Title            string    `json:"title"`
	KnowledgeBaseIDs []int64   `json:"knowledgeBaseIds"`
	CreatedAt        time.Time `json:"createdAt"`
}

// RagChatSessionListItemDTO 会话列表项（Java SessionListItemDTO record）。
type RagChatSessionListItemDTO struct {
	ID                 int64      `json:"id"`
	Title              string     `json:"title"`
	MessageCount       *int       `json:"messageCount"`
	KnowledgeBaseNames []string   `json:"knowledgeBaseNames"`
	UpdatedAt          *time.Time `json:"updatedAt"`
	IsPinned           bool       `json:"isPinned"`
}

// RagChatMessageDTO 消息（Java MessageDTO record；type 小写 user/assistant）。
type RagChatMessageDTO struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

// RagChatSessionDetailDTO 会话详情（Java SessionDetailDTO record）。
type RagChatSessionDetailDTO struct {
	ID             int64                      `json:"id"`
	Title          string                     `json:"title"`
	KnowledgeBases []KnowledgeBaseListItemDTO `json:"knowledgeBases"`
	Messages       []RagChatMessageDTO        `json:"messages"`
	CreatedAt      time.Time                  `json:"createdAt"`
	UpdatedAt      *time.Time                 `json:"updatedAt"`
}

// RagChatService RAG 会话服务。
type RagChatService struct {
	store  Store
	chat   ChatStore
	query  *QueryService
	cfg    RagQueryConfig
	logger *slog.Logger
}

// NewRagChatService 创建 RAG 会话服务。
func NewRagChatService(store Store, chat ChatStore, query *QueryService, cfg RagQueryConfig, logger *slog.Logger) *RagChatService {
	return &RagChatService{store: store, chat: chat, query: query, cfg: cfg, logger: logger}
}

// CreateSession 创建会话（Java createSession：逐一校验知识库存在，数量不符 →
// NOT_FOUND「部分知识库不存在」；标题空白自动生成）。
func (s *RagChatService) CreateSession(ctx context.Context, kbIDs []int64, title string) (*RagChatSessionDTO, error) {
	kbs, err := s.store.FindAllByIDs(ctx, kbIDs)
	if err != nil {
		return nil, err
	}
	if len(kbs) != len(kbIDs) {
		return nil, apperr.New(apperr.CodeNotFound, "部分知识库不存在")
	}
	if isBlank(title) {
		title = generateSessionTitle(kbs)
	}
	now := time.Now()
	zero := 0
	notPinned := false
	session := RagChatSessionEntity{
		Title:        title,
		Status:       string(RagChatActive),
		MessageCount: &zero,
		IsPinned:     &notPinned,
		CreatedAt:    now,
		UpdatedAt:    &now,
	}
	if err := s.chat.InsertSession(ctx, &session, kbIDs); err != nil {
		return nil, err
	}
	s.logger.Info("创建 RAG 聊天会话", slog.Int64("id", session.ID), slog.String("title", title))
	return &RagChatSessionDTO{ID: session.ID, Title: title, KnowledgeBaseIDs: kbIDs, CreatedAt: now}, nil
}

// ListSessions 会话列表（置顶优先、updated_at 倒序，含消息数与知识库名）。
func (s *RagChatService) ListSessions(ctx context.Context) ([]RagChatSessionListItemDTO, error) {
	rows, err := s.chat.FindSessionRowsOrderByPinnedUpdatedAtDesc(ctx)
	if err != nil {
		return nil, err
	}
	sessionIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		sessionIDs = append(sessionIDs, row.Session.ID)
	}
	namesBySession, err := s.chat.FindKBNamesBySessionIDs(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	out := make([]RagChatSessionListItemDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, RagChatSessionListItemDTO{
			ID:                 row.Session.ID,
			Title:              row.Session.Title,
			MessageCount:       row.Session.MessageCount,
			KnowledgeBaseNames: namesBySession[row.Session.ID],
			UpdatedAt:          row.Session.UpdatedAt,
			IsPinned:           row.Session.IsPinned != nil && *row.Session.IsPinned,
		})
	}
	return out, nil
}

// GetSessionDetail 会话详情（Java getSessionDetail：会话不存在 → 404「会话不存在」；
// 消息按 message_order 升序；知识库以列表项 DTO 形式输出）。
func (s *RagChatService) GetSessionDetail(ctx context.Context, sessionID int64) (*RagChatSessionDetailDTO, error) {
	row, err := s.chat.FindSessionRow(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages, err := s.chat.FindMessagesBySessionID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	kbs, err := s.store.FindAllByIDs(ctx, row.KBIDs)
	if err != nil {
		return nil, err
	}
	// Java new ArrayList<>(Set) 顺序不稳定，此处按关联 ID 升序输出。
	kbByID := make(map[int64]KnowledgeBaseEntity, len(kbs))
	for _, kb := range kbs {
		kbByID[kb.ID] = kb
	}
	kbDTOs := make([]KnowledgeBaseListItemDTO, 0, len(row.KBIDs))
	for _, id := range row.KBIDs {
		if kb, ok := kbByID[id]; ok {
			dto := toListItemDTO(&kb)
			kbDTOs = append(kbDTOs, dto)
		}
	}
	messageDTOs := make([]RagChatMessageDTO, 0, len(messages))
	for _, m := range messages {
		messageDTOs = append(messageDTOs, toMessageDTO(&m))
	}
	return &RagChatSessionDetailDTO{
		ID:             row.Session.ID,
		Title:          row.Session.Title,
		KnowledgeBases: kbDTOs,
		Messages:       messageDTOs,
		CreatedAt:      row.Session.CreatedAt,
		UpdatedAt:      row.Session.UpdatedAt,
	}, nil
}

// UpdateSessionTitle 改标题（Java updateSessionTitle：不存在 → 404）。
func (s *RagChatService) UpdateSessionTitle(ctx context.Context, sessionID int64, title string) error {
	if err := s.chat.UpdateSessionTitle(ctx, sessionID, title, time.Now()); err != nil {
		return err
	}
	s.logger.Info("更新会话标题", slog.Int64("sessionId", sessionID), slog.String("title", title))
	return nil
}

// ToggleSessionPin 置顶切换（Java togglePin：null 视为 false），返回翻转后的状态。
func (s *RagChatService) ToggleSessionPin(ctx context.Context, sessionID int64) (bool, error) {
	next, err := s.chat.ToggleSessionPin(ctx, sessionID, time.Now())
	if err != nil {
		return false, err
	}
	s.logger.Info("切换会话置顶状态", slog.Int64("sessionId", sessionID), slog.Bool("isPinned", next))
	return next, nil
}

// UpdateSessionKnowledgeBases 换绑知识库（Java updateSessionKnowledgeBases：
// 缺失项静默丢弃）。
func (s *RagChatService) UpdateSessionKnowledgeBases(ctx context.Context, sessionID int64, kbIDs []int64) error {
	if err := s.chat.UpdateSessionKnowledgeBases(ctx, sessionID, kbIDs, time.Now()); err != nil {
		return err
	}
	s.logger.Info("更新会话知识库", slog.Int64("sessionId", sessionID), slog.Any("kbIds", kbIDs))
	return nil
}

// DeleteSession 删除会话（Java deleteSession：不存在 → 404；级联消息与关联）。
func (s *RagChatService) DeleteSession(ctx context.Context, sessionID int64) error {
	if err := s.chat.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	s.logger.Info("删除会话", slog.Int64("sessionId", sessionID))
	return nil
}

// PrepareStreamMessage 发消息前同步落库：用户消息 + AI 占位，返回 AI 消息 ID
// （Java prepareStreamMessage：不存在 → 404「会话不存在」）。
func (s *RagChatService) PrepareStreamMessage(ctx context.Context, sessionID int64, question string) (int64, error) {
	row, err := s.chat.FindSessionRow(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	messageID, err := s.chat.InsertStreamMessages(ctx, sessionID, question, derefIntKb(row.Session.MessageCount), time.Now())
	if err != nil {
		return 0, err
	}
	s.logger.Info("准备流式消息", slog.Int64("sessionId", sessionID), slog.Int64("messageId", messageID))
	return messageID, nil
}

// CompleteStreamMessage 流末回写全文（Java completeStreamMessage：不存在 →
// 404「消息不存在」）。
func (s *RagChatService) CompleteStreamMessage(ctx context.Context, messageID int64, content string) error {
	if err := s.chat.CompleteMessage(ctx, messageID, content, time.Now()); err != nil {
		return err
	}
	s.logger.Info("完成流式消息", slog.Int64("messageId", messageID), slog.Int("contentLength", len([]rune(content))))
	return nil
}

// StreamAnswer 流式回答（Java getStreamAnswer：加载会话 → 历史开关 →
// 最近 10 条完成历史 → answerQuestionStream；不存在 → 404「会话不存在」）。
func (s *RagChatService) StreamAnswer(ctx context.Context, sessionID int64, question string) (<-chan StreamChunk, error) {
	row, err := s.chat.FindSessionRow(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var history []ChatMessage
	if s.cfg.HistoryEnabled {
		history = s.loadHistoryMessages(ctx, sessionID)
	}
	s.logger.Info("加载历史上下文", slog.Int64("sessionId", sessionID), slog.Int("historySize", len(history)))
	return s.query.AnswerQuestionStream(ctx, row.KBIDs, question, history), nil
}

// loadHistoryMessages 最近完成历史（Java loadHistoryMessages）：
// 多取一条（当前轮 user 消息），非空且 >1 时剔除首条（DESC 首条），
// 反转为正序（时间从早到晚），按类型映射 user/assistant。
func (s *RagChatService) loadHistoryMessages(ctx context.Context, sessionID int64) []ChatMessage {
	recent, err := s.chat.FindRecentCompletedMessages(ctx, sessionID, s.cfg.HistoryMaxMessages+1)
	if err != nil {
		s.logger.Warn("加载历史消息失败，本轮无历史上下文", slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return nil
	}
	if len(recent) <= 1 {
		return nil
	}
	historyMessages := recent[1:]
	out := make([]ChatMessage, 0, len(historyMessages))
	for i := len(historyMessages) - 1; i >= 0; i-- {
		m := historyMessages[i]
		role := "user"
		if m.Type == RagChatAssistant {
			role = "assistant"
		}
		out = append(out, ChatMessage{Role: role, Content: m.Content})
	}
	return out
}

// generateSessionTitle 默认标题（Java generateTitle：单个 → 知识库名；
// 多个 → 「N 个知识库对话」；空 → 「新对话」）。
func generateSessionTitle(kbs []KnowledgeBaseEntity) string {
	if len(kbs) == 0 {
		return "新对话"
	}
	if len(kbs) == 1 {
		return kbs[0].Name
	}
	return fmt.Sprintf("%d 个知识库对话", len(kbs))
}

// toMessageDTO 消息实体 → DTO（Java getTypeString：小写类型）。
func toMessageDTO(m *RagChatMessageEntity) RagChatMessageDTO {
	messageType := m.Type
	if messageType == RagChatAssistant {
		messageType = "assistant"
	} else {
		messageType = "user"
	}
	return RagChatMessageDTO{ID: m.ID, Type: messageType, Content: m.Content, CreatedAt: m.CreatedAt}
}
