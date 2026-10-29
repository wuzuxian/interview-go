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
	"strings"
	"sync"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
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

	questions      map[int64]*KnowledgeBaseQuestionEntity
	questionNextID int64

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

	// deleteVectorsErr DeleteVectorsByKBID 脚本化失败（删除补偿链测试）。
	deleteVectorsErr error
	// userRagMessages RAG 会话 USER 消息数（stats totalQuestionCount 口径）。
	userRagMessages int64
	// ragJoinRows RAG 会话-知识库关联（sessionID 集合按 kbID 索引），删除补偿链断言用。
	ragJoinRows map[int64]map[int64]bool

	// failResetForRetry 脚本化 ResetQuestionForRetry 失败（条件重置不命中：任务已被替换）。
	failResetForRetry bool
}

type vectorRow struct {
	content  string
	metadata map[string]any
}

func newFakeStore(rec *actionRecorder) *fakeStore {
	return &fakeStore{rec: rec, kbs: map[int64]*KnowledgeBaseEntity{}, ragJoinRows: map[int64]map[int64]bool{},
		questions: map[int64]*KnowledgeBaseQuestionEntity{}, nextID: 100, questionNextID: 900}
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

// ===== fakeStore 管理查询/删除（票 02，内存镜像 Java 派生查询语义）=====

// FindAllByIDs 按主键集合返回（内存镜像 Java findAllById，去重输出）。
func (s *fakeStore) FindAllByIDs(ctx context.Context, ids []int64) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, id := range ids {
		if kb, ok := s.kbs[id]; ok {
			out = append(out, *kb)
		}
	}
	return out, nil
}

// IncrementQuestionCountBatch 提问计数批量 +1（内存镜像单条 SQL 批量更新）。
func (s *fakeStore) IncrementQuestionCountBatch(ctx context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		kb, ok := s.kbs[id]
		if !ok {
			continue
		}
		n := 0
		if kb.QuestionCount != nil {
			n = *kb.QuestionCount
		}
		n++
		kb.QuestionCount = &n
	}
	return nil
}

// listByUploadedAtDesc 全部实体按 uploadedAt 倒序快照。
func (s *fakeStore) listByUploadedAtDesc() []KnowledgeBaseEntity {
	out := make([]KnowledgeBaseEntity, 0, len(s.kbs))
	for _, kb := range s.kbs {
		out = append(out, *kb)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UploadedAt.After(out[j].UploadedAt) })
	return out
}

func (s *fakeStore) FindAllOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listByUploadedAtDesc(), nil
}

func (s *fakeStore) FindByVectorStatusOrderByUploadedAtDesc(ctx context.Context, status VectorStatus) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, kb := range s.listByUploadedAtDesc() {
		if kb.VectorStatus == status {
			out = append(out, kb)
		}
	}
	return out, nil
}

func (s *fakeStore) FindAllCategories(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, kb := range s.kbs {
		if kb.Category == nil || seen[*kb.Category] {
			continue
		}
		seen[*kb.Category] = true
		out = append(out, *kb.Category)
	}
	sort.Strings(out)
	return out, nil
}

func (s *fakeStore) FindByCategoryOrderByUploadedAtDesc(ctx context.Context, category string) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, kb := range s.listByUploadedAtDesc() {
		if kb.Category != nil && *kb.Category == category {
			out = append(out, kb)
		}
	}
	return out, nil
}

func (s *fakeStore) FindByCategoryIsNullOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, kb := range s.listByUploadedAtDesc() {
		if kb.Category == nil {
			out = append(out, kb)
		}
	}
	return out, nil
}

func (s *fakeStore) SearchByKeyword(ctx context.Context, keyword string) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(keyword)
	var out []KnowledgeBaseEntity
	for _, kb := range s.listByUploadedAtDesc() {
		if strings.Contains(strings.ToLower(kb.Name), lower) ||
			strings.Contains(strings.ToLower(kb.OriginalFilename), lower) {
			out = append(out, kb)
		}
	}
	return out, nil
}

