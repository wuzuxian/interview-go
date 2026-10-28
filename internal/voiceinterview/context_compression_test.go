// 上下文压缩单测（票 07；Java context/VoiceContextCompressor + context/VoiceHistoryLoader
// + VoiceInterviewService.saveSummaryRow 语义逐字）：压缩器三模式装配、字符预算、
// 摘要截断、模板渲染；加载器有界装配、旧摘要行边界迁移、锁内写回、降级路径。
// 内存读写作替身（先例 evaluation_fakes_test.go），只测外部行为。
package voiceinterview

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ===== 替身 =====

// stubPlainCompleter 摘要模型替身：自身同时充当 resolver 与 completer
// （VoicePlainModelResolver + ChatCompleter），记录收到的 prompt。
type stubPlainCompleter struct {
	mu          sync.Mutex
	content     string
	callErr     error
	resolverErr error
	prompts     []string
}

func (s *stubPlainCompleter) VoicePlainCompleterFor(_ context.Context, _ string) (PlainChatCompleter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolverErr != nil {
		return nil, s.resolverErr
	}
	return s, nil
}

func (s *stubPlainCompleter) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, input[0].Content)
	if s.callErr != nil {
		return nil, s.callErr
	}
	return &schema.Message{Content: s.content}, nil
}

func (s *stubPlainCompleter) promptList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// savedSummary 锁内写回的落库记录（断言摘要正文与边界）。
type savedSummary struct {
	summary *string
	covered int
}

// fakeCompStore 票 07 压缩链路存储替身：内嵌 fakeStore 复用会话/消息内存表，
// 覆写窗口读取以注入故障，另记录摘要行写回。镜像生产 *Repo 的语义（含 SUMMARY
// 行过滤与锁内步骤顺序）。
type fakeCompStore struct {
	*fakeStore

	pendingErr     error
	recentErr      error
	summarySaveErr error

	mu             sync.Mutex
	savedSummaries map[int64]savedSummary
}

func newFakeCompStore() *fakeCompStore {
	return &fakeCompStore{
		fakeStore:      newFakeStore(&svcRecorder{}),
		savedSummaries: map[int64]savedSummary{},
	}
}

// addSessionAt 以指定会话 ID 预置进行中会话（写回路径校验会话存在性）。
func (s *fakeCompStore) addSessionAt(id int64) {
	e := inProgressSession(time.Hour)
	e.ID = id
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = e
}

