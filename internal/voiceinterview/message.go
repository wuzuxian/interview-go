// 消息落库（Java VoiceInterviewService.saveMessage L244-277 + fillLatestUnansweredQuestion
// L279-291 语义）：每轮一条 DIALOGUE 行；用户话语先尝试回填最近一条「已问未答」
// 消息（fillLatestUnansweredQuestion），回填成功则新行 user_recognized_text 置空；
// 序号 = 非 SUMMARY 消息数 + 1；阶段取会话当前阶段。落库失败仅记日志（Java
// Handler 的 saveMessage 包装 catch）。
package voiceinterview

import (
	"context"
	"log/slog"
)

// SaveTurnMessage 落一轮对话消息（userText/aiText 任一可为空，对齐 Java 双方
// trimToNull 归一）。会话不存在仅告警返回（Java saveMessage L252-255）。
func (s *Service) SaveTurnMessage(ctx context.Context, sessionID int64, userText, aiText string) {
	session, err := s.GetSessionEntity(ctx, sessionID)
	if err != nil {
		s.logger.Error("落库对话消息前查询会话失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return
	}
	if session == nil {
		s.logger.Warn("落库对话消息失败，会话不存在", slog.Int64("sessionId", sessionID))
		return
	}

	normalizedUserText := trimVoiceToNull(userText)
	normalizedAiText := trimVoiceToNull(aiText)

	// 答案回填：normalizedUserText 非空才尝试（Java L256-257 短路）。
	answerAttached := false
	if normalizedUserText != nil {
		attached, err := s.repo.FillLatestUnansweredQuestion(ctx, sessionID, *normalizedUserText)
		if err != nil {
			s.logger.Error("回填最近未答问题失败",
				slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
			return
		}
		answerAttached = attached
		if answerAttached {
			s.logger.Debug("已回填答案到最近的未答问题", slog.Int64("sessionId", sessionID))
		}
	}
	if normalizedAiText == nil {
		return
	}

	count, err := s.repo.CountDialogueMessages(ctx, sessionID)
	if err != nil {
		s.logger.Error("统计对话消息数失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return
	}
	seq := int(count) + 1
	e := &MessageEntity{
		SessionID:          sessionID,
		MessageType:        MessageDialogue,
		Phase:              session.CurrentPhase,
		UserRecognizedText: conditionalVoice(normalizedUserText != nil && !answerAttached, normalizedUserText),
		AiGeneratedText:    normalizedAiText,
		SequenceNum:        &seq,
	}
	if err := s.repo.SaveDialogueMessage(ctx, e); err != nil {
		s.logger.Error("落库对话消息失败",
			slog.Int64("sessionId", sessionID), slog.Int("sequenceNum", seq), slog.String("error", err.Error()))
		return
	}
	s.logger.Debug("对话消息已落库",
		slog.Int64("sessionId", sessionID),
		slog.String("phase", phaseValue(session.CurrentPhase)), slog.Int("sequence", seq))
}

// conditionalVoice 条件取值（cond 为假返回 nil）。
func conditionalVoice(cond bool, v *string) *string {
	if !cond {
		return nil
	}
	return v
}