func (s *fakeStore) UpdateCategory(ctx context.Context, id int64, category *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[id]
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	kb.Category = category
	return nil
}

func (s *fakeStore) CountKnowledgeBases(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.kbs)), nil
}

func (s *fakeStore) CountByVectorStatus(ctx context.Context, status VectorStatus) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, kb := range s.kbs {
		if kb.VectorStatus == status {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) SumAccessCount(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, kb := range s.kbs {
		n += int64(derefInt(kb.AccessCount))
	}
	return n, nil
}

func (s *fakeStore) CountUserRagMessages(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userRagMessages, nil
}

// addUserRagMessages 累加 USER 消息数（统计契约测试）。
func (s *fakeStore) addUserRagMessages(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userRagMessages += n
}

// addRagAssociation 记录会话-知识库关联（删除补偿链测试）。
func (s *fakeStore) addRagAssociation(kbID, sessionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ragJoinRows[kbID] == nil {
		s.ragJoinRows[kbID] = map[int64]bool{}
	}
	s.ragJoinRows[kbID][sessionID] = true
}

// ragAssociationCount 剩余关联数（断言摘除）。
func (s *fakeStore) ragAssociationCount(kbID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ragJoinRows[kbID])
}

// DeleteKnowledgeBaseRecords 内存镜像事务内删除：摘关联 → 删记录 → 返回 storageKey。
func (s *fakeStore) DeleteKnowledgeBaseRecords(ctx context.Context, id int64) (*string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("delete-kb-records:" + itoa(id))
	kb, ok := s.kbs[id]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "知识库不存在")
	}
	delete(s.ragJoinRows, id)
	delete(s.kbs, id)
	return kb.StorageKey, nil
}

// DeleteVectorsByKBID 内存镜像向量删除（按 metadata kb_id）；脚本化失败返回 6005。
func (s *fakeStore) DeleteVectorsByKBID(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("delete-vectors-by-kb:" + itoa(id))
	if s.deleteVectorsErr != nil {
		return apperr.Wrap(apperr.CodeKnowledgeBaseDeleteFailed, "删除向量数据失败", s.deleteVectorsErr)
	}
	kept := s.vectors[:0]
	for _, row := range s.vectors {
		if row.metadata[metadataKBID] == strconv.FormatInt(id, 10) {
			continue
		}
		kept = append(kept, row)
	}
	s.vectors = kept
	return nil
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
	rec         *actionRecorder
	mu          sync.Mutex
	objects     map[string][]byte
	uploaded    []string
	deleted     []string
	uploadErr   error
	deleteErr   error
	downloadErr error
}

func newFakeStorage(rec *actionRecorder) *fakeStorage {
	return &fakeStorage{rec: rec, objects: map[string][]byte{}}
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
	if s.rec != nil {
		s.rec.add("delete-storage-object:" + fileKey)
	}
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
		// 与生产 internal/storage.Download 同构：缺失对象 4002 + 同文案。
		return nil, apperr.New(apperr.CodeStorageDownloadFail, "文件不存在: "+fileKey)
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

// putObject 直写对象（下载契约测试用）。
func (s *fakeStorage) putObject(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
}

// objectBytes 读取对象内容（下载契约断言用）。
func (s *fakeStorage) objectBytes(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), data...), true
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
	qstream *fakeStream
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
		qstream: newFakeStream(rec),
		storage: newFakeStorage(rec),
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

// ===== fakeStore 题库与出题状态机（票 04，内存镜像 Java 事务语义）=====

// addQuestion 落一行题目，返回 ID。
func (s *fakeStore) addQuestion(q *KnowledgeBaseQuestionEntity) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questionNextID++
	q.ID = s.questionNextID
	s.questions[q.ID] = q
	return q.ID
}