func (s *fakeCompStore) FindRecentDialogueMessages(ctx context.Context, sessionID int64, limit int) ([]MessageEntity, error) {
	s.mu.Lock()
	if s.recentErr != nil {
		err := s.recentErr
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	return s.fakeStore.FindRecentDialogueMessages(ctx, sessionID, limit)
}

// FindSummaryRow 会话第一条（按序号升序）SUMMARY 行；无行返回 (nil, nil)。
func (s *fakeCompStore) FindSummaryRow(_ context.Context, sessionID int64) (*MessageEntity, error) {
	rows := s.fakeStore.dialogueRows(sessionID)
	for i := range rows {
		if rows[i].MessageType == MessageSummary {
			m := rows[i]
			return &m, nil
		}
	}
	return nil, nil
}

// FindPendingSummaryBatch 序号 ∈ (after, before) 的非 SUMMARY 批（升序，至多 limit 条）。
func (s *fakeCompStore) FindPendingSummaryBatch(_ context.Context, sessionID int64, after, before, limit int) ([]MessageEntity, error) {
	s.mu.Lock()
	if s.pendingErr != nil {
		err := s.pendingErr
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	rows := s.fakeStore.dialogueRows(sessionID)
	var out []MessageEntity
	for _, m := range rows {
		if m.MessageType == MessageSummary || m.SequenceNum == nil {
			continue
		}
		if *m.SequenceNum > after && *m.SequenceNum < before {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FindDialogueMessageAt 升序第 offset 条（0 起）非 SUMMARY 消息；无行返回 (nil, nil)。
func (s *fakeCompStore) FindDialogueMessageAt(_ context.Context, sessionID int64, offset int) (*MessageEntity, error) {
	rows := s.fakeStore.dialogueRows(sessionID)
	asc := make([]MessageEntity, 0, len(rows))
	for _, m := range rows {
		if m.MessageType != MessageSummary {
			asc = append(asc, m)
		}
	}
	for i := 0; i < len(asc); i++ {
		for j := i + 1; j < len(asc); j++ {
			si, sj := asc[i].SequenceNum, asc[j].SequenceNum
			if sj != nil && (si == nil || *sj < *si) {
				asc[i], asc[j] = asc[j], asc[i]
			}
		}
	}
	if offset >= len(asc) {
		return nil, nil
	}
	m := asc[offset]
	return &m, nil
}

// SaveSummaryRow 锁内写回（镜像 Java saveSummaryRow L321-344 步骤顺序：锁会话行 →
// 读既有 SUMMARY 行 → 边界单调校验 → 写行），rec 记录步骤供顺序断言。
func (s *fakeCompStore) SaveSummaryRow(_ context.Context, sessionID int64, summary *string, coveredSequenceNum int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("lock-session")
	if _, ok := s.sessions[sessionID]; !ok {
		return ErrSessionNotFound
	}
	s.rec.add("find-summary-row")
	var row *MessageEntity
	rows := s.dialogues[sessionID]
	for i := range rows {
		if rows[i].MessageType == MessageSummary {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		seq := -1
		row = &MessageEntity{SessionID: sessionID, MessageType: MessageSummary, SequenceNum: &seq}
	}
	if row.SummaryCoveredSequenceNum != nil && coveredSequenceNum < *row.SummaryCoveredSequenceNum {
		s.rec.add("reject-summary-regression")
		return nil
	}
	seq := -1
	if row.SequenceNum != nil && *row.SequenceNum < seq {
		seq = *row.SequenceNum
	}
	row.SequenceNum = &seq
	row.AiGeneratedText = summary
	row.SummaryCoveredSequenceNum = &coveredSequenceNum
	if row.ID == 0 {
		s.msgNextID++
		row.ID = s.msgNextID
		s.dialogues[sessionID] = append(s.dialogues[sessionID], *row)
	}
	s.rec.add("write-summary-row")
	s.savedSummaries[sessionID] = savedSummary{summary: summary, covered: coveredSequenceNum}
	if s.summarySaveErr != nil {
		return s.summarySaveErr
	}
	return nil
}

// ===== 夹具 =====

// compressionFixture 加载器夹具：fakeCompStore + stub 摘要模型 + 真压缩器/加载器。
// 默认小参数：window=2、batch=2，便于用少量轮次越过触发线。
type compressionFixture struct {
	store   *fakeCompStore
	summary *stubPlainCompleter
	loader  *HistoryLoader
}

func newCompressionFixture(mutate func(cfg *ContextCompressionConfig)) *compressionFixture {
	cfg := ContextCompressionConfig{
		Enabled:          true,
		Mode:             ModeSummary,
		WindowSize:       2,
		SummaryBatchSize: 2,
		MaxHistoryChars:  12000,
		MaxSummaryChars:  4000,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	summary := &stubPlainCompleter{content: "新摘要"}
	store := newFakeCompStore()
	comp := NewCompressor(cfg, summary, discardLogger())
	loader := NewHistoryLoader(store, comp, cfg, discardLogger())
	return &compressionFixture{store: store, summary: summary, loader: loader}
}

// addDialogues 预置 n 轮问答对话（seq 1..n，问N内容/答N）。
func (f *compressionFixture) addDialogues(sessionID int64, n int) {
	msgs := make([]MessageEntity, 0, n)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, qaTurn(sessionID, i, fmt.Sprintf("问%d内容", i), fmt.Sprintf("答%d", i)))
	}
	f.store.setDialogues(sessionID, msgs)
}

// prependSummaryRow 在既有对话行前插入一行 SUMMARY 摘要行。
func (f *compressionFixture) prependSummaryRow(sessionID int64, row MessageEntity) {
	f.store.setDialogues(sessionID, append([]MessageEntity{row}, f.store.dialogueRows(sessionID)...))
}

// qaTurn 构造一轮「面试官问 + 候选人答」对话行。
func qaTurn(sessionID int64, seq int, q, a string) MessageEntity {
	return voiceMsg(sessionID, seq, stringPtr(q), stringPtr(a))
}

// summaryRow 构造一行 SUMMARY 摘要行（summary 空串表示正文为空）。
func summaryRow(sessionID int64, seq int, summary string, covered *int) MessageEntity {
	m := MessageEntity{
		SessionID:                 sessionID,
		MessageType:               MessageSummary,
		SequenceNum:               &seq,
		SummaryCoveredSequenceNum: covered,
	}
	if summary != "" {
		m.AiGeneratedText = stringPtr(summary)
	}
	return m
}

// dialogueOnlyRows 内存消息表的 SUMMARY 行过滤（票 07 起内存表含摘要行，
// fake 各读取方法与生产 repo 的 message_type <> SUMMARY 语义对齐）。
func dialogueOnlyRows(rows []MessageEntity) []MessageEntity {
	out := make([]MessageEntity, 0, len(rows))
	for _, m := range rows {
		if m.MessageType != MessageSummary {
			out = append(out, m)
		}
	}
	return out
}

// ===== 断言小工具 =====

func seqList(msgs []MessageEntity) []int {
	out := make([]int, 0, len(msgs))
	for _, m := range msgs {
		if m.SequenceNum != nil {
			out = append(out, *m.SequenceNum)
		}
	}
	return out
}

func assertSeqList(t *testing.T, name string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 序号不符:\n got  %v\n want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s 第 %d 个序号不符:\n got  %v\n want %v", name, i, got, want)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// turns5 构造 5 轮问答（seq 1..5，问N/答N）。
func turns5() []MessageEntity {
	turns := make([]MessageEntity, 0, 5)
	for i := 1; i <= 5; i++ {
		turns = append(turns, qaTurn(0, i, fmt.Sprintf("问%d", i), fmt.Sprintf("答%d", i)))
	}
	return turns
}

// ===== 压缩器：三模式装配 =====

func TestCompress三模式装配(t *testing.T) {
	ctx := context.Background()

	t.Run("NONE模式不裁窗口不摘要", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeNone,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			&stubPlainCompleter{}, discardLogger())
		got := c.Compress(ctx, turns5(), nil, 0, "p1")
		if got.Summary != nil {
			t.Errorf("NONE 不应有摘要: %v", *got.Summary)
		}
		assertSeqList(t, "NONE 最近段", seqList(got.Recent), []int{1, 2, 3, 4, 5})
		if got.Changed || got.CoveredTurns != 0 {
			t.Errorf("NONE 不应推进: changed=%v covered=%d", got.Changed, got.CoveredTurns)
		}
	})

	t.Run("WINDOW模式仅保留最近窗口", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeWindow,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			&stubPlainCompleter{}, discardLogger())
		got := c.Compress(ctx, turns5(), nil, 0, "p1")
		if got.Summary != nil {
			t.Errorf("WINDOW 不应有摘要: %v", *got.Summary)
		}
		assertSeqList(t, "WINDOW 最近段", seqList(got.Recent), []int{4, 5})
	})

	t.Run("SUMMARY轮数未超窗口时带出旧摘要不触发调用", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "新摘要"}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		cached := "旧摘要"
		got := c.Compress(ctx, turns5()[:2], &cached, 0, "p1")
		if got.Summary == nil || *got.Summary != "旧摘要" {
			t.Errorf("应原样带出旧摘要: %v", got.Summary)
		}
		assertSeqList(t, "未超窗口最近段", seqList(got.Recent), []int{1, 2})
		if got.Changed || got.CoveredTurns != 0 {
			t.Errorf("不应推进: changed=%v covered=%d", got.Changed, got.CoveredTurns)
		}
		if len(summary.promptList()) != 0 {
			t.Errorf("窗口内不应调用摘要模型: %v", summary.promptList())
		}
	})

	t.Run("SUMMARY未覆盖批满触发增量摘要并推进覆盖", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "合并后的摘要"}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		got := c.Compress(ctx, turns5(), nil, 0, "p1")
		if got.Summary == nil || *got.Summary != "合并后的摘要" {
			t.Errorf("应写入新摘要: %v", got.Summary)
		}
		assertSeqList(t, "摘要后最近段", seqList(got.Recent), []int{4, 5})
		if !got.Changed || got.CoveredTurns != 3 {
			t.Errorf("覆盖推进不符: changed=%v covered=%d", got.Changed, got.CoveredTurns)
		}
		if prompts := summary.promptList(); len(prompts) != 1 {
			t.Fatalf("摘要模型应恰好调用 1 次: %d", len(prompts))
		}
	})

	t.Run("SUMMARY已有旧摘要时增量合并", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "旧+新增的摘要"}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		cached := "旧摘要"
		got := c.Compress(ctx, turns5(), &cached, 0, "p1")
		if got.Summary == nil || *got.Summary != "旧+新增的摘要" {
			t.Errorf("应合并出新摘要: %v", got.Summary)
		}
		prompt := summary.promptList()[0]
		if !containsAll(prompt, "旧摘要", "问1", "答1", "问2", "答2", "问3", "答3") {
			t.Errorf("摘要提示词应含前情摘要与第 1-3 轮: %s", prompt)
		}
		if containsAll(prompt, "问4", "问5") {
			t.Errorf("摘要提示词不应含最近窗口轮次: %s", prompt)
		}
	})

	t.Run("SUMMARY未覆盖不足一批时不摘要且保留未覆盖早期轮次", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "新摘要"}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		// 3 轮、窗口 2 → 早期 1 轮不足一批：没有旧摘要时不能丢，全量保留。
		got := c.Compress(ctx, turns5()[:3], nil, 0, "p1")
		if len(summary.promptList()) != 0 {
			t.Errorf("不足一批不应调用摘要模型")
		}
		if got.Summary != nil || got.Changed {
			t.Errorf("不应产生摘要: %v changed=%v", got.Summary, got.Changed)
		}
		assertSeqList(t, "未覆盖早期保留", seqList(got.Recent), []int{1, 2, 3})
	})

	t.Run("SUMMARY部分覆盖时仅对未覆盖余量判定批次", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "新摘要"}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		cached := "旧摘要"
		// 早期 3 轮中已覆盖 2 轮，余 1 轮不足一批：已覆盖部分不再下发，余量保留。
		got := c.Compress(ctx, turns5(), &cached, 2, "p1")
		if len(summary.promptList()) != 0 {
			t.Errorf("余量不足一批不应调用摘要模型")
		}
		if got.Summary == nil || *got.Summary != "旧摘要" {
			t.Errorf("应保留旧摘要: %v", got.Summary)
		}
		assertSeqList(t, "已覆盖早期剔除", seqList(got.Recent), []int{3, 4, 5})
		if got.Changed {
			t.Errorf("未触发摘要不应标记 changed")
		}
	})

	t.Run("SUMMARY摘要失败降级窗口且不推进", func(t *testing.T) {
		summary := &stubPlainCompleter{content: "", callErr: errString("模型故障")}
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 2, SummaryBatchSize: 2, MaxHistoryChars: 12000, MaxSummaryChars: 4000},
			summary, discardLogger())
		got := c.Compress(ctx, turns5(), nil, 0, "p1")
		if got.Summary != nil {
			t.Errorf("无旧摘要时降级不应有摘要: %v", got.Summary)
		}
		assertSeqList(t, "降级最近段", seqList(got.Recent), []int{4, 5})
		if got.Changed || got.CoveredTurns != 0 {
			t.Errorf("失败不应推进: changed=%v covered=%d", got.Changed, got.CoveredTurns)
		}

		t.Run("失败时保留旧摘要", func(t *testing.T) {
			cached := "旧摘要"
			got := c.Compress(ctx, turns5(), &cached, 0, "p1")
			if got.Summary == nil || *got.Summary != "旧摘要" {
				t.Errorf("失败应保留旧摘要: %v", got.Summary)
			}
			assertSeqList(t, "失败降级最近段", seqList(got.Recent), []int{4, 5})
		})
	})
}

