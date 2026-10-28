// 语音流式模型接缝契约：*Bridge 满足 voiceinterview.VoiceModelResolver
// （ai.StreamChatModel 与 voiceinterview.StreamChatModel 为同一别名类型）、
// provider 解析语义对齐 Java resolveProviderId。
package aibridge

import (
	"testing"

	"github.com/wuzuxian/interview/internal/voiceinterview"
)

// 编译期契约：*Bridge 满足语音模块的流式模型解析接缝（无需依赖业务模块）。
var _ voiceinterview.VoiceModelResolver = (*Bridge)(nil)

func TestResolveVoiceProviderID默认回退(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		def    string
		wantID string
	}{
		{"空串走默认", "", "default-chat", "default-chat"},
		{"空白走默认", "   ", "default-chat", "default-chat"},
		{"default忽略大小写", "DeFaUlT", "default-chat", "default-chat"},
		{"显式Provider原样", "qwen-max", "default-chat", "qwen-max"},
		{"显式Provider去空白", "  qwen-max  ", "default-chat", "qwen-max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVoiceProviderID(tc.in, tc.def); got != tc.wantID {
				t.Fatalf("resolveVoiceProviderID(%q) = %q, want %q", tc.in, got, tc.wantID)
			}
		})
	}
}
