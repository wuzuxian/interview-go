// 知识库文档解析服务：从对象存储下载原文件并提取文本。
// 对应 Java KnowledgeBaseParseService.downloadAndParseContent + DocumentParseService
// 的超时与文本上限语义：解析在独立 goroutine 执行（Java 专用线程池 + future.get(2min)），
// 超时/解析失败/文本超 5MB 均以 Java 同文案错误收尾；解析后按 Java
// TextCleaningService.cleanText 清洗。
package knowledgebase

import (
	"context"
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/docparse"
)

// ObjectStorage 对象存储接缝（生产实现 *storage.Service，测试注入 fake）。
type ObjectStorage interface {
	UploadKnowledgeBase(ctx context.Context, fileName, contentType string, data []byte) (string, error)
	DeleteKnowledgeBase(ctx context.Context, fileKey string) error
	Download(ctx context.Context, fileKey string) ([]byte, error)
	FileURL(fileKey string) string
}

// ParseService 知识库解析服务。
type ParseService struct {
	storage ObjectStorage
	timeout time.Duration
	// maxTextBytes 提取文本上限（Java BodyContentHandler 5MB 写上限）。
	maxTextBytes int
	logger       *slog.Logger
}

// NewParseService 创建解析服务。
func NewParseService(storage ObjectStorage, timeout time.Duration, maxTextBytes int, logger *slog.Logger) *ParseService {
	return &ParseService{storage: storage, timeout: timeout, maxTextBytes: maxTextBytes, logger: logger}
}

// DownloadAndParseContent 下载原文件并解析出清洗后的文本
// （Java downloadAndParseContent：空文件 → "下载文件失败"；解析超时 →
// "文件解析超时，请检查文件内容"；失败 → "文件解析失败"）。
func (s *ParseService) DownloadAndParseContent(ctx context.Context, storageKey, originalFilename string) (string, error) {
	data, err := s.storage.Download(ctx, storageKey)
	if err != nil {
		return "", apperr.New(apperr.CodeInternalError, "下载文件失败")
	}
	if len(data) == 0 {
		return "", apperr.New(apperr.CodeInternalError, "下载文件失败")
	}
	return s.parseContent(data, originalFilename)
}

// parseContent 单文件解析：独立 goroutine + 超时（Java parseWithTimeout 2 分钟），
// 文本超上限按 Java 写上限溢出语义报解析失败，最后清洗。
func (s *ParseService) parseContent(data []byte, fileName string) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, _, err := docparse.ExtractText(data, fileName)
		done <- result{text: text, err: err}
	}()
	var extracted string
	select {
	case r := <-done:
		if r.err != nil {
			s.logger.Warn("文件解析失败", slog.String("filename", fileName), slog.String("error", r.err.Error()))
			return "", apperr.New(apperr.CodeInternalError, "文件解析失败")
		}
		extracted = r.text
	case <-time.After(s.timeout):
		s.logger.Warn("文件解析超时", slog.String("filename", fileName), slog.Duration("timeoutMs", s.timeout))
		return "", apperr.New(apperr.CodeInternalError, "文件解析超时，请检查文件内容")
	}
	if len(extracted) > s.maxTextBytes {
		s.logger.Warn("提取文本超过上限，按解析失败处理",
			slog.String("filename", fileName), slog.Int("originLen", len(extracted)))
		return "", apperr.New(apperr.CodeInternalError, "文件解析失败")
	}
	return docparse.CleanTextJava(extracted), nil
}
