// 知识库链路单测的内存/脚本化依赖（先例 internal/voiceinterview/evaluation_fakes_test.go）：
// fakeStore（Java 条件更新语义的内存镜像）、fakeStream、fakeStorage、fakeVectors、
// fakeParser、fakeVectorizer + 动作记录器。spec 测试决策：只测外部行为，DB 条件
// 更新 SQL 语义由票 05 smoke 真库兜底。
package knowledgebase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ===== 动作记录器 =====

type actionRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *actionRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *actionRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// containsEvent 事件列表是否含任一前缀。
func containsEvent(events []string, prefix string) bool {
	for _, e := range events {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// assertEventOrder 断言事件按序出现。
func assertEventOrder(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, events []string, first, second string) {
	t.Helper()
	firstIdx, secondIdx := -1, -1
	for i, e := range events {
		if len(e) >= len(first) && e[:len(first)] == first && firstIdx < 0 {
			firstIdx = i
		}
		if len(e) >= len(second) && e[:len(second)] == second {
			secondIdx = i
		}
	}
	if firstIdx < 0 || secondIdx < 0 || firstIdx > secondIdx {
		t.Fatalf("事件顺序不符: want %q 先于 %q, got %v", first, second, events)
	}
}

// ===== fakeStore（Store 的内存实现）=====

type fakeStore struct {
	rec     *actionRecorder
	mu      sync.Mutex
	kbs     map[int64]*KnowledgeBaseEntity
	vectors []vectorRow
	nextID  int64

	insertErr           error
	activateErr         error
	deleteJobErr        error
	heartbeats          int
	lastCompleteAttempt string
	lastFailAttempt     string
	// findErr 非 nil 时 FindByID 恒返回该错误（模拟 DB 故障，区别于不存在）。
	findErr error
	// deleteAfterFind 首次 FindByID 正常、其后返回不存在（模拟 skip 后删除窗口）。
	deleteAfterFind bool
	findCalls       int
}

type vectorRow struct {
	content  string
	metadata map[string]any
}

func newFakeStore(rec *actionRecorder) *fakeStore {
	return &fakeStore{rec: rec, kbs: map[int64]*KnowledgeBaseEntity{}, nextID: 100}
}

func (s *fakeStore) addKB(kb *KnowledgeBaseEntity) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	kb.ID = s.nextID
	s.kbs[kb.ID] = kb
	return kb.ID
}

func (s *fakeStore) snapshot(id int64) *KnowledgeBaseEntity {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[id]
	if !ok {
		return nil
	}
	cp := *kb
	return &cp
}

func (s *fakeStore) FindByID(ctx context.Context, id int64) (*KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findCalls++
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.deleteAfterFind && s.findCalls >= 2 {
		return nil, ErrKnowledgeBaseNotFound
	}
	kb, ok := s.kbs[id]
	if !ok {
		return nil, ErrKnowledgeBaseNotFound
	}
	cp := *kb
	return &cp, nil
}

func (s *fakeStore) FindByFileHash(ctx context.Context, hash string) (*KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kb := range s.kbs {
		if kb.FileHash == hash {
			cp := *kb
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *fakeStore) InsertKB(ctx context.Context, kb *KnowledgeBaseEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	s.nextID++
	kb.ID = s.nextID
	s.kbs[kb.ID] = kb
	return nil
}

func (s *fakeStore) IncrementAccessCount(ctx context.Context, id int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("access-count")
	if kb, ok := s.kbs[id]; ok {
		kb.AccessCount = intPtr(derefInt(kb.AccessCount) + 1)
		kb.LastAccessedAt = timePtr(at)
	}
	return nil
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (s *fakeStore) TryMarkVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorPending {
		return 0, nil
	}
	kb.VectorStatus = VectorProcessing
	kb.VectorAttemptID = stringPtr(attemptID)
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) HeartbeatVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("heartbeat")
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorProcessing || kb.VectorAttemptID == nil || *kb.VectorAttemptID != attemptID {
		return 0, nil
	}
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) CompleteVectorIfProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("complete-if-processing")
	s.lastCompleteAttempt = attemptID
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorProcessing || kb.VectorAttemptID == nil || *kb.VectorAttemptID != attemptID {
		return 0, nil
	}
	kb.VectorStatus = VectorCompleted
	kb.VectorError = nil
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) FailVectorIfProcessing(ctx context.Context, id int64, attemptID, errMsg string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("fail-if-processing")
	s.lastFailAttempt = attemptID
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorProcessing || kb.VectorAttemptID == nil || *kb.VectorAttemptID != attemptID {
		return 0, nil
	}
	kb.VectorStatus = VectorFailed
	kb.VectorError = stringPtr(errMsg)
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) FailVectorIfPending(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("fail-if-pending")
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorPending {
		return 0, nil
	}
	kb.VectorStatus = VectorFailed
	kb.VectorError = stringPtr(errMsg)
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) ResetVectorToPending(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("reset-to-pending")
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorProcessing || kb.VectorAttemptID == nil || *kb.VectorAttemptID != attemptID {
		return 0, nil
	}
	kb.VectorStatus = VectorPending
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) UpdateVectorStatusToPending(ctx context.Context, id int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("update-status-pending")
	kb, ok := s.kbs[id]
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	kb.VectorStatus = VectorPending
	kb.VectorError = nil
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return nil
}

func (s *fakeStore) ResetVectorRecoveryCount(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("reset-recovery-count")
	if kb, ok := s.kbs[id]; ok {
		kb.VectorRecoveryCount = 0
	}
	return nil
}

func (s *fakeStore) TouchQueuedVectorForRecovery(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("touch-queued")
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorPending || kb.VectorUpdatedAt == nil || !kb.VectorUpdatedAt.Before(threshold) {
		return 0, nil
	}
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) ResetStaleVectorProcessing(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("reset-stale-processing")
	kb, ok := s.kbs[id]
	if !ok || kb.VectorStatus != VectorProcessing || kb.VectorUpdatedAt == nil || !kb.VectorUpdatedAt.Before(threshold) {
		return 0, nil
	}
	kb.VectorStatus = VectorPending
	kb.VectorAttemptID = nil
	kb.VectorUpdatedAt = timePtr(now)
	return 1, nil
}

func (s *fakeStore) IncrementVectorRecoveryCount(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("increment-recovery-count")
	if kb, ok := s.kbs[id]; ok {
		kb.VectorRecoveryCount++
	}
	return nil
}

func (s *fakeStore) FindStaleByVectorStatus(ctx context.Context, status VectorStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, kb := range s.kbs {
		if kb.VectorStatus != status {
			continue
		}
		if kb.VectorUpdatedAt == nil || !kb.VectorUpdatedAt.Before(threshold) {
			continue
		}
		out = append(out, *kb)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].VectorUpdatedAt, out[j].VectorUpdatedAt
		if ti == nil || tj == nil {
			return tj == nil
		}
		return ti.Before(*tj)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ActivateVectorJobAndUpdateSnapshot 内存镜像 Java activateVectorJobAndUpdateSnapshot：
// 锁内校验代次 → 删旧正式向量（kb_id 命中）→ promote 临时向量（按 jobId）→ 写快照。
func (s *fakeStore) ActivateVectorJobAndUpdateSnapshot(ctx context.Context, kbID int64, attemptID, jobID string, chunkCount int, vectorConfig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("activate-job")
	if s.activateErr != nil {
		return s.activateErr
	}
	kb, ok := s.kbs[kbID]
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	if kb.VectorStatus != VectorProcessing || kb.VectorAttemptID == nil || *kb.VectorAttemptID != attemptID {
		return errors.New("[6006] 向量化任务执行权已失效")
	}
	// 删旧正式向量。
	kept := s.vectors[:0]
	for _, row := range s.vectors {
		if row.metadata[metadataKBID] == strconv.FormatInt(kbID, 10) {
			continue
		}
		kept = append(kept, row)
	}
	s.vectors = kept
	// promote 临时向量。
	for i, row := range s.vectors {
		if row.metadata[metadataJobID] == jobID {
			s.vectors[i].metadata[metadataKBID] = strconv.FormatInt(kbID, 10)
			delete(s.vectors[i].metadata, metadataJobID)
			delete(s.vectors[i].metadata, metadataTarget)
		}
	}
	now := time.Now()
	kb.ChunkCount = intPtr(chunkCount)
	kb.VectorConfig = stringPtr(vectorConfig)
	kb.VectorizedAt = timePtr(now)
	kb.VectorUpdatedAt = timePtr(now)
	return nil
}

func (s *fakeStore) DeleteVectorsByJobID(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("delete-by-job")
	if s.deleteJobErr != nil {
		return s.deleteJobErr
	}
	kept := s.vectors[:0]
	for _, row := range s.vectors {
		if row.metadata[metadataJobID] == jobID {
			continue
		}
		kept = append(kept, row)
	}
	s.vectors = kept
	return nil
}

// vectorRows 快照（断言用）。
func (s *fakeStore) vectorRows() []vectorRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]vectorRow(nil), s.vectors...)
}

