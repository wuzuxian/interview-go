// 管理接口 HTTP 契约缝（主缝，票 02）：httptest 真路由 + 依赖全 fake，锁
// list/detail/delete/categories/category/uncategorized/set-category/download/search/stats
// 的请求/响应 JSON 字段名与错误码。契约来源：Java KnowledgeBaseController +
// KnowledgeBaseListService + KnowledgeBaseDeleteService 逐字（含 Java 的错误码
// 习惯：详情不存在 500、删除不存在 404、设置分类/下载不存在 6001）。
package knowledgebase

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/result"
)

// doMethod 发送非 multipart 请求并记录响应。
func (h *contractFixture) doMethod(t *testing.T, method, path string, body []byte) {
	t.Helper()
	h.recorder = httptest.NewRecorder()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h.engine.ServeHTTP(h.recorder, req)
}

// resultRaw 解析 Result{code,message,data}，data 保留原始 JSON（数组/标量断言用）。
func (h *contractFixture) resultRaw(t *testing.T) (int, string, json.RawMessage) {
	t.Helper()
	if h.recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d, want 200（统一 Result 封装）", h.recorder.Code)
	}
	raw, err := io.ReadAll(h.recorder.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	var out struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 Result 失败: %v, body=%s", err, raw)
	}
	return out.Code, out.Message, out.Data
}

// dataItems Result.data 解析为对象数组。
func dataItems(t *testing.T, data json.RawMessage) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("解析 data 数组失败: %v, data=%s", err, data)
	}
	return items
}

// seedKB 落一行知识库（可定制字段），返回 ID。
func seedKB(t *testing.T, h *contractFixture, mutate func(*KnowledgeBaseEntity)) int64 {
	t.Helper()
	kb := sampleKB(0)
	if mutate != nil {
		mutate(kb)
	}
	return h.f.store.addKB(kb)
}

// ===== DTO 黄金键集 =====

// TestListItemDTO契约字段名与Java逐字 KnowledgeBaseListItemDTO record 15 字段 +
// KnowledgeBaseStatsDTO record 5 字段。
func TestListItemDTO契约字段名与Java逐字(t *testing.T) {
	assertKeys(t, jsonKeys(t, KnowledgeBaseListItemDTO{}), sortedKeys(
		"id", "name", "category", "originalFilename", "fileSize", "contentType",
		"uploadedAt", "lastAccessedAt", "accessCount", "questionCount",
		"vectorStatus", "vectorError", "chunkCount", "questionGenStatus", "questionGenError",
	))
	assertKeys(t, jsonKeys(t, KnowledgeBaseStatsDTO{}), sortedKeys(
		"totalCount", "totalQuestionCount", "totalAccessCount", "completedCount", "processingCount",
	))
}

// ===== list =====

// TestList契约字段名与时间倒序 默认（无 sortBy）按 uploadedAt DESC，
// 列表项字段名与 Java KnowledgeBaseListItemDTO 逐字。
func TestList契约字段名与时间倒序(t *testing.T) {
	h := newContractFixture(t)
	seedKB(t, h, func(kb *KnowledgeBaseEntity) { kb.Name = "早" })
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "中"
		kb.UploadedAt = time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "晚"
		kb.UploadedAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	})

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/list", nil)
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	items := dataItems(t, data)
	if len(items) != 3 {
		t.Fatalf("列表长度 = %d, want 3", len(items))
	}
	for i, want := range []string{"晚", "中", "早"} {
		if items[i]["name"] != want {
			t.Errorf("items[%d][name] = %v, want %v（uploadedAt 倒序）", i, items[i]["name"], want)
		}
	}
	got := make([]string, 0, len(items[0]))
	for k := range items[0] {
		got = append(got, k)
	}
	assertKeys(t, sortedKeys(got...), sortedKeys(
		"id", "name", "category", "originalFilename", "fileSize", "contentType",
		"uploadedAt", "lastAccessedAt", "accessCount", "questionCount",
		"vectorStatus", "vectorError", "chunkCount", "questionGenStatus", "questionGenError",
	))
}

// TestList契约空列表输出空数组 空库 data 为 [] 而非 null（Java 空列表序列化）。
func TestList契约空列表输出空数组(t *testing.T) {
	h := newContractFixture(t)
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/list", nil)
	_, _, data := h.resultRaw(t)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Errorf("空列表 data = %s, want []", data)
	}
}

