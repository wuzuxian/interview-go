// RAG 查询/会话链路的测试替身与夹具（先例 upload_service_test.go 的 kbFixture、
// voiceinterview llm_stream_test.go 的 scriptedStreamModel）：
// fakeRetriever 检索脚本、fakeChatModel 脚本化 Generate/Stream（schema.Pipe 造流）、
// fakeChatStore RAG 会话内存镜像；HTTP 契约测试用 httptest.NewServer 真连接读流。
package knowledgebase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/vector"
)

// ===== fakeRetriever =====

// retrievalCall 一次检索调用参数快照。
type retrievalCall struct {
	query    string
	kbIDs    []int64
	topK     int
	minScore float64
}

type fakeRetriever struct {
	mu     sync.Mutex
	script map[string][]vector.KBDoc // 按查询文本命中的结果脚本
	falls  []vector.KBDoc            // 未命中脚本时的兜底结果
	calls  []retrievalCall
	err    error
}

func (r *fakeRetriever) SimilaritySearch(ctx context.Context, query string, kbIDs []int64, topK int, minScore float64) ([]vector.KBDoc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, retrievalCall{query: query, kbIDs: append([]int64(nil), kbIDs...), topK: topK, minScore: minScore})
	if r.err != nil {
		return nil, r.err
	}
	if docs, ok := r.script[query]; ok {
		return docs, nil
	}
	return r.falls, nil
}

func (r *fakeRetriever) callList() []retrievalCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]retrievalCall(nil), r.calls...)
}

func (r *fakeRetriever) addDoc(id, kbID, name, filename string, index, total int, content string, score float64) {
	if r.script == nil {
		r.script = map[string][]vector.KBDoc{}
	}
	if _, ok := r.script["命中问题"]; !ok {
		r.script["命中问题"] = nil
	}
	r.script["命中问题"] = append(r.script["命中问题"], vector.KBDoc{
		ID: id, Content: content,
		Metadata: map[string]any{
			"kb_id": kbID, "source_name": name, "source_filename": filename,
			"chunk_index": index, "total_chunks": total,
		},
		Score: &score,
	})
}

// ===== fakeChatModel =====

type genOutcome struct {
	content string
	err     error
}

type fakeChatModel struct {
	mu              sync.Mutex
	generateQueue   []genOutcome
	streamScripts   [][]string
	streamErrs      []error
	alwaysStreamErr error
	// holdAfterCount / hold：发送 holdAfterCount 个切片后阻塞在 hold 上，
	// 直到测试 close(hold)（取消语义测试用）。
	holdAfterCount int
	hold           chan struct{}

	generateInputs [][]*schema.Message
	streamInputs   [][]*schema.Message
	// hookAfterGenerate 首次 Generate 返回后触发（模拟 LLM 调用窗口内的实体变更）。
	hookAfterGenerate func()
}

func (m *fakeChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.generateInputs = append(m.generateInputs, input)
	callIndex := len(m.generateInputs)
	var out genOutcome
	if len(m.generateQueue) > 0 {
		out = m.generateQueue[0]
		m.generateQueue = m.generateQueue[1:]
	} else {
		m.mu.Unlock()
		return nil, errors.New("fakeChatModel: Generate 队列为空")
	}
	hook := m.hookAfterGenerate
	m.mu.Unlock()
	// 钩子只在首次 Generate 返回后触发（模拟 LLM 调用窗口内的实体变更）。
	if hook != nil && callIndex == 1 {
		hook()
	}
	if out.err != nil {
		return nil, out.err
	}
	return &schema.Message{Content: out.content}, nil
}