// ===== fakeStream =====

type fakeStream struct {
	rec     *actionRecorder
	mu      sync.Mutex
	adds    []map[string]any
	acks    []string
	addErr  error
	readErr error
	// reads/claims 为脚本化消费队列（XReadNew/XClaimPending 各自弹出）。
	reads  [][]VectorizeMessage
	claims [][]VectorizeMessage
}

func newFakeStream(rec *actionRecorder) *fakeStream {
	return &fakeStream{rec: rec}
}

func (s *fakeStream) EnsureGroup(ctx context.Context) error { return nil }

func (s *fakeStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("xadd")
	if s.addErr != nil {
		return "", s.addErr
	}
	s.adds = append(s.adds, values)
	return "1-1", nil
}

func (s *fakeStream) XAck(ctx context.Context, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("ack")
	s.acks = append(s.acks, messageID)
	return nil
}

func (s *fakeStream) XReadNew(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	if len(s.reads) == 0 {
		return nil, nil
	}
	batch := s.reads[0]
	s.reads = s.reads[1:]
	return batch, nil
}

func (s *fakeStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.claims) == 0 {
		return nil, nil
	}
	batch := s.claims[0]
	s.claims = s.claims[1:]
	return batch, nil
}

func (s *fakeStream) addAt(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adds[i]
}