// TestList契约vectorStatus过滤 过滤、小写解析与非法值 500。
func TestList契约vectorStatus过滤(t *testing.T) {
	h := newContractFixture(t)
	seedKB(t, h, func(kb *KnowledgeBaseEntity) { kb.Name = "待处理一" })
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "待处理二"
		kb.UploadedAt = time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "已完成"
		kb.VectorStatus = VectorCompleted
		kb.UploadedAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	})

	// 大写枚举过滤。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/list?vectorStatus=PENDING", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	items := dataItems(t, data)
	if len(items) != 2 || items[0]["name"] != "待处理二" || items[1]["name"] != "待处理一" {
		t.Errorf("PENDING 过滤结果 = %v", items)
	}

	// 小写入参（Java valueOf(toUpperCase)）。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/list?vectorStatus=completed", nil)
	code, _, data = h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	if items = dataItems(t, data); len(items) != 1 || items[0]["name"] != "已完成" {
		t.Errorf("completed 过滤结果 = %v", items)
	}

	// 非法值 → Java Result.error("无效的向量化状态: BOGUS") 固定 500。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/list?vectorStatus=BOGUS", nil)
	code, message, _ := h.resultRaw(t)
	if code != 500 || message != "无效的向量化状态: BOGUS" {
		t.Errorf("code = %d, message = %q, want 500/无效的向量化状态: BOGUS", code, message)
	}
}