func (m *fakeChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	m.streamInputs = append(m.streamInputs, input)
	if m.alwaysStreamErr != nil {
		err := m.alwaysStreamErr
		m.mu.Unlock()
		return nil, err
	}
	var chunks []string
	var scriptedErr error
	if len(m.streamScripts) > 0 {
		chunks = m.streamScripts[0]
		m.streamScripts = m.streamScripts[1:]
	}
	if len(m.streamErrs) > 0 {
		scriptedErr = m.streamErrs[0]
		m.streamErrs = m.streamErrs[1:]
	}
	m.mu.Unlock()

	sr, sw := schema.Pipe[*schema.Message](0)
	holdAfterCount, hold := m.holdAfterCount, m.hold
	go func() {
		defer sw.Close()
		for i, chunk := range chunks {
			if hold != nil && holdAfterCount > 0 && i == holdAfterCount {
				select {
				case <-hold:
				case <-ctx.Done():
					sw.Send(nil, ctx.Err())
					return
				}
			}
			select {
			case <-ctx.Done():
				sw.Send(nil, ctx.Err())
				return
			default:
			}
			sw.Send(&schema.Message{Content: chunk}, nil)
		}
		if scriptedErr != nil {
			sw.Send(nil, scriptedErr)
		}
	}()
	return sr, nil
}

func (m *fakeChatModel) pushGenerate(contents ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range contents {
		m.generateQueue = append(m.generateQueue, genOutcome{content: c})
	}
}

func (m *fakeChatModel) pushStream(chunks ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamScripts = append(m.streamScripts, chunks)
}

// pushGenerateErr 脚本化一次 Generate 失败（改写降级链路）。
func (m *fakeChatModel) pushGenerateErr() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generateQueue = append(m.generateQueue, genOutcome{err: errors.New("改写调用失败")})
}

// pushStreamWithError 脚本化「先发 chunk 再出错」的流。
func (m *fakeChatModel) pushStreamWithError(chunk string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamScripts = append(m.streamScripts, []string{chunk})
	m.streamErrs = append(m.streamErrs, err)
}

func (m *fakeChatModel) generateCall(i int) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generateInputs[i]
}

func (m *fakeChatModel) streamCall(i int) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamInputs[i]
}

// ===== 夹具种子与文档脚本 =====

// seedKBRAG 落一个向量化完成的知识库，返回 ID。
func (f *ragFixture) seedKBRAG(t *testing.T, name, filename string) int64 {
	t.Helper()
	kb := sampleKB(0)
	kb.Name = name
	kb.OriginalFilename = filename
	kb.FileHash = pad64("hash-" + name)
	kb.VectorStatus = VectorCompleted
	id := f.store.addKB(kb)
	f.chatStore.seedKBName(id, name)
	return id
}

// ragDocKB 单片段检索脚本（kb_id 按生产 metadata 协议为字符串）。
func ragDocKB(t *testing.T, kbID int64, name, filename string, index, total int, content string, score float64) []vector.KBDoc {
	t.Helper()
	s := &score
	return []vector.KBDoc{{
		ID:      "doc-" + strconv.FormatInt(kbID, 10) + "-" + strconv.Itoa(index),
		Content: content,
		Metadata: map[string]any{
			"kb_id": strconv.FormatInt(kbID, 10), "source_name": name,
			"source_filename": filename, "chunk_index": index, "total_chunks": total,
		},
		Score: s,
	}}
}

// head 前 n 个 rune。
func head(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ===== fakeChatStore（ChatStore 内存镜像）=====

type fakeChatStore struct {
	mu            sync.Mutex
	sessions      map[int64]*RagChatSessionEntity
	sessionOrder  []int64
	joins         map[int64][]int64
	kbNames       map[int64]string
	kbExists      map[int64]bool
	messages      map[int64]*RagChatMessageEntity
	nextSessionID int64
	nextMessageID int64
}

func newFakeChatStore() *fakeChatStore {
	return &fakeChatStore{
		sessions:      map[int64]*RagChatSessionEntity{},
		joins:         map[int64][]int64{},
		kbNames:       map[int64]string{},
		kbExists:      map[int64]bool{},
		messages:      map[int64]*RagChatMessageEntity{},
		nextSessionID: 500,
		nextMessageID: 5000,
	}
}

func (s *fakeChatStore) seedKBName(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kbNames[id] = name
	s.kbExists[id] = true
}

func (s *fakeChatStore) InsertSession(ctx context.Context, session *RagChatSessionEntity, kbIDs []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSessionID++
	session.ID = s.nextSessionID
	copied := *session
	s.sessions[session.ID] = &copied
	s.sessionOrder = append(s.sessionOrder, session.ID)
	s.joins[session.ID] = append([]int64(nil), kbIDs...)
	return nil
}

func (s *fakeChatStore) FindSessionRow(ctx context.Context, id int64) (*RagChatSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return nil, ErrRagSessionNotFound
	}
	copied := *session
	kbIDs := append([]int64(nil), s.joins[id]...)
	sort.Slice(kbIDs, func(i, j int) bool { return kbIDs[i] < kbIDs[j] })
	return &RagChatSessionRow{Session: copied, KBIDs: kbIDs}, nil
}

