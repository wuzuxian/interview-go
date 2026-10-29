// 知识库向量检索纯函数缝：kbSearchSQL 组装（kb_id IN 下推 + 阈值下推）与
// filterKBDocsLocal 回退过滤。SQL 执行语义由票 05 smoke 真库兜底。
package vector

import (
	"strings"
	"testing"
)

func TestKbSearchSQL带过滤与阈值(t *testing.T) {
	sql, args := kbSearchSQL([]int64{1, 2, 3}, 20, 0.18)
	if !strings.Contains(sql, "metadata->>'kb_id' IN ('1', '2', '3')") {
		t.Fatalf("SQL 缺 kb_id IN 下推: %s", sql)
	}
	if !strings.Contains(sql, "AND 1 - (embedding <=> ?) >= ?") {
		t.Fatalf("SQL 缺相似度阈值下推: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY embedding <=> ? LIMIT ?") {
		t.Fatalf("SQL 缺排序与 LIMIT: %s", sql)
	}
	// embedding 占位符在调用侧追加，这里只断言阈值与 LIMIT 参数。
	if len(args) != 2 || args[0] != 0.18 || args[1] != 20 {
		t.Fatalf("args = %v", args)
	}
}

func TestKbSearchSQL无过滤与零阈值(t *testing.T) {
	sql, args := kbSearchSQL(nil, 8, 0)
	if strings.Contains(sql, "kb_id") {
		t.Fatalf("空 kbIDs 不应有过滤: %s", sql)
	}
	if strings.Contains(sql, ">=") {
		t.Fatalf("minScore=0 不应有阈值（Java 仅 minScore>0 设置 similarityThreshold）: %s", sql)
	}
	if len(args) != 1 || args[0] != 8 {
		t.Fatalf("args = %v", args)
	}
}

func TestKbSearchSQLTopK下限抬到1(t *testing.T) {
	_, args := kbSearchSQL(nil, 0, 0.28)
	if args[len(args)-1] != 1 {
		t.Fatalf("topK=0 应抬到 1: %v", args)
	}
}

func TestFilterKBDocsLocal过滤与截断(t *testing.T) {
	score := 0.9
	docs := []KBDoc{
		{ID: "a", Metadata: map[string]any{"kb_id": "1"}, Score: &score},
		{ID: "b", Metadata: map[string]any{"kb_id": "2"}, Score: &score},
		{ID: "c", Metadata: map[string]any{"kb_id": 3}, Score: &score},   // 数值形态
		{ID: "d", Metadata: map[string]any{"kb_id": "x"}, Score: &score}, // 非法
		{ID: "e", Metadata: map[string]any{}, Score: &score},             // 缺键
	}
	got := filterKBDocsLocal(docs, []int64{1, 3}, 10)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("过滤结果 = %+v, want a/c", got)
	}
	// 截断 topK。
	all := filterKBDocsLocal(docs, nil, 3)
	if len(all) != 3 {
		t.Fatalf("无过滤截断 = %d, want 3", len(all))
	}
}