// TestList契约sortBy排序 表驱动：size/access/question 内存降序，time/未知值保持时间序。
func TestList契约sortBy排序(t *testing.T) {
	h := newContractFixture(t)
	base := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	// A(09:00) B(10:00) C(11:00) D(12:00)：默认时间序 D C B A。
	specs := []struct {
		name     string
		size     int64
		access   int
		question int
	}{
		{"甲", 100, 1, 9},
		{"乙", 400, 4, 2},
		{"丙", 300, 3, 8},
		{"丁", 200, 2, 1},
	}
	for i, sp := range specs {
		sp := sp
		seedKB(t, h, func(kb *KnowledgeBaseEntity) {
			kb.Name = sp.name
			kb.FileSize = int64Ptr(sp.size)
			kb.AccessCount = intPtr(sp.access)
			kb.QuestionCount = intPtr(sp.question)
			kb.UploadedAt = base.Add(time.Duration(i) * time.Hour)
		})
	}

	cases := []struct {
		sortBy string
		want   []string
	}{
		{"", []string{"丁", "丙", "乙", "甲"}},
		{"time", []string{"丁", "丙", "乙", "甲"}},
		{"TIME", []string{"丁", "丙", "乙", "甲"}},
		{"size", []string{"乙", "丙", "丁", "甲"}},
		{"access", []string{"乙", "丙", "丁", "甲"}},
		{"question", []string{"甲", "丙", "乙", "丁"}},
		{"unknown", []string{"丁", "丙", "乙", "甲"}},
	}
	for _, tc := range cases {
		t.Run("sortBy="+tc.sortBy, func(t *testing.T) {
			path := "/api/knowledgebase/list"
			if tc.sortBy != "" {
				path += "?sortBy=" + tc.sortBy
			}
			h.doMethod(t, http.MethodGet, path, nil)
			code, _, data := h.resultRaw(t)
			if code != result.CodeSuccess {
				t.Fatalf("code = %d", code)
			}
			items := dataItems(t, data)
			got := make([]string, 0, len(items))
			for _, it := range items {
				got = append(got, it["name"].(string))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("长度 = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("排序 = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// ===== detail =====

// Test详情契约成功与不存在 详情字段逐字；不存在按 Java Result.error(String) 固定 500。
func Test详情契约成功与不存在(t *testing.T) {
	h := newContractFixture(t)
	id := seedKB(t, h, func(kb *KnowledgeBaseEntity) { kb.Name = "详情项" })

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id), nil)
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	var item map[string]any
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("data = %s", data)
	}
	if item["name"] != "详情项" || item["vectorStatus"] != "PENDING" || item["questionGenStatus"] != "NONE" {
		t.Errorf("详情 = %v", item)
	}
	got := make([]string, 0, len(item))
	for k := range item {
		got = append(got, k)
	}
	assertKeys(t, sortedKeys(got...), sortedKeys(
		"id", "name", "category", "originalFilename", "fileSize", "contentType",
		"uploadedAt", "lastAccessedAt", "accessCount", "questionCount",
		"vectorStatus", "vectorError", "chunkCount", "questionGenStatus", "questionGenError",
	))

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/999", nil)
	code, message, data = h.resultRaw(t)
	if code != 500 || message != "知识库不存在" || string(data) != "null" {
		t.Errorf("不存在: code = %d, message = %q, data = %s（Java 契约固定 500）", code, message, data)
	}
}

// ===== delete =====

// Test删除契约补偿顺序 事务内摘关联删记录 → 事务外删向量 → 删对象存储文件，
// 任何失败不回滚已删记录（Java KnowledgeBaseDeleteService 顺序逐字）。
func Test删除契约补偿顺序(t *testing.T) {
	h := newContractFixture(t)
	id := seedKB(t, h, nil)
	otherID := seedKB(t, h, nil)
	key := "knowledgebases/2026/10/29/abcd1234_面试笔记.md"
	h.f.storage.putObject(key, []byte("# 内容"))
	// 向量：本知识库 2 条 + 其他知识库 1 条。
	h.f.store.mu.Lock()
	h.f.store.vectors = append(h.f.store.vectors,
		vectorRow{content: "a", metadata: map[string]any{metadataKBID: itoa(id)}},
		vectorRow{content: "b", metadata: map[string]any{metadataKBID: itoa(id)}},
		vectorRow{content: "c", metadata: map[string]any{metadataKBID: itoa(otherID)}})
	h.f.store.mu.Unlock()
	// RAG 会话关联：本知识库挂会话 1/2，其他知识库也挂会话 1。
	h.f.store.addRagAssociation(id, 1)
	h.f.store.addRagAssociation(id, 2)
	h.f.store.addRagAssociation(otherID, 1)

	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/"+itoa(id), nil)
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" || string(data) != "null" {
		t.Fatalf("code = %d, message = %q, data = %s", code, message, data)
	}

	// 补偿顺序：记录删除 → 向量删除 → 对象存储删除。
	events := h.f.rec.list()
	assertEventOrder(t, events, "delete-kb-records:"+itoa(id), "delete-vectors-by-kb:"+itoa(id))
	assertEventOrder(t, events, "delete-vectors-by-kb:"+itoa(id), "delete-storage-object:"+key)

	if h.f.store.snapshot(id) != nil {
		t.Error("知识库记录应已删除")
	}
	if h.f.store.ragAssociationCount(id) != 0 {
		t.Error("本知识库的 RAG 会话关联应已摘除")
	}
	if h.f.store.ragAssociationCount(otherID) != 1 {
		t.Error("其他知识库的 RAG 会话关联不应被波及")
	}
	if n := h.f.vectors.rowCount(); n != 1 {
		t.Errorf("剩余向量数 = %d, want 1（其他知识库向量保留）", n)
	}
	if h.f.storage.has(key) {
		t.Error("对象存储文件应已删除")
	}
}

// Test删除契约向量失败不阻断 向量删除失败仅记日志，流程继续删对象存储文件。
func Test删除契约向量失败不阻断(t *testing.T) {
	h := newContractFixture(t)
	id := seedKB(t, h, nil)
	h.f.store.deleteVectorsErr = errDBDown

	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/"+itoa(id), nil)
	code, message, _ := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q（向量失败不阻断）", code, message)
	}
	events := h.f.rec.list()
	assertEventOrder(t, events, "delete-vectors-by-kb:"+itoa(id), "delete-storage-object:")
}

// Test删除契约存储失败仍成功 对象存储删除失败留 storageKey 供人工补偿，响应仍 200。
func Test删除契约存储失败仍成功(t *testing.T) {
	h := newContractFixture(t)
	id := seedKB(t, h, nil)
	h.f.storage.deleteErr = errDBDown

	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/"+itoa(id), nil)
	code, message, _ := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" {
		t.Fatalf("code = %d, message = %q（存储失败仅告警）", code, message)
	}
	if h.f.store.snapshot(id) != nil {
		t.Error("记录应已删除")
	}
}

