package voiceinterview

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// 契约回归测试：请求/响应 DTO 的 JSON 键集以 Java 版 voiceinterview dto/ 包为黄金清单
// （CreateSessionRequest/SessionResponseDTO/SessionMetaDTO/VoiceInterviewMessageDTO，
// Lombok @Data 驼峰序列化，参考实现 /home/wuzuxian/interview-guide 只读原文）。
// 以"键集逐名相等"锁死契约：改名、加 omitempty、删字段都会红灯
// （先例：interview/contract_test.go 的 currentQuestionIndex 事故防线）。

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON 键集与 Java 契约不一致\n got: %v\nwant: %v", got, want)
	}
}

func sortedKeys(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// TestCreateSessionRequestContract 对应 Java dto/CreateSessionRequest.java 的 11 个字段。
func TestCreateSessionRequestContract(t *testing.T) {
	got := jsonKeys(t, CreateSessionRequest{})
	assertKeys(t, got, sortedKeys(
		"roleType", "skillId", "difficulty", "customJdText", "resumeId",
		"introEnabled", "techEnabled", "projectEnabled", "hrEnabled",
		"plannedDuration", "llmProvider",
	))
}

// TestSessionResponseDTOContract 对应 Java dto/SessionResponseDTO.java 的 7 个字段
// （webSocketUrl 为相对路径字段，零值时也必须输出）。
func TestSessionResponseDTOContract(t *testing.T) {
	got := jsonKeys(t, SessionResponseDTO{})
	assertKeys(t, got, sortedKeys(
		"sessionId", "roleType", "currentPhase", "status", "startTime",
		"plannedDuration", "webSocketUrl",
	))
}

// TestSessionMetaDTOContract 对应 Java dto/SessionMetaDTO.java 的十字段。
func TestSessionMetaDTOContract(t *testing.T) {
	got := jsonKeys(t, SessionMetaDTO{})
	assertKeys(t, got, sortedKeys(
		"sessionId", "roleType", "status", "currentPhase", "createdAt", "updatedAt",
		"actualDuration", "messageCount", "evaluateStatus", "evaluateError",
	))
}

// TestVoiceInterviewMessageDTOContract 对应 Java dto/VoiceInterviewMessageDTO.java 的八字段。
func TestVoiceInterviewMessageDTOContract(t *testing.T) {
	got := jsonKeys(t, VoiceInterviewMessageDTO{})
	assertKeys(t, got, sortedKeys(
		"id", "sessionId", "messageType", "phase", "userRecognizedText",
		"aiGeneratedText", "timestamp", "sequenceNum",
	))
}