// questionsOf 某知识库的题目快照（updatedAt 倒序，镜像列表查询）。
func (s *fakeStore) questionsOf(kbID int64) []KnowledgeBaseQuestionEntity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.questionsFiltered(kbID, nil)
}

// questionsFiltered status 过滤版本的快照（updatedAt 倒序，同刻以 ID 倒序定序，
// 镜像 Java 查询在自增主键下的稳定输出）。
func (s *fakeStore) questionsFiltered(kbID int64, status *QuestionBankStatus) []KnowledgeBaseQuestionEntity {
	var out []KnowledgeBaseQuestionEntity
	for _, q := range s.questions {
		if q.KnowledgeBaseID != kbID {
			continue
		}
		if status != nil && q.Status != *status {
			continue
		}
		out = append(out, *q)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// questionCount 某知识库题目数（断言用）。
func (s *fakeStore) questionCount(kbID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.questions {
		if q.KnowledgeBaseID == kbID {
			n++
		}
	}
	return n
}

func (s *fakeStore) FindQuestionsByKBOrderByUpdatedAtDesc(ctx context.Context, kbID int64) ([]KnowledgeBaseQuestionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.questionsFiltered(kbID, nil), nil
}

func (s *fakeStore) FindQuestionsByKBAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.questionsFiltered(kbID, &status), nil
}

func (s *fakeStore) FindCategoryCounts(ctx context.Context, kbID int64) ([]CategoryCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int64{}
	for _, q := range s.questions {
		if q.KnowledgeBaseID != kbID || q.Category == nil || *q.Category == "" {
			continue
		}
		counts[*q.Category]++
	}
	out := make([]CategoryCount, 0, len(counts))
	for category, count := range counts {
		out = append(out, CategoryCount{Category: category, Count: count})
	}
	// Java JPQL：count 降序、category 升序。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Category < out[j].Category
	})
	return out, nil
}