// Test删除契约不存在404 Java BusinessException(NOT_FOUND, "知识库不存在") → 404。
func Test删除契约不存在404(t *testing.T) {
	h := newContractFixture(t)
	h.doMethod(t, http.MethodDelete, "/api/knowledgebase/999", nil)
	code, message, _ := h.resultRaw(t)
	if code != 404 || message != "知识库不存在" {
		t.Errorf("code = %d, message = %q, want 404/知识库不存在", code, message)
	}
	if containsEvent(h.f.rec.list(), "delete-vectors-by-kb") {
		t.Error("记录不存在时不应继续补偿链")
	}
}

// ===== categories / category / uncategorized / set-category =====

// Test分类契约 分类列表去重升序、按分类浏览与未分类。
func Test分类契约(t *testing.T) {
	// 空库分类列表输出 []（Java 空列表序列化）。
	h0 := newContractFixture(t)
	h0.doMethod(t, http.MethodGet, "/api/knowledgebase/categories", nil)
	_, _, data := h0.resultRaw(t)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Errorf("空库 categories = %s, want []", data)
	}

	h := newContractFixture(t)
	base := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	java := "Java"
	goLang := "Go"
	seedKB(t, h, func(kb *KnowledgeBaseEntity) { kb.Category = &java; kb.UploadedAt = base })
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Category = &goLang
		kb.Name = "Go资料"
		kb.UploadedAt = base.Add(time.Hour)
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Category = &java
		kb.Name = "Java资料"
		kb.UploadedAt = base.Add(2 * time.Hour)
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "未分类"
		kb.UploadedAt = base.Add(3 * time.Hour)
	})

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/categories", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	var cats []string
	if err := json.Unmarshal(data, &cats); err != nil {
		t.Fatalf("data = %s", data)
	}
	if len(cats) != 2 || cats[0] != "Go" || cats[1] != "Java" {
		t.Errorf("categories = %v, want [Go Java]（去重升序）", cats)
	}

	// Java 分类两条，uploadedAt 倒序：Java资料(11:00) → 面试笔记(09:00)。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/category/Java", nil)
	_, _, data = h.resultRaw(t)
	items := dataItems(t, data)
	if len(items) != 2 || items[0]["name"] != "Java资料" || items[1]["name"] != "面试笔记" {
		t.Errorf("Java 分类浏览 = %v", items)
	}

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/uncategorized", nil)
	_, _, data = h.resultRaw(t)
	items = dataItems(t, data)
	if len(items) != 1 || items[0]["name"] != "未分类" {
		t.Errorf("未分类浏览 = %v", items)
	}
}

// Test设置分类契约 设置/空白置空/不存在 6001。
func Test设置分类契约(t *testing.T) {
	h := newContractFixture(t)
	id := seedKB(t, h, nil)

	// 正常设置；data 为 null（Result.success(null)）。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/"+itoa(id)+"/category", []byte(`{"category":"算法"}`))
	code, message, data := h.resultRaw(t)
	if code != result.CodeSuccess || message != "success" || string(data) != "null" {
		t.Fatalf("code = %d, message = %q, data = %s", code, message, data)
	}
	if kb := h.f.store.snapshot(id); kb.Category == nil || *kb.Category != "算法" {
		t.Errorf("category = %v, want 算法", kb.Category)
	}

	// 空白 → NULL（Java isBlank 分支）；非空白保留原值不 trim（Java 原样保存）。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/"+itoa(id)+"/category", []byte(`{"category":" "}`))
	h.resultRaw(t)
	if kb := h.f.store.snapshot(id); kb.Category != nil {
		t.Errorf("空白分类应置 NULL, got %v", *kb.Category)
	}
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/"+itoa(id)+"/category", []byte(`{"category":" 云原生 "}`))
	h.resultRaw(t)
	if kb := h.f.store.snapshot(id); kb.Category == nil || *kb.Category != " 云原生 " {
		t.Errorf("非空白分类应原样保存, got %v", kb.Category)
	}

	// category 键缺失/为 null → 置 NULL（Java body.get("category") == null）。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/"+itoa(id)+"/category", []byte(`{}`))
	h.resultRaw(t)
	if kb := h.f.store.snapshot(id); kb.Category != nil {
		t.Errorf("缺失 category 应置 NULL, got %v", *kb.Category)
	}

	// 不存在 → 6001「知识库不存在」。
	h.doMethod(t, http.MethodPut, "/api/knowledgebase/999/category", []byte(`{"category":"算法"}`))
	code, message, _ = h.resultRaw(t)
	if code != 6001 || message != "知识库不存在" {
		t.Errorf("code = %d, message = %q, want 6001/知识库不存在", code, message)
	}
}

