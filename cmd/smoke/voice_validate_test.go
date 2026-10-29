// 语音冒烟校验逻辑的单元测试（票 10）：voice_config 前置检查判定与评估报告
// 结构校验（七字段键齐全 + 值约束），表驱动 + 中文用例名，只测纯函数外部行为。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValidateVoiceConfigReady(t *testing.T) {
	ok := &voiceConfigView{MaskedAPIKey: "sk-***xyz"}
	missing := &voiceConfigView{MaskedAPIKey: "***"}

	cases := []struct {
		name    string
		asr     *voiceConfigView
		asrErr  error
		tts     *voiceConfigView
		ttsErr  error
		wantMsg string // 非空时期望错误信息包含该片段
	}{
		{
			name: "配置齐全且 Key 有效时通过",
			asr:  ok, tts: ok,
		},
		{
			name: "ASR 配置未初始化时给出清晰指引",
			asr:  nil, asrErr: fmt.Errorf("语音服务配置未初始化"), tts: ok,
			wantMsg: "ASR",
		},
		{
			name: "TTS 配置未初始化时给出清晰指引",
			asr:  ok, tts: nil, ttsErr: fmt.Errorf("语音服务配置未初始化"),
			wantMsg: "TTS",
		},
		{
			name: "ASR Key 缺失（掩码 ***）时报错",
			asr:  missing, tts: ok,
			wantMsg: "ASR",
		},
		{
			name: "TTS Key 缺失（掩码 ***）时报错",
			asr:  ok, tts: missing,
			wantMsg: "TTS",
		},
		{
			name: "ASR 问题优先于 TTS 问题上报",
			asr:  missing, tts: missing,
			wantMsg: "ASR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVoiceConfigReady(tc.asr, tc.asrErr, tc.tts, tc.ttsErr)
			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("应通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误信息应包含 %q，实际: %v", tc.wantMsg, err)
			}
		})
	}
}

// sampleVoiceReport 构造一份合法的语音评估报告 JSON（七字段齐全，
// answers 单条且 AnswerDetail 八字段齐全）。
func sampleVoiceReport(sessionID int64) []byte {
	raw := fmt.Sprintf(`{
		"sessionId": %d,
		"totalQuestions": 1,
		"overallScore": 82,
		"overallFeedback": "回答结构清晰",
		"strengths": ["表达自然"],
		"improvements": ["可补充细节"],
		"answers": [{
			"questionIndex": 0,
			"question": "请介绍一下你自己",
			"category": "行为面",
			"userAnswer": "我叫张三",
			"score": 82,
			"feedback": "表达流畅",
			"referenceAnswer": "参考答案",
			"keyPoints": ["自我定位"]
		}]
	}`, sessionID)
	return []byte(raw)
}

func TestValidateVoiceReport(t *testing.T) {
	cases := []struct {
		name    string
		raw     func() []byte
		wantMsg string // 非空时期望错误信息包含该片段
	}{
		{
			name: "合法报告通过",
			raw:  func() []byte { return sampleVoiceReport(9) },
		},
		{
			name:    "非法 JSON 报错",
			raw:     func() []byte { return []byte("not-json") },
			wantMsg: "JSON",
		},
		{
			name: "缺失必备键报错",
			raw: func() []byte {
				var m map[string]any
				if err := json.Unmarshal(sampleVoiceReport(9), &m); err != nil {
					t.Fatal(err)
				}
				delete(m, "strengths")
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "strengths",
		},
		{
			name:    "sessionId 与场次不符报错",
			raw:     func() []byte { return sampleVoiceReport(8) },
			wantMsg: "sessionId",
		},
		{
			name: "overallScore 越界报错",
			raw: func() []byte {
				var m map[string]any
				_ = json.Unmarshal(sampleVoiceReport(9), &m)
				m["overallScore"] = 150
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "overallScore",
		},
		{
			name: "totalQuestions 与 answers 条数不符报错",
			raw: func() []byte {
				var m map[string]any
				_ = json.Unmarshal(sampleVoiceReport(9), &m)
				m["totalQuestions"] = 2
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "totalQuestions",
		},
		{
			name: "answers 为空报错（冒烟至少完成一轮对话）",
			raw: func() []byte {
				var m map[string]any
				_ = json.Unmarshal(sampleVoiceReport(9), &m)
				m["totalQuestions"] = 0
				m["answers"] = []any{}
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "answers",
		},
		{
			name: "answer 缺必备键报错",
			raw: func() []byte {
				var m map[string]any
				_ = json.Unmarshal(sampleVoiceReport(9), &m)
				answer := m["answers"].([]any)[0].(map[string]any)
				delete(answer, "feedback")
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "feedback",
		},
		{
			name: "answer 的 feedback 为空报错",
			raw: func() []byte {
				var m map[string]any
				_ = json.Unmarshal(sampleVoiceReport(9), &m)
				answer := m["answers"].([]any)[0].(map[string]any)
				answer["feedback"] = "  "
				out, _ := json.Marshal(m)
				return out
			},
			wantMsg: "feedback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVoiceReport(tc.raw(), 9)
			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("应通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误信息应包含 %q，实际: %v", tc.wantMsg, err)
			}
		})
	}
}

func TestVoiceReportWarnings(t *testing.T) {
	// 合法报告（overallFeedback 非空、参考答案齐全）无告警。
	if ws := voiceReportWarnings(sampleVoiceReport(9)); len(ws) != 0 {
		t.Fatalf("合法报告不应有告警，实际: %v", ws)
	}
	// overallFeedback 为空、参考答案为 null（键仍在，Java Lombok 恒输出键）
	// → 各产出一条告警且不判失败。
	var m map[string]any
	_ = json.Unmarshal(sampleVoiceReport(9), &m)
	m["overallFeedback"] = ""
	answer := m["answers"].([]any)[0].(map[string]any)
	answer["referenceAnswer"] = nil
	raw, _ := json.Marshal(m)
	if err := validateVoiceReport(raw, 9); err != nil {
		t.Fatalf("参考答案缺失属软性告警不应判失败: %v", err)
	}
	ws := voiceReportWarnings(raw)
	if len(ws) != 2 {
		t.Fatalf("应产出 2 条告警（总评语为空 + 参考答案缺失），实际 %d: %v", len(ws), ws)
	}
}
