// RAG 会话接口 HTTP 契约缝（主缝）+ 假上游服务缝：/api/rag-chat 8 接口。
// 锁 Result JSON 字段名（Java RagChatDTO record 逐字）、404/400 错误契约、
// 发消息流式的 SSE 换行转义、用户消息 + AI 占位落库、完成/取消/错误回写、
// 多轮历史取最近 10 条完成消息（排除当前轮 user）。
package knowledgebase

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// doJSON 发请求并解析 Result 信封。
func (f *ragFixture) doJSON(t *testing.T, method, path, body string) (int, string, map[string]any, *http.Response) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var out struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    interface{} `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 Result 失败: %v, body=%s", err, raw)
	}
	var dataMap map[string]any
	if m, ok := out.Data.(map[string]any); ok {
		dataMap = m
	}
	return out.Code, out.Message, dataMap, resp
}

// getRaw GET 请求返回原始响应体。
func (f *ragFixture) getRaw(t *testing.T, method, path string) []byte {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	return raw
}

// completedOf 消息完成态。
func completedOf(m *RagChatMessageEntity) bool { return m.Completed != nil && *m.Completed }

// createSession 创建一个会话，返回 ID。
func (f *ragFixture) createSession(t *testing.T, kbIDs []int64, title string) int64 {
	t.Helper()
	ids := make([]string, 0, len(kbIDs))
	for _, id := range kbIDs {
		ids = append(ids, itoa(id))
	}
	body := `{"knowledgeBaseIds":[` + strings.Join(ids, ",") + `]`
	if title != "" {
		body += `,"title":"` + title + `"`
	}
	body += `}`
	code, message, data, _ := f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions", body)
	if code != 200 || message != "success" {
		t.Fatalf("创建会话失败: code=%d, message=%q", code, message)
	}
	return int64(data["id"].(float64))
}

// streamChat 对会话发消息并读出全部 SSE 事件（转义已在 wire 上，不反转义）。
func (f *ragFixture) streamChat(t *testing.T, sessionID int64, question string) []string {
	t.Helper()
	resp := f.postJSON(t, "/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream",
		`{"question":"`+question+`"}`)
	return rawSSELines(t, resp)
}

func TestRagChat创建会话契约字段与自动标题(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "面试笔记", "笔记.md")

	// 单库：标题缺省取知识库名。
	code, _, data, _ := f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions",
		`{"knowledgeBaseIds":[`+itoa(id1)+`]}`)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	for _, key := range []string{"id", "title", "knowledgeBaseIds", "createdAt"} {
		if _, ok := data[key]; !ok {
			t.Errorf("SessionDTO 缺字段 %q: %v", key, data)
		}
	}
	if data["title"] != "面试笔记" {
		t.Errorf("自动标题 = %v, want 知识库名", data["title"])
	}
	ids := data["knowledgeBaseIds"].([]interface{})
	if len(ids) != 1 || ids[0] != float64(id1) {
		t.Errorf("knowledgeBaseIds = %v", ids)
	}

	// 显式标题优先。
	f.cm.pushGenerate("改写")
	f.retriever.script["改写"] = ragDocKB(t, id1, "面试笔记", "笔记.md", 0, 1, "c", 0.9)
	_, _, data, _ = f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions",
		`{"knowledgeBaseIds":[`+itoa(id1)+`],"title":"我的备战"}`)
	if data["title"] != "我的备战" {
		t.Errorf("显式标题 = %v", data["title"])
	}

	// 多库：默认标题 N 个知识库对话。
	id2 := f.seedKBRAG(t, "八股文", "八股.md")
	_, _, data, _ = f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions",
		`{"knowledgeBaseIds":[`+itoa(id1)+`,`+itoa(id2)+`]}`)
	if data["title"] != "2 个知识库对话" {
		t.Errorf("多库默认标题 = %v", data["title"])
	}
}

func TestRagChat创建会话参数校验与缺失知识库(t *testing.T) {
	f := newRagFixture(t)
	code, message, _, _ := f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions",
		`{"knowledgeBaseIds":[]}`)
	if code != 400 || message != "至少选择一个知识库" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	code, message, _, _ = f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions",
		`{"knowledgeBaseIds":[999]}`)
	if code != 404 || message != "部分知识库不存在" {
		t.Fatalf("code = %d, message = %q, want 404/部分知识库不存在", code, message)
	}
}

func TestRagChat会话列表置顶优先与字段名(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	id2 := f.seedKBRAG(t, "乙", "b.md")
	s1 := f.createSession(t, []int64{id1}, "会话一")
	s2 := f.createSession(t, []int64{id2}, "会话二")
	// 推开 updatedAt 保证排序可辨。
	later := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	f.chatStore.mu.Lock()
	f.chatStore.sessions[s2].UpdatedAt = &later
	f.chatStore.mu.Unlock()

	// 置顶旧会话 → 仍排最前。
	if _, err := f.chatStore.ToggleSessionPin(t.Context(), s1, later); err != nil {
		t.Fatalf("置顶失败: %v", err)
	}

	raw := f.getRaw(t, http.MethodGet, "/api/rag-chat/sessions")
	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Code != 200 {
		t.Fatalf("code = %d", resp.Code)
	}
	items := resp.Data
	if len(items) != 2 {
		t.Fatalf("列表长度 = %d", len(items))
	}
	if items[0]["title"] != "会话一" || items[1]["title"] != "会话二" {
		t.Fatalf("置顶优先排序不符: %v", items)
	}
	for _, item := range items {
		for _, key := range []string{"id", "title", "messageCount", "knowledgeBaseNames", "updatedAt", "isPinned"} {
			if _, ok := item[key]; !ok {
				t.Errorf("SessionListItemDTO 缺字段 %q: %v", key, item)
			}
		}
	}
	if items[0]["isPinned"] != true || items[1]["isPinned"] != false {
		t.Errorf("isPinned 不符: %v", items)
	}
	if names := items[0]["knowledgeBaseNames"].([]interface{}); len(names) != 1 || names[0] != "甲" {
		t.Errorf("knowledgeBaseNames = %v", names)
	}
	if mc := items[0]["messageCount"].(float64); mc != 0 {
		t.Errorf("messageCount = %v, want 0", mc)
	}
}

func TestRagChat会话详情契约与消息(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "面试笔记", "笔记.md")
	sessionID := f.createSession(t, []int64{id1}, "备战")
	f.cm.pushGenerate("改写")
	f.retriever.script["改写"] = ragDocKB(t, id1, "面试笔记", "笔记.md", 0, 1, "GMP 内容", 0.9)
	f.cm.pushStream("回答正文")

	f.postJSON(t, "/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream", `{"question":"第一问"}`)

	raw := f.getRaw(t, http.MethodGet, "/api/rag-chat/sessions/"+itoa(sessionID))
	var resp struct {
		Data struct {
			ID             int64            `json:"id"`
			Title          string           `json:"title"`
			KnowledgeBases []map[string]any `json:"knowledgeBases"`
			Messages       []map[string]any `json:"messages"`
			CreatedAt      string           `json:"createdAt"`
			UpdatedAt      *string          `json:"updatedAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	detail := resp.Data
	if detail.ID != sessionID || detail.Title != "备战" {
		t.Fatalf("详情头部不符: %+v", detail)
	}
	if len(detail.KnowledgeBases) != 1 {
		t.Fatalf("knowledgeBases = %v", detail.KnowledgeBases)
	}
	for _, key := range []string{"id", "name", "originalFilename", "vectorStatus", "questionGenStatus"} {
		if _, ok := detail.KnowledgeBases[0][key]; !ok {
			t.Errorf("详情知识库缺字段 %q", key)
		}
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("messages = %d, want 2（user + assistant）", len(detail.Messages))
	}
	if detail.Messages[0]["type"] != "user" || detail.Messages[0]["content"] != "第一问" {
		t.Errorf("user 消息不符: %v", detail.Messages[0])
	}
	if detail.Messages[1]["type"] != "assistant" || !strings.Contains(detail.Messages[1]["content"].(string), "回答正文") {
		t.Errorf("assistant 消息不符: %v", detail.Messages[1])
	}
	for _, m := range detail.Messages {
		for _, key := range []string{"id", "type", "content", "createdAt"} {
			if _, ok := m[key]; !ok {
				t.Errorf("MessageDTO 缺字段 %q: %v", key, m)
			}
		}
	}

	// 不存在 → 404 会话不存在。
	code, message, _, _ := f.doJSON(t, http.MethodGet, "/api/rag-chat/sessions/424242", "")
	if code != 404 || message != "会话不存在" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
}