func (s *fakeStore) FindTop20QuestionsByKBAndDifficultyOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty string) ([]KnowledgeBaseQuestionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseQuestionEntity
	for _, q := range s.questions {
		if q.KnowledgeBaseID == kbID && q.Difficulty != nil && *q.Difficulty == difficulty {
			out = append(out, *q)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

func (s *fakeStore) FindQuestionByID(ctx context.Context, id int64) (*KnowledgeBaseQuestionEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.questions[id]
	if !ok {
		return nil, ErrQuestionNotFound
	}
	cp := *q
	return &cp, nil
}

func (s *fakeStore) InsertQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questionNextID++
	q.ID = s.questionNextID
	s.questions[q.ID] = q
	return nil
}

func (s *fakeStore) UpdateQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.questions[q.ID]; !ok {
		return ErrQuestionNotFound
	}
	cp := *q
	s.questions[q.ID] = &cp
	return nil
}

func (s *fakeStore) DeleteQuestionByID(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.questions[id]; !ok {
		return ErrQuestionNotFound
	}
	delete(s.questions, id)
	return nil
}

// CreateQuestionGenTask 内存镜像 Java createTask：锁内校验向量完成 + 状态空闲，
// 写入 QUEUED + taskId + 配置快照；校验失败返回业务错误。
func (s *fakeStore) CreateQuestionGenTask(ctx context.Context, kbID int64, taskID, configJSON string, now time.Time) (*KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok {
		return nil, ErrKnowledgeBaseNotFound
	}
	if kb.VectorStatus != VectorCompleted {
		return nil, apperr.New(apperr.CodeBadRequest, "知识库尚未完成向量化")
	}
	if kb.QuestionGenStatus == QuestionGenQueued || kb.QuestionGenStatus == QuestionGenProcessing {
		return nil, apperr.New(apperr.CodeBadRequest, "知识库问题正在生成中，请勿重复提交")
	}
	kb.QuestionGenTaskID = stringPtr(taskID)
	kb.QuestionGenStatus = QuestionGenQueued
	kb.QuestionGenConfig = stringPtr(configJSON)
	kb.QuestionGenError = nil
	kb.QuestionGenMessage = nil
	kb.QuestionGenSavedCount = 0
	kb.QuestionGenSkippedCount = 0
	kb.QuestionGenUpdatedAt = timePtr(now)
	cp := *kb
	return &cp, nil
}

func (s *fakeStore) TryMarkQuestionProcessing(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID || kb.QuestionGenStatus != QuestionGenQueued {
		return false, nil
	}
	kb.QuestionGenStatus = QuestionGenProcessing
	kb.QuestionGenError = nil
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

func (s *fakeStore) ResetQuestionForRetry(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failResetForRetry {
		return false, nil
	}
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID || kb.QuestionGenStatus != QuestionGenProcessing {
		return false, nil
	}
	kb.QuestionGenStatus = QuestionGenQueued
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

func (s *fakeStore) MarkQuestionGenFailed(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID {
		return false, nil
	}
	if kb.QuestionGenStatus == QuestionGenCompleted {
		return false, nil
	}
	kb.QuestionGenStatus = QuestionGenFailed
	kb.QuestionGenError = stringPtr(safeFailureMessage)
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

// ReplaceQuestionsAndComplete 内存镜像 Java replaceQuestionsAndComplete 小事务：
// 锁内校验 taskId 仍匹配且状态 PROCESSING → 删旧题 → 存新题（DRAFT）→ COMPLETED。
func (s *fakeStore) ReplaceQuestionsAndComplete(ctx context.Context, kbID int64, taskID string, questions []KnowledgeBaseQuestionEntity, skippedCount int, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID || kb.QuestionGenStatus != QuestionGenProcessing {
		return false, nil
	}
	for id, q := range s.questions {
		if q.KnowledgeBaseID == kbID {
			delete(s.questions, id)
		}
	}
	for i := range questions {
		s.questionNextID++
		q := questions[i]
		q.ID = s.questionNextID
		s.questions[q.ID] = &q
	}
	message := "已生成 " + strconv.Itoa(len(questions)) + " 道题"
	if skippedCount > 0 {
		message = "已生成 " + strconv.Itoa(len(questions)) + " 道题，跳过 " + strconv.Itoa(skippedCount) + " 道重复题"
	}
	kb.QuestionGenStatus = QuestionGenCompleted
	kb.QuestionGenError = nil
	kb.QuestionGenMessage = stringPtr(message)
	kb.QuestionGenSavedCount = len(questions)
	kb.QuestionGenSkippedCount = skippedCount
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

func (s *fakeStore) TouchQueuedQuestionForRecovery(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID ||
		kb.QuestionGenStatus != QuestionGenQueued ||
		(kb.QuestionGenUpdatedAt != nil && !kb.QuestionGenUpdatedAt.Before(threshold)) {
		return false, nil
	}
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

func (s *fakeStore) ResetStaleQuestionProcessing(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kb, ok := s.kbs[kbID]
	if !ok || kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID ||
		kb.QuestionGenStatus != QuestionGenProcessing ||
		(kb.QuestionGenUpdatedAt != nil && !kb.QuestionGenUpdatedAt.Before(threshold)) {
		return false, nil
	}
	kb.QuestionGenStatus = QuestionGenQueued
	kb.QuestionGenUpdatedAt = timePtr(now)
	return true, nil
}

func (s *fakeStore) FindStaleQuestionGenTasks(ctx context.Context, status QuestionGenStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnowledgeBaseEntity
	for _, kb := range s.kbs {
		if kb.QuestionGenStatus != status {
			continue
		}
		if kb.QuestionGenUpdatedAt == nil || !kb.QuestionGenUpdatedAt.Before(threshold) {
			continue
		}
		out = append(out, *kb)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].QuestionGenUpdatedAt, out[j].QuestionGenUpdatedAt
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
