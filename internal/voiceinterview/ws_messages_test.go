// 下行六类消息的 JSON 形状契约（对齐 Java Handler 的 Map.of 序列化与
// dto/WebSocketSubtitleMessage；键集黄金清单锁死，零值也必须输出）。
package voiceinterview

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// assertMsgShape 断言消息的 JSON 键集与字段值逐字。
func assertMsgShape(t *testing.T, raw []byte, wantKeys []string, wantValues map[string]any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("JSON 键集与 Java 契约不一致\n got: %v\nwant: %v", keys, wantKeys)
	}
	for k, want := range wantValues {
		got, ok := m[k]
		if !ok {
			t.Errorf("缺少键 %q", k)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("键 %q 值不一致: got %#v, want %#v", k, got, want)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Test下行六类消息形状逐字 六类消息的键集与取值逐字对齐 Java。
func Test下行六类消息形状逐字(t *testing.T) {
	t.Run("control welcome", func(t *testing.T) {
		raw := mustJSON(t, buildWelcome())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "welcome", "message": "连接成功，准备开始语音面试"})
	})

	t.Run("control asr_ready", func(t *testing.T) {
		raw := mustJSON(t, buildAsrReady())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "asr_ready", "message": "语音识别已就绪"})
	})

	t.Run("control asr_reconnecting", func(t *testing.T) {
		raw := mustJSON(t, buildAsrReconnecting())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "asr_reconnecting", "message": "语音识别连接较慢，正在自动重连"})
	})

	t.Run("control audio_complete", func(t *testing.T) {
		raw := mustJSON(t, buildAudioComplete())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "audio_complete", "message": "面试官语音播放完成"})
	})

	t.Run("control pause_timeout_warning", func(t *testing.T) {
		raw := mustJSON(t, buildPauseTimeoutWarning())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "pause_timeout_warning", "message": "会话将在30秒后暂停，请继续说话或点击继续"})
	})

	t.Run("control pause_timeout 半角逗号逐字", func(t *testing.T) {
		raw := mustJSON(t, buildPauseTimeout())
		assertMsgShape(t, raw,
			[]string{"action", "message", "timestamp", "type"},
			map[string]any{"type": "control", "action": "pause_timeout", "message": "会话因超时已暂停,可在历史记录中恢复"})
	})

	t.Run("subtitle 最终与非最终均输出三键", func(t *testing.T) {
		final := mustJSON(t, buildSubtitle("已提交的回答", true))
		assertMsgShape(t, final, []string{"isFinal", "text", "type"},
			map[string]any{"type": "subtitle", "text": "已提交的回答", "isFinal": true})
		partial := mustJSON(t, buildSubtitle("", false))
		assertMsgShape(t, partial, []string{"isFinal", "text", "type"},
			map[string]any{"type": "subtitle", "text": "", "isFinal": false})
	})

	t.Run("text 流式与最终", func(t *testing.T) {
		streaming := mustJSON(t, buildTextMessage("第一句", false))
		assertMsgShape(t, streaming, []string{"content", "final", "type"},
			map[string]any{"type": "text", "content": "第一句", "final": false})
		final := mustJSON(t, buildTextMessage("完整回复", true))
		assertMsgShape(t, final, []string{"content", "final", "type"},
			map[string]any{"type": "text", "content": "完整回复", "final": true})
	})

	t.Run("audio base64 整段", func(t *testing.T) {
		raw := mustJSON(t, buildAudioMessage("d2F2", "面试官回复"))
		assertMsgShape(t, raw, []string{"data", "text", "type"},
			map[string]any{"type": "audio", "data": "d2F2", "text": "面试官回复"})
	})

	t.Run("audio_chunk 分句块", func(t *testing.T) {
		raw := mustJSON(t, buildAudioChunkMessage("d2F2", 2, true))
		assertMsgShape(t, raw, []string{"data", "index", "isLast", "type"},
			map[string]any{"type": "audio_chunk", "data": "d2F2", "index": float64(2), "isLast": true})
	})

	t.Run("error", func(t *testing.T) {
		raw := mustJSON(t, buildErrorMessage("语音识别连接中断，请刷新页面后重试"))
		assertMsgShape(t, raw, []string{"message", "type"},
			map[string]any{"type": "error", "message": "语音识别连接中断，请刷新页面后重试"})
	})
}

// Test上行控制消息解析 对齐 Java dto/WebSocketControlMessage（type/action/phase/data，
// 未知字段忽略）。
func Test上行控制消息解析(t *testing.T) {
	raw := []byte(`{"type":"control","action":"submit","phase":"TECH","data":{"text":"修正文本"},"extra":123}`)
	var cm wsControlMessage
	if err := json.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cm.Type != "control" || cm.Action != "submit" || cm.Phase != "TECH" {
		t.Errorf("基础字段解析错误: %+v", cm)
	}
	if text, _ := cm.Data["text"].(string); text != "修正文本" {
		t.Errorf("data.text 解析错误: %v", cm.Data["text"])
	}
}
