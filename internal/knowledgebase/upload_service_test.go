// 上传服务与重新向量化接口的外部行为单测（夹具：fakeStore/fakeStorage/fakeStream），
// 契约来源：Java KnowledgeBaseUploadService.doUploadKnowledgeBase / revectorize、
// FileValidationService、KnowledgeBaseUploadLimiter 逐字。
package knowledgebase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuzuxian/interview/internal/apperr"
)

// uploadFixture 上传链路夹具。
type uploadFixture struct {
	*kbFixture
	producer *VectorizeProducer
	svc      *UploadService
}

func newUploadFixture(maxConcurrent int) *uploadFixture {
	f := &uploadFixture{kbFixture: newKBFixture()}
	f.producer = NewVectorizeProducer(f.stream, f.store, discardLogger())
	f.svc = NewUploadService(f.store, f.storage, f.producer, 50*1024*1024, maxConcurrent, discardLogger())
	return f
}

// TestUpload成功返回PENDING并投递任务 上传响应契约与落库/投递动作。
func TestUpload成功返回PENDING并投递任务(t *testing.T) {
	f := newUploadFixture(4)
	resp, err := f.svc.Upload(context.Background(), "笔记.md", "text/markdown", []byte("# 内容"), "我的笔记", " Java ")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if resp.Duplicate {
		t.Error("新上传 duplicate 应为 false")
	}
	if !resp.EnqueueAccepted || resp.Message != "" {
		t.Errorf("enqueueAccepted = %v, message = %q", resp.EnqueueAccepted, resp.Message)
	}
	if resp.KnowledgeBase.ID == 0 || resp.KnowledgeBase.Name != "我的笔记" {
		t.Errorf("knowledgeBase = %+v", resp.KnowledgeBase)
	}
	// category trim 后落库；DTO 空 category 输出 ""（Java category != null ? : ""）。
	if resp.KnowledgeBase.Category != "Java" {
		t.Errorf("category = %q, want Java", resp.KnowledgeBase.Category)
	}
	if resp.KnowledgeBase.VectorStatus != "PENDING" {
		t.Errorf("vectorStatus = %q, want PENDING", resp.KnowledgeBase.VectorStatus)
	}
	if resp.KnowledgeBase.FileSize != 8 {
		t.Errorf("fileSize = %d, want 8", resp.KnowledgeBase.FileSize)
	}
	if resp.Storage.FileKey == "" || !strings.HasPrefix(resp.Storage.FileURL, "http://localhost:9000/bucket/knowledgebases/") {
		t.Errorf("storage = %+v", resp.Storage)
	}
	// 落库状态 PENDING 且进展时间已初始化（否则 stale 扫描漏判）。
	kb := f.store.snapshot(resp.KnowledgeBase.ID)
	if kb.VectorStatus != VectorPending || kb.VectorUpdatedAt == nil {
		t.Errorf("落库状态 = %v, vectorUpdatedAt = %v", kb.VectorStatus, kb.VectorUpdatedAt)
	}
	if kb.Category == nil || *kb.Category != "Java" {
		t.Errorf("category 落库 = %v", kb.Category)
	}
	// 投递消息只含 kbId + retryCount=0（Java buildMessage）。
	if f.stream.addCount() != 1 {
		t.Fatalf("应投递 1 条消息, got %d", f.stream.addCount())
	}
	added := f.stream.addAt(0)
	if added[FieldKBID] != itoa(resp.KnowledgeBase.ID) || added[FieldRetryCount] != "0" {
		t.Errorf("投递消息 = %v", added)
	}
	if !strings.HasSuffix(added[FieldKBID].(string), "") && len(f.storage.uploaded) != 1 {
		t.Errorf("应上传 1 个对象, got %v", f.storage.uploaded)
	}
}

// TestUpload名称缺省从文件名提取 与未分类落库。
func TestUpload名称缺省从文件名提取(t *testing.T) {
	f := newUploadFixture(4)
	resp, err := f.svc.Upload(context.Background(), "架构漫谈.md", "text/markdown", []byte("正文"), "", "")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if resp.KnowledgeBase.Name != "架构漫谈" {
		t.Errorf("name = %q, want 架构漫谈", resp.KnowledgeBase.Name)
	}
	if resp.KnowledgeBase.Category != "" {
		t.Errorf("category = %q, want 空串（未分类）", resp.KnowledgeBase.Category)
	}
	kb := f.store.snapshot(resp.KnowledgeBase.ID)
	if kb.Category != nil {
		t.Errorf("未分类应落 NULL, got %v", *kb.Category)
	}
}

