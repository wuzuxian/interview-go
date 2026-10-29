// 向量化服务单测：metadata 协议（ADR 0005 键名逐字）、source_id 稳定标识、
// 分批写入（DashScope batch ≤ 10）、两阶段 promote 与失败清理。
// 契约来源：Java KnowledgeBaseVectorService.vectorizeAndStore 逐字。
package knowledgebase

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/knowledgebase/textsplit"
)

// newVectorizeService 构建真实切分器 + fake 写入/存储。
func newVectorizeService(f *kbFixture, cfg textsplit.Config, batchSize int) *VectorizeService {
	splitter := textsplit.NewTokenTextSplitter(cfg)
	return NewVectorizeService(splitter, cfg, batchSize, f.store, f.vectors, discardLogger())
}

func TestVectorizeMetadata协议逐字对齐(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(7)
	// 两句以上确保多块（chunkSize 收小避免超长输入）。
	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 30
	cfg.MinChunkSizeChars = 10
	svc := newVectorizeService(f, cfg, 10)

	content := "第一句话讲述分布式系统的基本概念与CAP定理的权衡取舍。第二句话讨论一致性哈希在分片场景下的应用与热点问题。第三句话介绍共识协议Raft的领导者选举机制。"
	heartbeats := 0
	err := svc.VectorizeAndStore(context.Background(), 7, content, "attempt-1", func() error {
		heartbeats++
		return nil
	}, SourceInfo{Name: "面试笔记", Filename: "面试笔记.md", FileHash: "hash123"})
	if err != nil {
		t.Fatalf("向量化失败: %v", err)
	}

	// 断言写入时刻（promote 之前）的临时态 metadata。
	rows := f.vectors.pendingRows
	if len(rows) < 2 {
		t.Fatalf("应写入多块向量, got %d", len(rows))
	}
	configJSON, _ := cfg.ConfigJSON()
	for i, row := range rows {
		meta := row.metadata
		jobID, _ := meta[metadataJobID].(string)
		if jobID == "" {
			t.Fatalf("第 %d 行缺少 kb_vector_job_id: %v", i, meta)
		}
		// 临时 kb_id = pending:<kbId>:<jobId>（检索永不命中半成品）。
		if meta[metadataKBID] != "pending:7:"+jobID {
			t.Errorf("第 %d 行 kb_id = %v, want pending:7:<jobId>", i, meta[metadataKBID])
		}
		if meta[metadataTarget] != "7" {
			t.Errorf("第 %d 行 kb_target_id = %v, want \"7\"", i, meta[metadataTarget])
		}
		if meta[metadataChunkStrategy] != "token-v1" {
			t.Errorf("第 %d 行 chunk_strategy = %v, want token-v1", i, meta[metadataChunkStrategy])
		}
		if meta["source_kb_id"] != "7" || meta["source_name"] != "面试笔记" || meta["source_filename"] != "面试笔记.md" {
			t.Errorf("第 %d 行 source_* = %v/%v/%v", i, meta["source_kb_id"], meta["source_name"], meta["source_filename"])
		}
		if meta["chunk_index"] != i || meta["total_chunks"] != len(rows) {
			t.Errorf("第 %d 行 chunk_index/total_chunks = %v/%v, want %d/%d", i, meta["chunk_index"], meta["total_chunks"], i, len(rows))
		}
		// source_id = md5(fileHash + "\n" + configJson + "\n" + i)（Java DigestUtils 小写十六进制）。
		wantSourceID := md5Hex([]byte("hash123\n" + configJSON + "\n" + strconv.Itoa(i)))
		if meta["source_id"] != wantSourceID {
			t.Errorf("第 %d 行 source_id = %v, want %s", i, meta["source_id"], wantSourceID)
		}
	}
	if heartbeats == 0 {
		t.Error("分块完成与每批次后应有心跳回调")
	}
	// 成功后在数据库锁内 activate（先删旧正式向量再 promote + 快照）。
	if !containsEvent(f.rec.list(), "activate-job") {
		t.Fatalf("应调用 activateVectorJob, events = %v", f.rec.list())
	}
	kb := f.store.snapshot(7)
	if kb.ChunkCount == nil || *kb.ChunkCount != len(rows) {
		t.Errorf("chunkCount = %v, want %d", kb.ChunkCount, len(rows))
	}
	if kb.VectorConfig == nil || *kb.VectorConfig != configJSON {
		t.Errorf("vectorConfig 快照不符: %v", kb.VectorConfig)
	}
}