func TestRagChat改标题置顶换绑删除(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	id2 := f.seedKBRAG(t, "乙", "b.md")
	sessionID := f.createSession(t, []int64{id1}, "原题")

	// 改标题：空白 400。
	code, message, _, _ := f.doJSON(t, http.MethodPut, "/api/rag-chat/sessions/"+itoa(sessionID)+"/title",
		`{"title":"  "}`)
	if code != 400 || message != "标题不能为空" {
		t.Fatalf("title 校验: code=%d, message=%q", code, message)
	}
	code, _, _, _ = f.doJSON(t, http.MethodPut, "/api/rag-chat/sessions/"+itoa(sessionID)+"/title",
		`{"title":"新标题"}`)
	if code != 200 {
		t.Fatalf("改标题 code = %d", code)
	}
	if got := f.chatStore.session(sessionID).Title; got != "新标题" {
		t.Errorf("标题未更新: %q", got)
	}

	// 置顶切换：false → true → false。
	code, _, _, _ = f.doJSON(t, http.MethodPut, "/api/rag-chat/sessions/"+itoa(sessionID)+"/pin", "")
	if code != 200 {
		t.Fatalf("置顶 code = %d", code)
	}
	if got := f.chatStore.session(sessionID).IsPinned; got == nil || !*got {
		t.Errorf("置顶后 isPinned 应为 true")
	}
	f.doJSON(t, http.MethodPut, "/api/rag-chat/sessions/"+itoa(sessionID)+"/pin", "")
	if got := f.chatStore.session(sessionID).IsPinned; got == nil || *got {
		t.Errorf("再切换后 isPinned 应为 false")
	}

	// 换绑知识库：缺失项静默丢弃（Java findAllById 语义）。
	code, _, _, _ = f.doJSON(t, http.MethodPut, "/api/rag-chat/sessions/"+itoa(sessionID)+"/knowledge-bases",
		`{"knowledgeBaseIds":[`+itoa(id2)+`,999]}`)
	if code != 200 {
		t.Fatalf("换绑 code = %d", code)
	}
	row, _ := f.chatStore.FindSessionRow(t.Context(), sessionID)
	if len(row.KBIDs) != 1 || row.KBIDs[0] != id2 {
		t.Errorf("换绑后 KBIDs = %v, want 仅含 %d", row.KBIDs, id2)
	}

	// 删除：消息与关联一并清理；重复删除 404。
	code, _, _, _ = f.doJSON(t, http.MethodDelete, "/api/rag-chat/sessions/"+itoa(sessionID), "")
	if code != 200 {
		t.Fatalf("删除 code = %d", code)
	}
	code, message, _, _ = f.doJSON(t, http.MethodDelete, "/api/rag-chat/sessions/"+itoa(sessionID), "")
	if code != 404 || message != "会话不存在" {
		t.Fatalf("重复删除: code=%d, message=%q", code, message)
	}
	msgs, _ := f.chatStore.FindMessagesBySessionID(t.Context(), sessionID)
	if len(msgs) != 0 {
		t.Errorf("删除后消息未清理: %d", len(msgs))
	}
}