// TestUploadMIME白名单与扩展名兜底 表驱动：白名单逐项放行、非法类型拒绝、md 兜底。
func TestUploadMIME白名单与扩展名兜底(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		ct      string
		data    []byte
		wantErr bool
	}{
		{"PDF放行", "a.pdf", "application/pdf", []byte("%PDF-1.4"), false},
		{"DOC放行", "a.doc", "application/msword", []byte("word"), false},
		{"DOCX放行", "a.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("PK"), false},
		{"TXT放行", "a.txt", "text/plain", []byte("文本"), false},
		{"MARKDOWN放行", "a.md", "text/markdown", []byte("# 标题"), false},
		{"X-MARKDOWN放行", "a.md", "text/x-markdown", []byte("# 标题"), false},
		{"RTF放行", "a.rtf", "application/rtf", []byte(`{\rtf1 ok}`), false},
		{"内容嗅探RTF归一为application", "a.rtf", "ignored", []byte(`{\rtf1 ok}`), false},
		{"内容嗅探PDF放行", "a.bin", "ignored", []byte("%PDF-1.4 fake"), false},
		{"MD扩展名兜底OCTET-STREAM", "a.md", "application/octet-stream", []byte("# 标题"), false},
		{"MARKDOWN扩展名兜底", "a.markdown", "application/octet-stream", []byte("# 标题"), false},
		{"MDOWN扩展名兜底", "a.mdown", "application/octet-stream", []byte("# 标题"), false},
		{"非法类型ZIP拒绝", "a.zip", "application/zip", []byte("PK\x03\x04"), true},
		{"非法类型EXE拒绝", "a.exe", "application/vnd.microsoft.portable-executable", []byte("MZ"), true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newUploadFixture(4)
			resp, err := f.svc.Upload(context.Background(), c.file, c.ct, c.data, "", "")
			if c.wantErr {
				var ae *apperr.AppError
				if !errors.As(err, &ae) || ae.Code != 400 {
					t.Fatalf("应返回 400, got %v", err)
				}
				if !strings.HasPrefix(ae.Message, "不支持的文件类型: ") || !strings.HasSuffix(ae.Message, "，支持的类型：PDF、DOCX、DOC、TXT、MD等") {
					t.Errorf("消息 = %q", ae.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("应放行, got %v", err)
			}
			if resp == nil || resp.KnowledgeBase.ID == 0 {
				t.Fatal("应成功返回")
			}
		})
	}
}

// TestUpload空文件与超大文件 基本校验（Java validateFile 逐字）。
func TestUpload空文件与超大文件(t *testing.T) {
	f := newUploadFixture(4)
	_, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", nil, "", "")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 400 || ae.Message != "请选择要上传的知识库文件" {
		t.Errorf("空文件错误 = %v", err)
	}
	f2 := newUploadFixture(4)
	f2.svc.maxFileSize = 4
	_, err = f2.svc.Upload(context.Background(), "a.md", "text/markdown", []byte("12345"), "", "")
	if !errors.As(err, &ae) || ae.Code != 400 || ae.Message != "文件大小超过限制" {
		t.Errorf("超大文件错误 = %v", err)
	}
}

// TestUpload重复上传返回既有条目不重复投递 SHA-256 去重契约。
func TestUpload重复上传返回既有条目不重复投递(t *testing.T) {
	f := newUploadFixture(4)
	data := []byte("# 完全相同的内容")
	first, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", data, "第一份", "")
	if err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	f.stream.adds = nil
	f.rec.add("__clear__")

	second, err := f.svc.Upload(context.Background(), "b.md", "text/markdown", data, "第二份", "")
	if err != nil {
		t.Fatalf("重复上传失败: %v", err)
	}
	if !second.Duplicate || second.Message != "检测到重复文件，已返回现有知识库" {
		t.Errorf("duplicate 响应 = %+v, message = %q", second.Duplicate, second.Message)
	}
	if second.KnowledgeBase.ID != first.KnowledgeBase.ID {
		t.Errorf("应返回既有条目 %d, got %d", first.KnowledgeBase.ID, second.KnowledgeBase.ID)
	}
	if second.Storage.FileKey != first.Storage.FileKey {
		t.Errorf("storage 应来自既有条目: %v", second.Storage)
	}
	if f.stream.addCount() != 0 {
		t.Errorf("重复上传不应投递向量化任务, got %d", f.stream.addCount())
	}
	// 访问计数 +1（Java incrementAccessCount）。
	kb := f.store.snapshot(first.KnowledgeBase.ID)
	if kb.AccessCount == nil || *kb.AccessCount != 2 {
		t.Errorf("accessCount = %v, want 2", kb.AccessCount)
	}
}

// TestUpload落库失败补偿删除S3孤儿对象 补偿路径。
func TestUpload落库失败补偿删除S3孤儿对象(t *testing.T) {
	f := newUploadFixture(4)
	f.store.insertErr = errors.New("db down")
	_, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", []byte("内容"), "", "")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "保存知识库失败" {
		t.Fatalf("落库失败错误 = %v", err)
	}
	if len(f.storage.deleted) != 1 {
		t.Fatalf("应补偿删除 1 个孤儿对象, got %v", f.storage.deleted)
	}
	if f.storage.has(f.storage.deleted[0]) {
		t.Error("孤儿对象应已被删除")
	}
}