// ===== search =====

// Test搜索契约 关键词匹配名称/原文件名（不区分大小写），空白返回全量。
func Test搜索契约(t *testing.T) {
	h := newContractFixture(t)
	base := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "Go面试宝典"
		kb.OriginalFilename = "golang.md"
		kb.UploadedAt = base
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.Name = "Java笔记"
		kb.OriginalFilename = "java.md"
		kb.UploadedAt = base.Add(time.Hour)
	})

	// 名称命中（大小写不敏感）。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/search?keyword=面试", nil)
	_, _, data := h.resultRaw(t)
	if items := dataItems(t, data); len(items) != 1 || items[0]["name"] != "Go面试宝典" {
		t.Errorf("面试 搜索 = %v", items)
	}
	// 原文件名命中 + 大写关键词。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/search?keyword=GOLANG", nil)
	_, _, data = h.resultRaw(t)
	if items := dataItems(t, data); len(items) != 1 || items[0]["originalFilename"] != "golang.md" {
		t.Errorf("GOLANG 搜索 = %v", items)
	}
	// 无命中 → 空数组。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/search?keyword=不存在词", nil)
	_, _, data = h.resultRaw(t)
	if items := dataItems(t, data); len(items) != 0 {
		t.Errorf("无命中 = %v, want []", items)
	}
	// 空白 → 全量（时间倒序）。
	h.doMethod(t, http.MethodGet, "/api/knowledgebase/search?keyword=", nil)
	_, _, data = h.resultRaw(t)
	items := dataItems(t, data)
	if len(items) != 2 || items[0]["name"] != "Java笔记" || items[1]["name"] != "Go面试宝典" {
		t.Errorf("空白关键词 = %v, want 全量时间倒序", items)
	}
}

// ===== stats =====

// Test统计契约 总数/USER 消息数口径的提问总数/访问总数/完成数/处理中数。
func Test统计契约(t *testing.T) {
	h := newContractFixture(t)
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.AccessCount = intPtr(2)
		kb.VectorStatus = VectorCompleted
	})
	seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.AccessCount = intPtr(3)
		kb.VectorStatus = VectorProcessing
	})
	h.f.store.addUserRagMessages(7)

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/stats", nil)
	code, _, data := h.resultRaw(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	var stats map[string]any
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatalf("data = %s", data)
	}
	want := map[string]float64{
		"totalCount": 2, "totalQuestionCount": 7, "totalAccessCount": 5,
		"completedCount": 1, "processingCount": 1,
	}
	for k, v := range want {
		if stats[k] != v {
			t.Errorf("stats[%s] = %v, want %v", k, stats[k], v)
		}
	}
	got := make([]string, 0, len(stats))
	for k := range stats {
		got = append(got, k)
	}
	assertKeys(t, sortedKeys(got...), sortedKeys(
		"totalCount", "totalQuestionCount", "totalAccessCount", "completedCount", "processingCount",
	))

	// 空库全零（StatsDTO 字段恒输出，数值型不缺席）。
	h2 := newContractFixture(t)
	h2.doMethod(t, http.MethodGet, "/api/knowledgebase/stats", nil)
	_, _, data = h2.resultRaw(t)
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatalf("data = %s", data)
	}
	for k := range want {
		if stats[k] != float64(0) {
			t.Errorf("空库 stats[%s] = %v, want 0", k, stats[k])
		}
	}
}

// ===== download =====