func TestRagChat发消息流式换行转义与回写(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "面试笔记", "笔记.md")
	sessionID := f.createSession(t, []int64{id1}, "备战")
	f.retriever.script["改写"] = ragDocKB(t, id1, "面试笔记", "笔记.md", 0, 1, "c", 0.9)
	f.cm.pushGenerate("改写")
	long := strings.Repeat("内", 130) // 越过探测窗口实时透出
	f.cm.pushStream("第一行\n第二行", long)

	lines := f.streamChat(t, sessionID, "提问一")
	// 换行转义为字面 \n：wire 上为单 data 行，事件内不出现裸换行。
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "data:第一行\\n第二行") {
		t.Fatalf("换行转义不符: %q", lines)
	}

	// 用户消息 + AI 占位已落库，完成后回写全文。
	msgs, _ := f.chatStore.FindMessagesBySessionID(t.Context(), sessionID)
	if len(msgs) != 2 {
		t.Fatalf("落库消息数 = %d, want 2", len(msgs))
	}
	if msgs[0].Type != RagChatUser || msgs[0].Content != "提问一" {
		t.Errorf("用户消息不符: %+v", msgs[0])
	}
	assistant := f.chatStore.message(msgs[1].ID)
	if assistant.Type != RagChatAssistant {
		t.Fatalf("第二条应为 AI 消息")
	}
	if !completedOf(assistant) {
		t.Errorf("完成后消息应为 completed")
	}
	want := "第一行\n第二行" + long + ragSourceFooter(f.retriever.script["改写"])
	if assistant.Content != want {
		t.Errorf("回写全文不符:\n got=%q\nwant=%q", assistant.Content, want)
	}
}