// ===== 压缩器：字符硬预算 =====

func TestCompress字符硬预算(t *testing.T) {
	ctx := context.Background()
	longTurns := func(n int) []MessageEntity {
		turns := make([]MessageEntity, 0, n)
		for i := 1; i <= n; i++ {
			// 每轮格式化成本 = 4(面试官：) + 10 + 4(候选人：) + 2 = 20 字符。
			turns = append(turns, qaTurn(0, i, "0123456789", "ab"))
		}
		return turns
	}

	t.Run("预算内全部保留", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeWindow,
			WindowSize: 10, MaxHistoryChars: 60}, &stubPlainCompleter{}, discardLogger())
		got := c.Compress(ctx, longTurns(3), nil, 0, "")
		assertSeqList(t, "预算内", seqList(got.Recent), []int{1, 2, 3})
	})

	t.Run("超预算从最早近期消息开始丢弃", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeWindow,
			WindowSize: 10, MaxHistoryChars: 50}, &stubPlainCompleter{}, discardLogger())
		got := c.Compress(ctx, longTurns(3), nil, 0, "")
		assertSeqList(t, "超预算保留最近", seqList(got.Recent), []int{2, 3})
	})

	t.Run("连最近一条都放不下时尾部截断且优先候选人", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeWindow,
			WindowSize: 10, MaxHistoryChars: 12}, &stubPlainCompleter{}, discardLogger())
		turn := qaTurn(0, 1, "AAAAAAAAAA", "bbbbbbbbbb")
		got := c.Compress(ctx, []MessageEntity{turn}, nil, 0, "")
		if len(got.Recent) != 1 {
			t.Fatalf("应保留最近一条: %d", len(got.Recent))
		}
		m := got.Recent[0]
		// 预算 12 全给最近一条：候选人 14 字符（含前缀）截到尾部 12 → 正文留 8；
		// 面试官无余量清空。
		if m.UserRecognizedText == nil || *m.UserRecognizedText != "bbbbbbbb" {
			t.Errorf("候选人应按尾部保留 8 字: %v", m.UserRecognizedText)
		}
		if m.AiGeneratedText == nil || *m.AiGeneratedText != "" {
			t.Errorf("面试官应无余量清空: %v", m.AiGeneratedText)
		}
		if m.SequenceNum == nil || *m.SequenceNum != 1 {
			t.Errorf("截断副本应保留序号: %v", m.SequenceNum)
		}
	})

	t.Run("SUMMARY预算计入摘要字符", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
			WindowSize: 10, SummaryBatchSize: 10, MaxHistoryChars: 40, MaxSummaryChars: 4000},
			&stubPlainCompleter{}, discardLogger())
		cached := "0123456789" // 摘要 10 字符 → 剩余预算 30 → 只装得下最近 1 轮
		got := c.Compress(ctx, longTurns(2), &cached, 0, "")
		assertSeqList(t, "计入摘要后", seqList(got.Recent), []int{2})
	})

	t.Run("预算为非正数时不裁剪", func(t *testing.T) {
		c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeWindow,
			WindowSize: 10, MaxHistoryChars: 0}, &stubPlainCompleter{}, discardLogger())
		got := c.Compress(ctx, longTurns(3), nil, 0, "")
		assertSeqList(t, "无预算", seqList(got.Recent), []int{1, 2, 3})
	})
}