// Test下载契约头与字节 Content-Disposition UTF-8 文件名、Content-Type 与原始字节直出
// （Java ResponseEntity<byte[]>，不走 Result 封装）。
func Test下载契约头与字节(t *testing.T) {
	h := newContractFixture(t)
	key := "knowledgebases/2026/10/29/abcd1234_面试笔记.md"
	id := seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.OriginalFilename = "面试笔记.md"
		kb.ContentType = stringPtr("text/markdown")
		kb.StorageKey = stringPtr(key)
	})
	h.f.storage.putObject(key, []byte("# 知识库内容"))

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/download", nil)
	if h.recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d, want 200", h.recorder.Code)
	}
	wantDisposition := `attachment; filename="%E9%9D%A2%E8%AF%95%E7%AC%94%E8%AE%B0.md"; filename*=UTF-8''%E9%9D%A2%E8%AF%95%E7%AC%94%E8%AE%B0.md`
	if got := h.recorder.Header().Get("Content-Disposition"); got != wantDisposition {
		t.Errorf("Content-Disposition = %q\nwant %q", got, wantDisposition)
	}
	if got := h.recorder.Header().Get("Content-Type"); got != "text/markdown" {
		t.Errorf("Content-Type = %q, want text/markdown", got)
	}
	body, _ := io.ReadAll(h.recorder.Body)
	if string(body) != "# 知识库内容" {
		t.Errorf("响应体 = %q, want 原始字节", body)
	}
}

// Test下载契约contentType缺省_octet_stream（Java MediaType.APPLICATION_OCTET_STREAM_VALUE）。
func Test下载契约contentType缺省_octet_stream(t *testing.T) {
	h := newContractFixture(t)
	key := "knowledgebases/2026/10/29/x.bin"
	id := seedKB(t, h, func(kb *KnowledgeBaseEntity) {
		kb.OriginalFilename = "x.bin"
		kb.ContentType = nil
		kb.StorageKey = stringPtr(key)
	})
	h.f.storage.putObject(key, []byte("bin"))

	h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/download", nil)
	if got := h.recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
}

// Test下载契约错误路径 表驱动：不存在 6001、storageKey 缺失 4002、对象缺失 4002。
func Test下载契约错误路径(t *testing.T) {
	t.Run("不存在6001", func(t *testing.T) {
		h := newContractFixture(t)
		h.doMethod(t, http.MethodGet, "/api/knowledgebase/999/download", nil)
		code, message, _ := h.resultRaw(t)
		if code != 6001 || message != "知识库不存在" {
			t.Errorf("code = %d, message = %q, want 6001/知识库不存在", code, message)
		}
	})
	t.Run("存储信息缺失4002", func(t *testing.T) {
		h := newContractFixture(t)
		id := seedKB(t, h, func(kb *KnowledgeBaseEntity) { kb.StorageKey = nil })
		h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/download", nil)
		code, message, _ := h.resultRaw(t)
		if code != 4002 || message != "文件存储信息不存在" {
			t.Errorf("code = %d, message = %q, want 4002/文件存储信息不存在", code, message)
		}
	})
	t.Run("对象不存在4002", func(t *testing.T) {
		h := newContractFixture(t)
		id := seedKB(t, h, nil) // StorageKey 有值但对象存储无该对象。
		h.doMethod(t, http.MethodGet, "/api/knowledgebase/"+itoa(id)+"/download", nil)
		code, message, _ := h.resultRaw(t)
		want := "文件不存在: " + *sampleKB(0).StorageKey
		if code != 4002 || message != want {
			t.Errorf("code = %d, message = %q, want 4002/%s", code, message, want)
		}
	})
}

// Test下载文件名编码表驱动 javaURLEncode 逐字节对齐
// java.net.URLEncoder.encode(s, UTF_8) + replace("+", "%20")。
func Test下载文件名编码表驱动(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"面试笔记.md", "%E9%9D%A2%E8%AF%95%E7%AC%94%E8%AE%B0.md"},
		{"my notes (1).md", "my%20notes%20%281%29.md"},
		{"a+b.md", "a%2Bb.md"},
		{"report*final_v2.docx", "report*final_v2.docx"},
		{"纯ASCII.txt", "%E7%BA%AFASCII.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := javaURLEncode(tc.in); got != tc.want {
				t.Errorf("javaURLEncode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// errDBDown 通用下游故障（fake 脚本化失败用）。
var errDBDown = apperr.New(apperr.CodeInternalError, "db down")