func TestRagChat发消息流式问题校验与会话缺失(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")

	code, message, _, _ := f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream",
		`{"question":"  "}`)
	if code != 400 || message != "问题不能为空" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
	code, message, _, _ = f.doJSON(t, http.MethodPost, "/api/rag-chat/sessions/424242/messages/stream",
		`{"question":"问题"}`)
	if code != 404 || message != "会话不存在" {
		t.Fatalf("code = %d, message = %q", code, message)
	}
}

func TestRagChat发消息流式无命中落库固定拒答(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")
	f.retriever.falls = nil

	events := sseEvents(t, f.postJSON(t, "/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream",
		`{"question":"问题"}`))
	if len(events) != 1 || events[0] != noResultResponse {
		t.Fatalf("events = %q, want 固定拒答", events)
	}
	msgs, _ := f.chatStore.FindMessagesBySessionID(t.Context(), sessionID)
	if len(msgs) != 2 {
		t.Fatalf("落库消息数 = %d", len(msgs))
	}
	assistant := f.chatStore.message(msgs[1].ID)
	if assistant.Content != noResultResponse {
		t.Errorf("AI 消息 = %q, want 固定拒答", assistant.Content)
	}
}

func TestRagChat发消息流式取消保存已生成部分(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")
	f.retriever.script["改写"] = ragDocKB(t, id1, "甲", "a.md", 0, 1, "c", 0.9)
	f.cm.pushGenerate("改写")
	first := strings.Repeat("已", 130) // 越过探测窗口，实时透出
	f.cm.pushStream(first, "后续内容")
	// 发出第一块后阻塞，模拟客户端中断时上游仍在生成。
	f.cm.mu.Lock()
	f.cm.holdAfterCount = 1
	f.cm.hold = make(chan struct{})
	hold := f.cm.hold
	f.cm.mu.Unlock()

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream",
		strings.NewReader(`{"question":"问题"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := t.Context()
	req = req.WithContext(ctx)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() {
		t.Fatalf("应读到第一块")
	}
	if !strings.HasPrefix(scanner.Text(), "data:") {
		t.Fatalf("首行不是 data: %q", scanner.Text())
	}
	// 客户端中断。
	resp.Body.Close()

	// 轮询等待取消落库（Java doOnCancel）。
	var saved *RagChatMessageEntity
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := f.chatStore.FindMessagesBySessionID(t.Context(), sessionID)
		if len(msgs) == 2 {
			saved = f.chatStore.message(msgs[1].ID)
			if completedOf(saved) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(hold) // 释放上游，避免泄漏
	if saved == nil {
		t.Fatalf("取消后未落库")
	}
	if saved.Content != first {
		t.Errorf("取消应保存已生成部分:\n got=%q\nwant=%q", saved.Content, first)
	}
}

func TestRagChat发消息流式上游错误文案落库(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")
	f.retriever.script["改写"] = ragDocKB(t, id1, "甲", "a.md", 0, 1, "c", 0.9)
	f.cm.pushGenerate("改写")
	// 上游 Stream 直接失败：错误转为固定错误文案下发并回写 AI 消息
	// （Java onErrorResume → Flux.just(错误文案) → doOnNext/doOnComplete）。
	f.cm.alwaysStreamErr = errors.New("模型不可用")

	events := sseEvents(t, f.postJSON(t, "/api/rag-chat/sessions/"+itoa(sessionID)+"/messages/stream",
		`{"question":"问题"}`))
	if len(events) != 1 || events[0] != "【错误】知识库查询失败：AI服务暂时不可用，请稍后重试。" {
		t.Fatalf("events = %q, want 固定错误文案", events)
	}
	msgs, _ := f.chatStore.FindMessagesBySessionID(t.Context(), sessionID)
	assistant := f.chatStore.message(msgs[1].ID)
	if assistant.Content != "【错误】知识库查询失败：AI服务暂时不可用，请稍后重试。" {
		t.Errorf("错误文案应回写 AI 消息: %q", assistant.Content)
	}
	if !completedOf(assistant) {
		t.Errorf("错误后消息应置完成")
	}
}

func TestRagChat多轮历史最近10条完成消息(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")
	f.retriever.script["改写"] = ragDocKB(t, id1, "甲", "a.md", 0, 1, "c", 0.9)

	// 预置 12 轮历史（每轮 user+assistant 均完成），message_order 单调。
	now := time.Now()
	for i := 0; i < 12; i++ {
		completed := true
		f.chatStore.mu.Lock()
		f.chatStore.nextMessageID++
		uid := f.chatStore.nextMessageID
		f.chatStore.messages[uid] = &RagChatMessageEntity{ID: uid, SessionID: sessionID,
			Type: RagChatUser, Content: itoa(int64(i)) + "问", MessageOrder: i * 2, Completed: &completed, CreatedAt: now}
		f.chatStore.nextMessageID++
		aid := f.chatStore.nextMessageID
		f.chatStore.messages[aid] = &RagChatMessageEntity{ID: aid, SessionID: sessionID,
			Type: RagChatAssistant, Content: itoa(int64(i)) + "答", MessageOrder: i*2 + 1, Completed: &completed, CreatedAt: now}
		f.chatStore.mu.Unlock()
	}

	f.cm.pushGenerate("改写")
	f.cm.pushStream("回答")
	f.streamChat(t, sessionID, "第13问")

	input := f.cm.streamCall(0)
	// [system, 最近 10 条完成历史（正序）, user prompt]。
	if len(input) != 12 {
		t.Fatalf("消息数 = %d, want 12（system+10 历史+user）", len(input))
	}
	if input[0].Role != "system" {
		t.Errorf("首消息应为 system: %q", input[0].Role)
	}
	// 12 轮历史 → DESC 取 11 条（含当前轮 user），剔除当前轮后剩 10 条：
	// 第 7~11 轮的 4 条…精确说：完成消息共 24+1(当前) = 25 条？否——预置 24 条 + 当前 user 1 条。
	// DESC 前 11 = [当前user, 11答, 11问, 10答, 10问, 9答, 9问, 8答, 8问, 7答, 7问]，
	// 剔除首条后正序 = [7问, 7答, …, 11答]。
	if input[1].Role != "user" || input[1].Content != "7问" {
		t.Errorf("历史首条 = %q/%q, want user/7问", input[1].Role, input[1].Content)
	}
	if input[11].Role != "user" {
		t.Errorf("末消息应为当前问题 user: %q", input[11].Role)
	}
	if input[10].Role != "assistant" || input[10].Content != "11答" {
		t.Errorf("历史末条 = %q/%q, want assistant/11答", input[10].Role, input[10].Content)
	}
}

func TestRagChat多轮历史首轮无历史(t *testing.T) {
	f := newRagFixture(t)
	id1 := f.seedKBRAG(t, "甲", "a.md")
	sessionID := f.createSession(t, []int64{id1}, "会话")
	f.retriever.script["改写"] = ragDocKB(t, id1, "甲", "a.md", 0, 1, "c", 0.9)
	f.cm.pushGenerate("改写")
	f.cm.pushStream("回答")

	f.streamChat(t, sessionID, "第一问")
	input := f.cm.streamCall(0)
	if len(input) != 2 || input[0].Role != "system" || input[1].Role != "user" {
		t.Fatalf("首轮消息 = %d 条, want 2（system+user）", len(input))
	}
}