func TestCompress摘要超限尾部截断(t *testing.T) {
	c := NewCompressor(ContextCompressionConfig{Enabled: true, Mode: ModeSummary,
		WindowSize: 2, SummaryBatchSize: 10, MaxHistoryChars: 12000, MaxSummaryChars: 10},
		&stubPlainCompleter{}, discardLogger())
	summary := "0123456789ABCDEF"
	got := c.Compress(context.Background(), turns5()[:2], &summary, 0, "p1")
	// keep = 10 - len("(摘要已截断)")(=7) = 3 → 前 3 字 + 标记。
	if got.Summary == nil || *got.Summary != "012(摘要已截断)" {
		t.Errorf("摘要应尾部截断: %v", got.Summary)
	}
}

// ===== 加载器 =====

func TestLoadHistory有界装配(t *testing.T) {
	ctx := context.Background()

	t.Run("窗口未满时全量装配且无摘要无写回", func(t *testing.T) {
		f := newCompressionFixture(nil) // window=2
		f.addDialogues(9, 2)
		got := f.loader.LoadHistory(ctx, 9, "p1")
		want := []string{"面试官：问1内容", "候选人：答1", "面试官：问2内容", "候选人：答2"}
		if len(got) != len(want) {
			t.Fatalf("行数不符:\n got  %v\n want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("第 %d 行不符: got %q want %q", i, got[i], want[i])
			}
		}
		if len(f.summary.promptList()) != 0 {
			t.Errorf("窗口未满不应调用摘要模型")
		}
		if len(f.store.savedSummaries) != 0 {
			t.Errorf("无新摘要不应写回边界: %v", f.store.savedSummaries)
		}
	})

	t.Run("窗口满且批满触发摘要并锁内写回新边界", func(t *testing.T) {
		f := newCompressionFixture(nil)
		f.store.addSessionAt(9)
		f.addDialogues(9, 5)
		covered := 0
		f.prependSummaryRow(9, summaryRow(9, -1, "", &covered))

		got := f.loader.LoadHistory(ctx, 9, "p1")
		// pending = 边界 0 之后、窗口 seq4 之前升序前 2 条 = seq1,seq2（Java
		// PageRequest.of(0, batchSize) 取最早批）→ 摘要触发 → 新边界 = turns[1].seq = 2。
		if len(got) == 0 || got[0] != "【对话摘要】新摘要" {
			t.Fatalf("history 首行应为摘要行: %v", got)
		}
		if !containsAll(strings.Join(got[1:], "\n"), "问4内容", "答4", "问5内容", "答5") {
			t.Errorf("摘要后应只保留最近窗口: %v", got)
		}
		if containsAll(strings.Join(got[1:], "\n"), "问1", "问2", "问3") {
			t.Errorf("已摘要轮次与窗口外轮次不应下发: %v", got)
		}
		// 摘要提示词只含待摘要批（seq1、seq2）。
		prompts := f.summary.promptList()
		if len(prompts) != 1 || !containsAll(prompts[0], "问1内容", "问2内容") || containsAll(prompts[0], "问3", "问4") {
			t.Errorf("摘要提示词应只含待摘要批: %v", prompts)
		}
		// 写回：summary + 边界 2，锁内顺序 lock → find → write。
		row, ok := f.store.savedSummaries[9]
		if !ok {
			t.Fatalf("应写回摘要行: %v", f.store.savedSummaries)
		}
		if row.summary == nil || *row.summary != "新摘要" || row.covered != 2 {
			t.Errorf("写回内容不符: %+v", row)
		}
		var steps []string
		for _, e := range f.store.rec.list() {
			if e == "lock-session" || e == "find-summary-row" || e == "write-summary-row" {
				steps = append(steps, e)
			}
		}
		wantSteps := []string{"lock-session", "find-summary-row", "write-summary-row"}
		if strings.Join(steps, ",") != strings.Join(wantSteps, ",") {
			t.Fatalf("锁内步骤顺序不符:\n got  %v\n want %v", steps, wantSteps)
		}
	})

	t.Run("写回失败不中断本次装配", func(t *testing.T) {
		f := newCompressionFixture(nil)
		f.store.addSessionAt(9)
		f.store.summarySaveErr = errString("库忙")
		f.addDialogues(9, 5)
		got := f.loader.LoadHistory(ctx, 9, "p1")
		if len(got) == 0 || got[0] != "【对话摘要】新摘要" {
			t.Fatalf("写回失败应仍返回压缩后历史: %v", got)
		}
	})
}