// TestUpload投递失败如实返回FAILED 投递失败契约（不伪装 PENDING）。
func TestUpload投递失败如实返回FAILED(t *testing.T) {
	f := newUploadFixture(4)
	f.stream.addErr = errors.New("redis down")
	resp, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", []byte("内容"), "", "")
	if err != nil {
		t.Fatalf("上传不应整体失败: %v", err)
	}
	if resp.EnqueueAccepted {
		t.Error("enqueueAccepted 应为 false")
	}
	if resp.KnowledgeBase.VectorStatus != "FAILED" {
		t.Errorf("vectorStatus = %q, want FAILED", resp.KnowledgeBase.VectorStatus)
	}
	if resp.Message != "文件已保存，但任务投递失败，可在管理页重试" {
		t.Errorf("message = %q", resp.Message)
	}
	// Java onSendFailed：PENDING → FAILED（"任务入队失败"）。
	kb := f.store.snapshot(resp.KnowledgeBase.ID)
	if kb.VectorStatus != VectorFailed {
		t.Errorf("落库状态 = %v, want FAILED", kb.VectorStatus)
	}
	if kb.VectorError == nil || *kb.VectorError != "任务入队失败" {
		t.Errorf("vectorError = %v", kb.VectorError)
	}
}

// TestUpload并发闸门不排队直接拒绝 Semaphore(4) 语义。
func TestUpload并发闸门不排队直接拒绝(t *testing.T) {
	f := newUploadFixture(1)
	// 占满唯一许可（直接注入，绕过 HTTP 层）。
	f.svc.gate <- struct{}{}
	_, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", []byte("内容"), "", "")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != apperr.CodeRateLimitExceeded || ae.Message != "知识库上传繁忙，请稍后重试" {
		t.Fatalf("闸门满错误 = %v", err)
	}
	// 释放后可再次上传。
	<-f.svc.gate
	if _, err := f.svc.Upload(context.Background(), "a.md", "text/markdown", []byte("内容"), "", ""); err != nil {
		t.Errorf("释放后上传失败: %v", err)
	}
}

// TestRevectorize重新向量化 手动重试三分支 + 投递失败感知。
func TestRevectorize重新向量化(t *testing.T) {
	t.Run("知识库不存在返回6001", func(t *testing.T) {
		f := newUploadFixture(4)
		err := f.svc.Revectorize(context.Background(), 999)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != apperr.CodeKnowledgeBaseNotFound || ae.Message != "知识库不存在" {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("缺少存储信息拒绝", func(t *testing.T) {
		f := newUploadFixture(4)
		kb := sampleKB(0)
		kb.StorageKey = nil
		id := f.store.addKB(kb)
		err := f.svc.Revectorize(context.Background(), id)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "知识库缺少存储信息，无法重新向量化" {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("成功置PENDING清零计数并投递", func(t *testing.T) {
		f := newUploadFixture(4)
		kb := sampleKB(0)
		kb.VectorStatus = VectorFailed
		kb.VectorError = stringPtr("上次的错误")
		kb.VectorRecoveryCount = 3
		id := f.store.addKB(kb)
		if err := f.svc.Revectorize(context.Background(), id); err != nil {
			t.Fatalf("revectorize 失败: %v", err)
		}
		got := f.store.snapshot(id)
		if got.VectorStatus != VectorPending || got.VectorError != nil || got.VectorAttemptID != nil {
			t.Errorf("状态 = %v, error = %v, attempt = %v", got.VectorStatus, got.VectorError, got.VectorAttemptID)
		}
		if got.VectorRecoveryCount != 0 {
			t.Errorf("恢复计数应清零, got %d", got.VectorRecoveryCount)
		}
		if f.stream.addCount() != 1 || f.stream.addAt(0)[FieldRetryCount] != "0" {
			t.Errorf("应投递 1 条 retryCount=0 的消息, got %v", f.stream.adds)
		}
		// 顺序契约：先置 PENDING 清零计数，再投递。
		events := f.rec.list()
		assertEventOrder(t, events, "update-status-pending", "reset-recovery-count")
		assertEventOrder(t, events, "reset-recovery-count", "xadd")
	})

	t.Run("投递失败返回错误", func(t *testing.T) {
		f := newUploadFixture(4)
		id := f.store.addKB(sampleKB(0))
		f.stream.addErr = errors.New("redis down")
		err := f.svc.Revectorize(context.Background(), id)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "任务投递失败，请稍后重试" {
			t.Fatalf("错误 = %v", err)
		}
	})
}
