// RAG 检索编排缝测试：双路召回融合与改写开关（服务级外部行为，
// 检索走 fakeRetriever 脚本）。
package knowledgebase

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/wuzuxian/interview/internal/vector"
)

// newMergeFixture 构造可定制配置的 QueryService（不经 HTTP）。
func newMergeFixture(t *testing.T, mutate func(*RagQueryConfig)) (*QueryService, *fakeRetriever, *fakeChatModel) {
	t.Helper()
	retriever := &fakeRetriever{script: map[string][]vector.KBDoc{}}
	cm := &fakeChatModel{}
	cfg := RagQueryConfig{
		RewriteEnabled:     true,
		MergeOriginalQuery: false,
		ShortQueryLength:   4,
		TopkShort:          20,
		TopkMedium:         12,
		TopkLong:           8,
		MinScoreShort:      0.18,
		MinScoreDefault:    0.28,
		HistoryEnabled:     true,
		HistoryMaxMessages: 10,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewQueryService(func(ctx context.Context) (ChatModel, error) { return cm, nil },
		retriever, newFakeStore(&actionRecorder{}),
		NewListService(newFakeStore(&actionRecorder{}), newFakeStorage(&actionRecorder{}), logger), cfg, logger)
	return svc, retriever, cm
}

func kbDoc(id string, score float64) vector.KBDoc {
	s := score
	return vector.KBDoc{ID: id, Content: "内容-" + id, Metadata: map[string]any{"kb_id": "1"}, Score: &s}
}

func Test双路召回融合顺序与去重断言(t *testing.T) {
	svc, retriever, cm := newMergeFixture(t, func(c *RagQueryConfig) { c.MergeOriginalQuery = true })
	cm.pushGenerate("Go GMP 调度详解")
	retriever.script["Go GMP 调度详解"] = []vector.KBDoc{kbDoc("a", 0.9), kbDoc("b", 0.6), kbDoc("c", 0.5)}
	retriever.script["gmp"] = []vector.KBDoc{kbDoc("b", 0.95), kbDoc("d", 0.4)}

	// 直接走检索编排（不出 footer，便于断言片段顺序）。
	qc, err := svc.buildQueryContext(context.Background(), "gmp", nil)
	if err != nil {
		t.Fatalf("buildQueryContext 失败: %v", err)
	}
	docs, err := svc.retrieveAndMerge(context.Background(), qc, []int64{1})
	if err != nil {
		t.Fatalf("retrieveAndMerge 失败: %v", err)
	}
	want := []struct {
		id    string
		score float64
	}{{"b", 0.95}, {"a", 0.9}, {"c", 0.5}, {"d", 0.4}}
	if len(docs) != len(want) {
		t.Fatalf("融合后片段数 = %d, want %d", len(docs), len(want))
	}
	for i, w := range want {
		if docs[i].ID != w.id || *docs[i].Score != w.score {
			t.Errorf("docs[%d] = %s@%v, want %s@%v（去重保留高分、分数降序）", i, docs[i].ID, *docs[i].Score, w.id, w.score)
		}
	}
}

func Test改写关闭时降级原问题(t *testing.T) {
	svc, retriever, cm := newMergeFixture(t, func(c *RagQueryConfig) { c.RewriteEnabled = false })
	retriever.script["gmp"] = []vector.KBDoc{kbDoc("a", 0.9)}

	docs, err := svc.retrieveRelevantDocs(context.Background(),
		&queryContext{originalQuestion: "gmp", candidateQueries: []string{"gmp"}, params: searchParams{topK: 8, minScore: 0.28}},
		[]int64{1})
	_ = docs
	_ = err
	// 关闭改写：buildQueryContext 产生的候选只有原问题，且不触发 Generate。
	qc, err := svc.buildQueryContext(context.Background(), "gmp", nil)
	if err != nil {
		t.Fatalf("buildQueryContext 失败: %v", err)
	}
	if len(qc.candidateQueries) != 1 || qc.candidateQueries[0] != "gmp" {
		t.Fatalf("关闭改写候选 = %v, want [gmp]", qc.candidateQueries)
	}
	if len(cm.generateInputs) != 0 {
		t.Fatalf("关闭改写不应调用模型")
	}
	calls := retriever.callList()
	if len(calls) != 1 || calls[0].query != "gmp" {
		t.Fatalf("检索调用 = %+v, want 原问题单路", calls)
	}
}