func TestLoadHistory旧摘要行边界迁移(t *testing.T) {
	ctx := context.Background()

	t.Run("负序号旧行按第N条消息定位边界并写回", func(t *testing.T) {
		f := newCompressionFixture(func(c *ContextCompressionConfig) { c.WindowSize = 20 })
		f.store.addSessionAt(9)
		f.addDialogues(9, 15)
		f.prependSummaryRow(9, summaryRow(9, -11, "旧摘要", nil)) // 旧编码：coveredTurns = 10

		got := f.loader.LoadHistory(ctx, 9, "p1")
		// 迁移后本轮不重复摘要：15 条全部在窗口内 → 直接带出旧摘要。
		if len(got) == 0 || got[0] != "【对话摘要】旧摘要" {
			t.Fatalf("应带出旧摘要行: %v", got)
		}
		if len(f.summary.promptList()) != 0 {
			t.Errorf("迁移不做重新摘要: %v", f.summary.promptList())
		}
		row, ok := f.store.savedSummaries[9]
		if !ok {
			t.Fatalf("迁移应写回边界: %v", f.store.savedSummaries)
		}
		if row.covered != 10 {
			t.Errorf("边界应为第 10 条消息的序号 10: %+v", row)
		}
		if row.summary == nil || *row.summary != "旧摘要" {
			t.Errorf("迁移写回应保留旧摘要正文: %+v", row)
		}
	})

	t.Run("负一序号旧行迁移边界为零", func(t *testing.T) {
		f := newCompressionFixture(func(c *ContextCompressionConfig) { c.WindowSize = 20 })
		f.store.addSessionAt(9)
		f.addDialogues(9, 3)
		f.prependSummaryRow(9, summaryRow(9, -1, "旧摘要", nil))

		f.loader.LoadHistory(ctx, 9, "p1")
		row, ok := f.store.savedSummaries[9]
		if !ok {
			t.Fatalf("迁移应写回: %v", f.store.savedSummaries)
		}
		if row.covered != 0 {
			t.Errorf("coveredTurns=0 时边界应为 0: %+v", row)
		}
	})

	t.Run("定位第N条失败时回退边界零", func(t *testing.T) {
		f := newCompressionFixture(func(c *ContextCompressionConfig) { c.WindowSize = 20 })
		f.store.addSessionAt(9)
		f.addDialogues(9, 3) // 只有 3 条，第 10 条不存在
		f.prependSummaryRow(9, summaryRow(9, -11, "旧摘要", nil))

		f.loader.LoadHistory(ctx, 9, "p1")
		row, ok := f.store.savedSummaries[9]
		if !ok {
			t.Fatalf("回退也应写回: %v", f.store.savedSummaries)
		}
		if row.covered != 0 {
			t.Errorf("定位失败应回退边界 0: %+v", row)
		}
	})
}