func (s *fakeChatStore) FindSessionRowsOrderByPinnedUpdatedAtDesc(ctx context.Context) ([]RagChatSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]RagChatSessionRow, 0, len(s.sessionOrder))
	for _, id := range s.sessionOrder {
		copied := *s.sessions[id]
		kbIDs := append([]int64(nil), s.joins[id]...)
		sort.Slice(kbIDs, func(i, j int) bool { return kbIDs[i] < kbIDs[j] })
		rows = append(rows, RagChatSessionRow{Session: copied, KBIDs: kbIDs})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := rows[i].Session.IsPinned != nil && *rows[i].Session.IsPinned,
			rows[j].Session.IsPinned != nil && *rows[j].Session.IsPinned
		if pi != pj {
			return pi
		}
		// Java ORDER BY updated_at DESC（Postgres DESC NULL 靠前）。
		ui, uj := rows[i].Session.UpdatedAt, rows[j].Session.UpdatedAt
		switch {
		case ui == nil && uj == nil:
			return false
		case ui == nil:
			return true
		case uj == nil:
			return false
		}
		return ui.After(*uj)
	})
	return rows, nil
}

func (s *fakeChatStore) FindKBNamesBySessionIDs(ctx context.Context, sessionIDs []int64) (map[int64][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64][]string{}
	for _, sid := range sessionIDs {
		for _, kbID := range s.joins[sid] {
			out[sid] = append(out[sid], s.kbNames[kbID])
		}
	}
	return out, nil
}

func (s *fakeChatStore) UpdateSessionTitle(ctx context.Context, id int64, title string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return ErrRagSessionNotFound
	}
	session.Title = title
	session.UpdatedAt = &now
	return nil
}

func (s *fakeChatStore) ToggleSessionPin(ctx context.Context, id int64, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return false, ErrRagSessionNotFound
	}
	current := session.IsPinned != nil && *session.IsPinned
	next := !current
	session.IsPinned = &next
	session.UpdatedAt = &now
	return next, nil
}

func (s *fakeChatStore) UpdateSessionKnowledgeBases(ctx context.Context, id int64, kbIDs []int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrRagSessionNotFound
	}
	// Java findAllById 语义：缺失项静默丢弃。
	existing := make([]int64, 0, len(kbIDs))
	for _, kbID := range kbIDs {
		if s.kbExists[kbID] {
			existing = append(existing, kbID)
		}
	}
	s.joins[id] = existing
	s.sessions[id].UpdatedAt = &now
	return nil
}

func (s *fakeChatStore) DeleteSession(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrRagSessionNotFound
	}
	delete(s.sessions, id)
	delete(s.joins, id)
	for mid, m := range s.messages {
		if m.SessionID == id {
			delete(s.messages, mid)
		}
	}
	return nil
}

