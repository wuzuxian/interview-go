// 语音冒烟的纯校验逻辑（票 10）：voice_config 前置检查判定与评估报告结构校验，
// 不接触网络与时间，便于单测覆盖失败分支。校验目标来自票面验收项：
//   - 前置检查：voice_config 未配置真实 ASR/TTS Key 时给出清晰错误
//   - 报告必备字段齐全：sessionId/totalQuestions/overallScore/overallFeedback/
//     strengths/improvements/answers 七字段 + AnswerDetail 八字段
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// voiceConfigView GET /api/llm-provider/voice/asr|tts 响应中冒烟关心的字段
//（服务端对 Key 做掩码：MaskAPIKey 长度 ≤6 的 Key 返回 "***"）。
type voiceConfigView struct {
	MaskedAPIKey string `json:"maskedApiKey"`
}

// validateVoiceConfigReady voice 模式前置检查（票面：voice_config 未配置真实
// ASR/TTS Key 时给出清晰错误）：读取失败（未初始化/解密失败）或掩码为 "***"
//（Key 缺失或过短）都判定为不可运行，并指引到设置页配置。
func validateVoiceConfigReady(asr *voiceConfigView, asrErr error, tts *voiceConfigView, ttsErr error) error {
	if asrErr != nil {
		return fmt.Errorf("ASR 配置不可用（%v）；请先在设置页（GET /api/llm-provider/voice/asr 可核对）配置语音识别参数与真实 DashScope API Key", asrErr)
	}
	if asr == nil || strings.TrimSpace(asr.MaskedAPIKey) == "" || asr.MaskedAPIKey == "***" {
		return fmt.Errorf("ASR API Key 未配置；请先在设置页配置真实 DashScope API Key（voice_config）后重试")
	}
	if ttsErr != nil {
		return fmt.Errorf("TTS 配置不可用（%v）；请先在设置页（GET /api/llm-provider/voice/tts 可核对）配置语音合成参数与真实 DashScope API Key", ttsErr)
	}
	if tts == nil || strings.TrimSpace(tts.MaskedAPIKey) == "" || tts.MaskedAPIKey == "***" {
		return fmt.Errorf("TTS API Key 未配置；请先在设置页配置真实 DashScope API Key（voice_config）后重试")
	}
	return nil
}

// voiceReportKeys 评估报告必备键（Java dto/VoiceEvaluationDetailDTO 七字段，
// Lombok 序列化恒输出全部键，缺失即契约破坏）。
var voiceReportKeys = []string{
	"sessionId", "totalQuestions", "overallScore", "overallFeedback",
	"strengths", "improvements", "answers",
}

// voiceAnswerKeys AnswerDetail 八字段（同上，对齐文字面试明细形状）。
var voiceAnswerKeys = []string{
	"questionIndex", "question", "category", "userAnswer",
	"score", "feedback", "referenceAnswer", "keyPoints",
}

// validateVoiceReport 校验语音评估报告结构（票面断言项：评估 COMPLETED、
// 报告必备字段齐全）：七字段键齐全、sessionId 归属本场、totalQuestions 与
// answers 条数一致且 ≥1（冒烟至少完成一轮对话）、overallScore 0-100、
// 逐题 question/feedback 文本非空。
func validateVoiceReport(evaluationRaw []byte, sessionID int64) error {
	var report map[string]json.RawMessage
	if err := json.Unmarshal(evaluationRaw, &report); err != nil {
		return fmt.Errorf("评估报告不是合法 JSON: %v", err)
	}
	for _, key := range voiceReportKeys {
		if _, ok := report[key]; !ok {
			return fmt.Errorf("评估报告缺少必备字段 %q", key)
		}
	}
	var gotSessionID int64
	if err := json.Unmarshal(report["sessionId"], &gotSessionID); err != nil {
		return fmt.Errorf("评估报告 sessionId 非数字: %v", err)
	}
	if gotSessionID != sessionID {
		return fmt.Errorf("评估报告 sessionId=%d 与本场次 %d 不符", gotSessionID, sessionID)
	}
	var totalQuestions int
	if err := json.Unmarshal(report["totalQuestions"], &totalQuestions); err != nil {
		return fmt.Errorf("评估报告 totalQuestions 非数字: %v", err)
	}
	var overallScore *int
	if err := json.Unmarshal(report["overallScore"], &overallScore); err != nil {
		return fmt.Errorf("评估报告 overallScore 类型异常: %v", err)
	}
	if overallScore != nil && (*overallScore < 0 || *overallScore > 100) {
		return fmt.Errorf("overallScore=%d 超出 0-100", *overallScore)
	}
	var answers []map[string]json.RawMessage
	if err := json.Unmarshal(report["answers"], &answers); err != nil {
		return fmt.Errorf("评估报告 answers 类型异常: %v", err)
	}
	if len(answers) == 0 {
		return fmt.Errorf("answers 为空（冒烟至少完成一轮对话，应产出至少一条问答明细）")
	}
	if totalQuestions != len(answers) {
		return fmt.Errorf("totalQuestions=%d 与 answers 条数 %d 不符", totalQuestions, len(answers))
	}
	for i, answer := range answers {
		for _, key := range voiceAnswerKeys {
			if _, ok := answer[key]; !ok {
				return fmt.Errorf("第 %d 条问答缺少必备字段 %q", i, key)
			}
		}
		for _, key := range []string{"question", "feedback"} {
			var text string
			if err := json.Unmarshal(answer[key], &text); err != nil {
				return fmt.Errorf("第 %d 条问答的 %s 非字符串: %v", i, key, err)
			}
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("第 %d 条问答的 %s 为空", i, key)
			}
		}
	}
	return nil
}

// voiceReportWarnings 报告软性告警（不阻断冒烟，仅提示）：总评语为空、逐题
// 参考答案/要点缺失。这些字段由 LLM 生成，偶发缺失不应判定链路失败。
func voiceReportWarnings(evaluationRaw []byte) []string {
	var report struct {
		OverallFeedback string           `json:"overallFeedback"`
		Answers         []map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(evaluationRaw, &report); err != nil {
		return nil
	}
	var warnings []string
	if strings.TrimSpace(report.OverallFeedback) == "" {
		warnings = append(warnings, "overallFeedback 为空（LLM 偶发缺失，建议人工复核）")
	}
	for i, answer := range report.Answers {
		if s, _ := answer["referenceAnswer"].(string); strings.TrimSpace(s) == "" {
			warnings = append(warnings, fmt.Sprintf("第 %d 题参考答案缺失（未命中参考题库或 LLM 偶发缺失）", i))
		}
	}
	return warnings
}