// TestVectorize哈希缺省取内容MD5 Java fileHash 兜底语义。
func TestVectorize哈希缺省取内容MD5(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(3)
	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 20
	cfg.MinChunkSizeChars = 5
	svc := newVectorizeService(f, cfg, 10)

	content := "短文本切片。再来一句。"
	if err := svc.VectorizeAndStore(context.Background(), 3, content, "attempt-1", func() error { return nil }, SourceInfo{}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	rows := f.vectors.pendingRows
	if len(rows) == 0 {
		t.Fatal("应写入向量")
	}
	configJSON, _ := cfg.ConfigJSON()
	want := md5Hex([]byte(md5Hex([]byte(content)) + "\n" + configJSON + "\n0"))
	got := rows[0].metadata["source_id"]
	if got != want {
		t.Errorf("source_id = %v, want %s（内容 md5 兜底）", got, want)
	}
}

// TestVectorize分批写入batch10 25 块 → 3 批（10/10/5）。
func TestVectorize分批写入batch10(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(9)
	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 8
	cfg.MinChunkSizeChars = 1
	cfg.MinChunkLengthToEmbed = 1
	svc := newVectorizeService(f, cfg, 10)

	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("句子")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("。")
	}
	if err := svc.VectorizeAndStore(context.Background(), 9, sb.String(), "attempt-1", func() error { return nil }, SourceInfo{}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	kb := f.store.snapshot(9)
	total := *kb.ChunkCount
	if total <= 10 {
		t.Fatalf("用例应产出多于 10 块, got %d", total)
	}
	if total != f.vectors.rowCount() {
		t.Errorf("chunkCount = %d, 写入行 = %d", total, f.vectors.rowCount())
	}
	if got := f.vectors.batchCount(); got != (total+9)/10 {
		t.Errorf("批次 = %d, want %d（向上取整）", got, (total+9)/10)
	}
	for _, n := range f.vectors.batchSizes {
		if n > 10 {
			t.Errorf("单批 %d 超过 DashScope 上限 10", n)
		}
	}
}

// TestVectorizeEmbedding失败清理临时向量 失败按 jobId 清理并返回 6006。
func TestVectorizeEmbedding失败清理临时向量(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(5)
	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 10
	svc := newVectorizeService(f, cfg, 10)
	f.vectors.addErr = errors.New("embedding down")

	err := svc.VectorizeAndStore(context.Background(), 5, "内容切片。再来。", "attempt-1", func() error { return nil }, SourceInfo{})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != apperr.CodeKnowledgeBaseVectorizationFailed || ae.Message != "向量化知识库失败" {
		t.Fatalf("错误 = %v, want [6006] 向量化知识库失败", err)
	}
	events := f.rec.list()
	if !containsEvent(events, "delete-by-job") {
		t.Errorf("应按 jobId 清理临时向量, events = %v", events)
	}
	if containsEvent(events, "activate-job") {
		t.Errorf("失败路径不应 promote, events = %v", events)
	}
	if len(f.store.vectorRows()) != 0 {
		t.Errorf("临时向量应已清理, rows = %d", len(f.store.vectorRows()))
	}
}

// TestVectorize代次失效清理临时向量 promote 校验失败同样清理并置 6006。
func TestVectorize代次失效清理临时向量(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(5)
	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 10
	svc := newVectorizeService(f, cfg, 10)
	// 模拟 promote 时执行权已被恢复调度器剥夺。
	f.store.activateErr = apperr.New(apperr.CodeKnowledgeBaseVectorizationFailed, "向量化任务执行权已失效")

	err := svc.VectorizeAndStore(context.Background(), 5, "内容切片。再来。", "attempt-stale", func() error { return nil }, SourceInfo{})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != apperr.CodeKnowledgeBaseVectorizationFailed {
		t.Fatalf("错误 = %v, want 6006", err)
	}
	if !containsEvent(f.rec.list(), "delete-by-job") {
		t.Errorf("执行权失效也应清理临时向量, events = %v", f.rec.list())
	}
}

// TestVectorize特殊Token失败 走 6006 失败路径。
func TestVectorize特殊Token失败(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(5)
	svc := newVectorizeService(f, textsplit.DefaultConfig(), 10)
	err := svc.VectorizeAndStore(context.Background(), 5, "包含 <|endoftext|> 的文本", "attempt-1", func() error { return nil }, SourceInfo{})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != apperr.CodeKnowledgeBaseVectorizationFailed || ae.Message != "向量化知识库失败" {
		t.Fatalf("错误 = %v, want [6006] 向量化知识库失败", err)
	}
}

// TestVectorize两阶段promote旧正式向量被替换 promote 语义：同库先删旧正式向量。
func TestVectorize两阶段promote旧正式向量被替换(t *testing.T) {
	f := newKBFixture()
	f.addProcessingKB(6)
	// 预置一份旧正式向量。
	f.store.mu.Lock()
	f.store.vectors = append(f.store.vectors, vectorRow{
		content:  "旧内容",
		metadata: map[string]any{metadataKBID: "6", "source_id": "old"},
	})
	f.store.mu.Unlock()

	cfg := textsplit.DefaultConfig()
	cfg.ChunkSize = 10
	svc := newVectorizeService(f, cfg, 10)
	if err := svc.VectorizeAndStore(context.Background(), 6, "新内容切片。再来一句。", "attempt-1", func() error { return nil }, SourceInfo{}); err != nil {
		t.Fatalf("向量化失败: %v", err)
	}
	rows := f.store.vectorRows()
	if len(rows) == 0 {
		t.Fatal("应有新正式向量")
	}
	for _, row := range rows {
		if row.metadata[metadataKBID] != "6" {
			t.Errorf("promote 后 kb_id = %v, want \"6\"", row.metadata[metadataKBID])
		}
		if _, ok := row.metadata[metadataJobID]; ok {
			t.Errorf("promote 后应删除 kb_vector_job_id: %v", row.metadata)
		}
		if _, ok := row.metadata[metadataTarget]; ok {
			t.Errorf("promote 后应删除 kb_target_id: %v", row.metadata)
		}
		if row.content == "旧内容" {
			t.Error("旧正式向量应被替换")
		}
	}
}

var _ = md5.New
var _ = hex.EncodeToString