func (s *fakeStream) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

// ===== fakeStorage =====

type fakeStorage struct {
	mu          sync.Mutex
	objects     map[string][]byte
	uploaded    []string
	deleted     []string
	uploadErr   error
	deleteErr   error
	downloadErr error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: map[string][]byte{}}
}

func (s *fakeStorage) UploadKnowledgeBase(ctx context.Context, fileName, contentType string, data []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadErr != nil {
		return "", s.uploadErr
	}
	key := "knowledgebases/2026/10/29/abcd1234_" + fileName
	s.objects[key] = append([]byte(nil), data...)
	s.uploaded = append(s.uploaded, key)
	return key, nil
}

func (s *fakeStorage) DeleteKnowledgeBase(ctx context.Context, fileKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, fileKey)
	s.deleted = append(s.deleted, fileKey)
	return nil
}

func (s *fakeStorage) Download(ctx context.Context, fileKey string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downloadErr != nil {
		return nil, s.downloadErr
	}
	data, ok := s.objects[fileKey]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStorage) FileURL(fileKey string) string {
	return "http://localhost:9000/bucket/" + fileKey
}

func (s *fakeStorage) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// ===== fakeVectors =====

// fakeVectors 向量写入替身：与 fakeStore 共享同一张"表"（对齐真实部署里
// vector.Store 写入与 Repo promote/delete 操作同一 vector_store 表）。
type fakeVectors struct {
	store *fakeStore
	// pendingRows 按 AddDocuments 时刻留档的行（promote 之前的临时态，供断言）。
	pendingRows []vectorRow
	batchSizes  []int
	addErr      error
}

