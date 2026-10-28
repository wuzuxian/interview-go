package resume

import (
	"testing"
)

// 前端契约回归：recommendation 为空会导致 AnalysisPanel 崩溃
// （`{s.recommendation || s}` 渲染整个对象），priority 非法会被分组逻辑丢弃。
func TestNormalizeSuggestions(t *testing.T) {
	in := []Suggestion{
		{Category: " 格式 ", Priority: "高", Issue: " 名词不规范 ", Recommendation: "  统一大小写  "},
		{Category: "技能", Priority: "高", Issue: "技能描述过度夸大", Recommendation: ""},   // 触发崩溃的历史脏数据
		{Category: "项目", Priority: "", Issue: "", Recommendation: "按 STAR 法则重写"}, // issue 空
		{Category: "", Priority: "critical", Issue: "缺少量化", Recommendation: ""},  // 全空 recommendation + 非法 priority
		{Category: "内容", Priority: "低"},                                          // issue 与 recommendation 全空，应被丢弃
	}
	out := NormalizeSuggestions(in)

	if len(out) != 4 {
		t.Fatalf("expect 4 suggestions (1 dropped), got %d: %+v", len(out), out)
	}
	if out[0].Category != "格式" || out[0].Priority != "高" || out[0].Issue != "名词不规范" || out[0].Recommendation != "统一大小写" {
		t.Errorf("trim failed: %+v", out[0])
	}
	for i, s := range out {
		if s.Recommendation == "" {
			t.Errorf("suggestion[%d].recommendation is empty (would crash frontend)", i)
		}
		switch s.Priority {
		case "高", "中", "低":
		default:
			t.Errorf("suggestion[%d].priority %q invalid (would be dropped by frontend grouping)", i, s.Priority)
		}
	}
	if out[2].Issue != "改进建议" {
		t.Errorf("empty issue should get placeholder, got %q", out[2].Issue)
	}
	if out[3].Priority != "高" {
		t.Errorf("'critical' should map to 高, got %q", out[3].Priority)
	}
	if out[3].Recommendation == "" {
		t.Errorf("empty recommendation should get fallback text")
	}
}