func TestLoadHistory异常降级窗口(t *testing.T) {
	ctx := context.Background()

	t.Run("有界加载失败降级为最近窗口", func(t *testing.T) {
		f := newCompressionFixture(nil)
		f.store.pendingErr = errString("批次查询失败")
		f.addDialogues(9, 5)
		got := f.loader.LoadHistory(ctx, 9, "p1")
		// 降级：最近窗口（window=2）+ 预算格式化，无摘要行。
		if len(got) == 0 || got[0] == "【对话摘要】新摘要" {
			t.Fatalf("降级不应有新摘要行: %v", got)
		}
		if !containsAll(strings.Join(got, "\n"), "问4内容", "问5内容") {
			t.Errorf("降级应只含最近窗口: %v", got)
		}
		if containsAll(strings.Join(got, "\n"), "问3内容") {
			t.Errorf("降级不应含窗口外轮次: %v", got)
		}
		if len(f.summary.promptList()) != 0 {
			t.Errorf("降级路径不应调用摘要模型")
		}
	})

	t.Run("窗口读取也失败时返回空历史", func(t *testing.T) {
		f := newCompressionFixture(nil)
		f.store.recentErr = errString("库不可用")
		got := f.loader.LoadHistory(ctx, 9, "p1")
		if len(got) != 0 {
			t.Fatalf("应返回空历史: %v", got)
		}
	})
}