func (f *fakeVectors) AddDocuments(ctx context.Context, contents []string, metadatas []map[string]any) error {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	for i := range contents {
		pendingMeta := make(map[string]any, len(metadatas[i]))
		for k, v := range metadatas[i] {
			pendingMeta[k] = v
		}
		row := vectorRow{content: contents[i], metadata: metadatas[i]}
		f.store.vectors = append(f.store.vectors, row)
		f.pendingRows = append(f.pendingRows, vectorRow{content: contents[i], metadata: pendingMeta})
	}
	f.batchSizes = append(f.batchSizes, len(contents))
	return nil
}

func (f *fakeVectors) batchCount() int {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	return len(f.batchSizes)
}

func (f *fakeVectors) rowCount() int {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	return len(f.store.vectors)
}

// ===== fakeParser / fakeVectorizer =====

type fakeParser struct {
	content string
	err     error
	calls   int
	// hook 在解析调用时触发（模拟消费中途的实体变更等窗口）。
	hook func()
}

func (f *fakeParser) DownloadAndParseContent(ctx context.Context, storageKey, originalFilename string) (string, error) {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	return f.content, f.err
}

type fakeVectorizer struct {
	mu           sync.Mutex
	calls        int
	lastKbID     int64
	lastAttempt  string
	lastContent  string
	lastSource   SourceInfo
	heartbeats   int
	heartbeatErr error
	err          error
}

func (f *fakeVectorizer) VectorizeAndStore(ctx context.Context, kbID int64, content string, attemptID string, heartbeat func() error, source SourceInfo) error {
	f.mu.Lock()
	f.calls++
	f.lastKbID = kbID
	f.lastAttempt = attemptID
	f.lastContent = content
	f.lastSource = source
	f.mu.Unlock()
	if err := heartbeat(); err != nil {
		return err
	}
	f.mu.Lock()
	f.heartbeats++
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ===== 共享小工具 =====

// kbFixture 向量化/消费链路夹具。
type kbFixture struct {
	rec     *actionRecorder
	store   *fakeStore
	stream  *fakeStream
	storage *fakeStorage
	vectors *fakeVectors
	parser  *fakeParser
	vec     *fakeVectorizer
	now     time.Time
}

func newKBFixture() *kbFixture {
	rec := &actionRecorder{}
	store := newFakeStore(rec)
	return &kbFixture{
		rec:     rec,
		store:   store,
		stream:  newFakeStream(rec),
		storage: newFakeStorage(),
		vectors: &fakeVectors{store: store},
		parser:  &fakeParser{},
		vec:     &fakeVectorizer{},
		now:     time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	}
}

// addProcessingKB 构造一个已领取（PROCESSING + attemptId）的知识库。
func (f *kbFixture) addProcessingKB(id int64) *KnowledgeBaseEntity {
	kb := sampleKB(id)
	kb.VectorStatus = VectorProcessing
	kb.VectorAttemptID = stringPtr("attempt-1")
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.kbs[id] = kb
	return kb
}

// sampleKB 构造标准知识库实体。
func sampleKB(id int64) *KnowledgeBaseEntity {
	uploaded := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	updated := uploaded
	return &KnowledgeBaseEntity{
		ID:               id,
		FileHash:         "deadbeef" + pad64("deadbeef"),
		Name:             "面试笔记",
		OriginalFilename: "面试笔记.md",
		FileSize:         int64Ptr(1024),
		ContentType:      stringPtr("text/markdown"),
		StorageKey:       stringPtr("knowledgebases/2026/10/29/abcd1234_面试笔记.md"),
		StorageURL:       stringPtr("http://localhost:9000/bucket/knowledgebases/2026/10/29/abcd1234_面试笔记.md"),
		UploadedAt:       uploaded,
		LastAccessedAt:   timePtr(uploaded),
		AccessCount:      intPtr(1),
		QuestionCount:    intPtr(0),
		VectorStatus:     VectorPending,
		ChunkCount:       intPtr(0),
		VectorUpdatedAt:  timePtr(updated),
	}
}

func pad64(s string) string {
	for len(s) < 64 {
		s += "0"
	}
	return s
}

// itoa int64 → 十进制字符串（测试断言用）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
