// 知识库向量检索纯函数缝：kbSearchSQL 组装（kb_id IN 下推 + 阈值下推 +
// 占位符/参数位置对齐）与 filterKBDocsLocal 回退过滤。
// SQL 执行语义由票 05 smoke 真库兜底；位置对齐契约在此单测锁死
// （回归：2026-10-29 参数错位导致 22P02，见 vector_store.go kbSearchSQL 注释）。
package vector

import (
	"strings"
	"testing"

	"github.com/pgvector/pgvector-go"
)

// assertPlaceholderArgAlignment 断言 SQL 占位符数与参数数一致，且参数按文本序
// 逐一类型匹配——向量位必须是向量、阈值位必须是 float64、LIMIT 位必须是整数。
// 这是参数错位（vector 字符串喂给 double precision 比较）的回归防线。
func assertPlaceholderArgAlignment(t *testing.T, sql string, args []any, wantTypes []string) {
	t.Helper()
	if got := strings.Count(sql, "?"); got != len(args) {
		t.Fatalf("占位符数 %d != 参数数 %d（位置错位前兆）: sql=%s args=%v", got, len(args), sql, args)
	}
	if len(args) != len(wantTypes) {
		t.Fatalf("参数数 %d != 期望类型序列长 %d: args=%v", len(args), len(wantTypes), args)
	}
	for i, want := range wantTypes {
		switch want {
		case "vector":
			if _, ok := args[i].(pgvector.Vector); !ok {
				t.Fatalf("第 %d 位参数应为 pgvector.Vector（向量占位），实为 %T: %v", i+1, args[i], args[i])
			}
		case "float64":
			if _, ok := args[i].(float64); !ok {
				t.Fatalf("第 %d 位参数应为 float64（阈值占位），实为 %T: %v", i+1, args[i], args[i])
			}
		case "int":
			if _, ok := args[i].(int); !ok {
				t.Fatalf("第 %d 位参数应为 int（LIMIT 占位），实为 %T: %v", i+1, args[i], args[i])
			}
		default:
			t.Fatalf("未知期望类型 %q", want)
		}
	}
}

func TestKbSearchSQL带过滤与阈值(t *testing.T) {
	vec := pgvector.NewVector([]float32{0.1, 0.2})
	sql, args := kbSearchSQL(vec, []int64{1, 2, 3}, 20, 0.18)
	if !strings.Contains(sql, "metadata->>'kb_id' IN ('1', '2', '3')") {
		t.Fatalf("SQL 缺 kb_id IN 下推: %s", sql)
	}
	if !strings.Contains(sql, "AND 1 - (embedding <=> ?) >= ?") {
		t.Fatalf("SQL 缺相似度阈值下推: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY embedding <=> ? LIMIT ?") {
		t.Fatalf("SQL 缺排序与 LIMIT: %s", sql)
	}
	// 文本序五占位：SELECT 向量、阈值向量、阈值 float、ORDER BY 向量、LIMIT 整数。
	assertPlaceholderArgAlignment(t, sql, args, []string{"vector", "vector", "float64", "vector", "int"})
	if args[2] != 0.18 || args[4] != 20 {
		t.Fatalf("阈值/LIMIT 参数值错: %v", args)
	}
}

func TestKbSearchSQL无过滤与零阈值(t *testing.T) {
	vec := pgvector.NewVector([]float32{0.1})
	sql, args := kbSearchSQL(vec, nil, 8, 0)
	if strings.Contains(sql, "kb_id") {
		t.Fatalf("空 kbIDs 不应有过滤: %s", sql)
	}
	if strings.Contains(sql, ">=") {
		t.Fatalf("minScore=0 不应有阈值（Java 仅 minScore>0 设置 similarityThreshold）: %s", sql)
	}
	// 三占位：SELECT 向量、ORDER BY 向量、LIMIT 整数（无阈值分支参数数随之收缩）。
	assertPlaceholderArgAlignment(t, sql, args, []string{"vector", "vector", "int"})
	if args[2] != 8 {
		t.Fatalf("LIMIT 参数值错: %v", args)
	}
}

func TestKbSearchSQLTopK下限抬到1(t *testing.T) {
	vec := pgvector.NewVector([]float32{0.1})
	_, args := kbSearchSQL(vec, nil, 0, 0.28)
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