func TestLoadHistory未启用走全量(t *testing.T) {
	f := newCompressionFixture(func(c *ContextCompressionConfig) {
		c.Enabled = false
	})
	long := strings.Repeat("长", 30000)
	f.store.setDialogues(9, []MessageEntity{qaTurn(9, 1, long, long)})

	got := f.loader.LoadHistory(context.Background(), 9, "p1")
	// enabled=false 是唯一关闭方式：不做字符硬预算，仅本地排查，全量返回。
	if len(got) != 2 {
		t.Fatalf("全量应两行: %d", len(got))
	}
	if len([]rune(got[0])) != len([]rune("面试官："))+30000 {
		t.Errorf("全量分支不裁剪: %d", len([]rune(got[0])))
	}
}

// ===== 锁内写回（fakeCompStore 镜像 Java saveSummaryRow 语义）=====

func TestSaveSummaryRow边界单调与行形态(t *testing.T) {
	setup := func(t *testing.T) (*fakeCompStore, int64) {
		t.Helper()
		s := newFakeCompStore()
		return s, s.fakeStore.addSession(inProgressSession(time.Hour))
	}

	t.Run("无既有行时新建负序号摘要行", func(t *testing.T) {
		s, id := setup(t)
		summary := "摘要正文"
		if err := s.SaveSummaryRow(context.Background(), id, &summary, 7); err != nil {
			t.Fatalf("写回应成功: %v", err)
		}
		rows := s.fakeStore.dialogueRows(id)
		if len(rows) != 1 {
			t.Fatalf("应恰好一行: %v", rows)
		}
		m := rows[0]
		if m.MessageType != MessageSummary {
			t.Errorf("类型应为 SUMMARY: %s", m.MessageType)
		}
		if m.SequenceNum == nil || *m.SequenceNum != -1 {
			t.Errorf("新行序号应为 -1: %v", m.SequenceNum)
		}
		if m.SummaryCoveredSequenceNum == nil || *m.SummaryCoveredSequenceNum != 7 {
			t.Errorf("覆盖边界应为 7: %v", m.SummaryCoveredSequenceNum)
		}
		if m.AiGeneratedText == nil || *m.AiGeneratedText != "摘要正文" {
			t.Errorf("摘要正文应写入: %v", m.AiGeneratedText)
		}
	})

	t.Run("边界回退被拒绝且行不变", func(t *testing.T) {
		s, id := setup(t)
		covered := 10
		s.setDialogues(id, []MessageEntity{summaryRow(id, -1, "旧摘要", &covered)})
		newer := "新摘要"
		if err := s.SaveSummaryRow(context.Background(), id, &newer, 5); err != nil {
			t.Fatalf("拒绝不算错误: %v", err)
		}
		m := s.fakeStore.dialogueRows(id)[0]
		if *m.AiGeneratedText != "旧摘要" || *m.SummaryCoveredSequenceNum != 10 {
			t.Errorf("拒绝后行应不变: %+v", m)
		}
	})

	t.Run("边界相等允许重复写回", func(t *testing.T) {
		s, id := setup(t)
		covered := 10
		s.setDialogues(id, []MessageEntity{summaryRow(id, -1, "旧摘要", &covered)})
		same := "重写摘要"
		if err := s.SaveSummaryRow(context.Background(), id, &same, 10); err != nil {
			t.Fatalf("写回应成功: %v", err)
		}
		m := s.fakeStore.dialogueRows(id)[0]
		if *m.AiGeneratedText != "重写摘要" {
			t.Errorf("边界相等应允许覆盖: %+v", m)
		}
	})

	t.Run("旧迁移序号保留负值排序语义", func(t *testing.T) {
		s, id := setup(t)
		s.setDialogues(id, []MessageEntity{summaryRow(id, -11, "旧摘要", nil)})
		migrated := "迁移摘要"
		if err := s.SaveSummaryRow(context.Background(), id, &migrated, 10); err != nil {
			t.Fatalf("写回应成功: %v", err)
		}
		m := s.fakeStore.dialogueRows(id)[0]
		if m.SequenceNum == nil || *m.SequenceNum != -11 {
			t.Errorf("序号应保留 -11: %v", m.SequenceNum)
		}
	})

	t.Run("会话不存在返回错误", func(t *testing.T) {
		s, _ := setup(t)
		summary := "摘要"
		if err := s.SaveSummaryRow(context.Background(), 999, &summary, 1); err != ErrSessionNotFound {
			t.Fatalf("应返回会话不存在: %v", err)
		}
	})
}