func (s *fakeChatStore) InsertStreamMessages(ctx context.Context, sessionID int64, question string, startOrder int, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return 0, ErrRagSessionNotFound
	}
	s.nextMessageID++
	completed := true
	s.messages[s.nextMessageID] = &RagChatMessageEntity{
		ID: s.nextMessageID, SessionID: sessionID, Type: RagChatUser, Content: question,
		MessageOrder: startOrder, Completed: &completed, CreatedAt: now, UpdatedAt: &now,
	}
	s.nextMessageID++
	notCompleted := false
	assistantID := s.nextMessageID
	s.messages[assistantID] = &RagChatMessageEntity{
		ID: assistantID, SessionID: sessionID, Type: RagChatAssistant, Content: "",
		MessageOrder: startOrder + 1, Completed: &notCompleted, CreatedAt: now, UpdatedAt: &now,
	}
	next := startOrder + 2
	s.sessions[sessionID].MessageCount = &next
	s.sessions[sessionID].UpdatedAt = &now
	return assistantID, nil
}

func (s *fakeChatStore) FindMessagesBySessionID(ctx context.Context, sessionID int64) ([]RagChatMessageEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RagChatMessageEntity
	for _, id := range s.sortedMessageIDs() {
		if m := s.messages[id]; m.SessionID == sessionID {
			out = append(out, *m)
		}
	}
	if out == nil {
		out = []RagChatMessageEntity{}
	}
	return out, nil
}

func (s *fakeChatStore) FindRecentCompletedMessages(ctx context.Context, sessionID int64, limit int) ([]RagChatMessageEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RagChatMessageEntity
	ids := s.sortedMessageIDs()
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- {
		m := s.messages[ids[i]]
		if m.SessionID == sessionID && m.Completed != nil && *m.Completed {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *fakeChatStore) CompleteMessage(ctx context.Context, messageID int64, content string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[messageID]
	if !ok {
		return ErrRagMessageNotFound
	}
	m.Content = content
	completed := true
	m.Completed = &completed
	m.UpdatedAt = &now
	return nil
}

// sortedMessageIDs 按主键排序的快照（message_order 单调递增的同序镜像）。
func (s *fakeChatStore) sortedMessageIDs() []int64 {
	ids := make([]int64, 0, len(s.messages))
	for id := range s.messages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// message 断言取用。
func (s *fakeChatStore) message(id int64) *RagChatMessageEntity {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return nil
	}
	copied := *m
	return &copied
}

// session 快照取用。
func (s *fakeChatStore) session(id int64) *RagChatSessionEntity {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[id]
	if !ok {
		return nil
	}
	copied := *row
	return &copied
}

// ===== 夹具 =====

type ragFixture struct {
	rec       *actionRecorder
	store     *fakeStore
	chatStore *fakeChatStore
	retriever *fakeRetriever
	cm        *fakeChatModel
	svc       *QueryService
	chatSvc   *RagChatService
	cfg       RagQueryConfig
	engine    *gin.Engine
	srv       *httptest.Server
}

func newRagFixture(t interface {
	Helper()
	Cleanup(func())
}) *ragFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := &actionRecorder{}
	store := newFakeStore(rec)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewQueryService(func(ctx context.Context) (ChatModel, error) { return cm, nil },
		retriever, store, NewListService(store, newFakeStorage(rec), logger), cfg, logger)
	chatStore := newFakeChatStore()
	chatSvc := NewRagChatService(store, chatStore, svc, cfg, logger)

	f := &ragFixture{
		rec: rec, store: store, chatStore: chatStore, retriever: retriever, cm: cm,
		svc: svc, chatSvc: chatSvc, cfg: cfg,
	}
	f.engine = gin.New()
	api := f.engine.Group("/api")
	NewController(
		NewUploadService(store, newFakeStorage(rec), NewVectorizeProducer(newFakeStream(rec), store, logger),
			50*1024*1024, 4, logger),
		NewListService(store, newFakeStorage(rec), logger),
		NewDeleteService(store, newFakeStorage(rec), logger),
		svc,
		ratelimit.New(errScripter{}, logger),
	).RegisterRoutes(api)
	NewRagChatController(chatSvc, logger).RegisterRoutes(api)
	f.srv = httptest.NewServer(f.engine)
	t.Cleanup(f.srv.Close)
	return f
}
