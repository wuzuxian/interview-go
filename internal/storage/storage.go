// Package storage 文件存储：S3 兼容（RustFS/MinIO），与 Java 版 FileStorageService 行为对齐。
package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"

	"github.com/wuzuxian/interview/internal/apperr"
)

// Config 存储配置。
type Config struct {
	Endpoint         string
	AccessKey        string
	SecretKey        string
	Bucket           string
	Region           string
	AutoCreateBucket bool
}

// Service S3 兼容存储服务。
type Service struct {
	client *s3.Client
	cfg    Config
	logger *slog.Logger
}

// New 创建存储服务并按需确保桶存在。
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Service, error) {
	awsCli, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(cfg.Region),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: 初始化 AWS 配置失败: %w", err)
	}
	client := s3.NewFromConfig(awsCli, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
	})
	s := &Service{client: client, cfg: cfg, logger: logger}
	if cfg.AutoCreateBucket {
		if err := s.ensureBucket(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Service) ensureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)})
	if err == nil {
		s.logger.Info("存储桶已存在", slog.String("bucket", s.cfg.Bucket))
		return nil
	}
	var nf *smithy.GenericAPIError
	if ok := asNotFound(err, &nf); ok {
		if _, cerr := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.cfg.Bucket)}); cerr != nil {
			if strings.Contains(cerr.Error(), "BucketAlreadyOwnedByYou") || strings.Contains(cerr.Error(), "409") {
				return nil
			}
			return apperr.Wrap(apperr.CodeStorageUploadFailed, "创建存储桶失败", cerr)
		}
		s.logger.Info("存储桶创建成功", slog.String("bucket", s.cfg.Bucket))
		return nil
	}
	s.logger.Warn("检查存储桶失败", slog.String("bucket", s.cfg.Bucket), slog.String("error", err.Error()))
	return nil
}

func asNotFound(err error, target **smithy.GenericAPIError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*smithy.GenericAPIError); ok {
		*target = e
		return e.Code == "NotFound" || e.Code == "NoSuchBucket"
	}
	return strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchBucket")
}

var unsafeNameChars = regexp.MustCompile(`[^0-9A-Za-z._-]+`)

// sanitizeFilename 清理文件名：非安全字符替换为下划线（Java 版转拼音，此处保底替换）。
func sanitizeFilename(name string) string {
	if name == "" {
		return "unknown"
	}
	safe := unsafeNameChars.ReplaceAllString(path.Base(name), "_")
	if safe == "" || safe == "." {
		return "unknown"
	}
	return safe
}

// generateFileKey 生成 resumes/yyyy/MM/dd/{uuid8}_{safeName} 形式的键。
func generateFileKey(originalFilename, prefix string) string {
	now := time.Now()
	datePath := now.Format("2006/01/02")
	uid := uuid.NewString()[:8]
	return fmt.Sprintf("%s/%s/%s_%s", prefix, datePath, uid, sanitizeFilename(originalFilename))
}

// UploadResume 上传简历文件，返回 fileKey。
func (s *Service) UploadResume(ctx context.Context, fileName string, contentType string, data []byte) (string, error) {
	key := generateFileKey(fileName, "resumes")
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeStorageUploadFailed, "文件存储失败", err)
	}
	s.logger.Info("文件上传成功", slog.String("filename", fileName), slog.String("key", key))
	return key, nil
}

// DeleteResume 删除简历文件；键为空或对象不存在时静默跳过。
func (s *Service) DeleteResume(ctx context.Context, fileKey string) error {
	return s.deleteObject(ctx, fileKey)
}

// UploadKnowledgeBase 上传知识库文件（键前缀 knowledgebases/），返回 fileKey。
func (s *Service) UploadKnowledgeBase(ctx context.Context, fileName string, contentType string, data []byte) (string, error) {
	key := generateFileKey(fileName, "knowledgebases")
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeStorageUploadFailed, "文件存储失败", err)
	}
	s.logger.Info("文件上传成功", slog.String("filename", fileName), slog.String("key", key))
	return key, nil
}

// DeleteKnowledgeBase 删除知识库文件；键为空或对象不存在时静默跳过。
func (s *Service) DeleteKnowledgeBase(ctx context.Context, fileKey string) error {
	return s.deleteObject(ctx, fileKey)
}

// deleteObject 删除对象；键为空或对象不存在时静默跳过。
func (s *Service) deleteObject(ctx context.Context, fileKey string) error {
	if fileKey == "" {
		return nil
	}
	ok, err := s.FileExists(ctx, fileKey)
	if err != nil || !ok {
		s.logger.Warn("文件不存在，跳过删除", slog.String("key", fileKey))
		return nil
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fileKey),
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageDeleteFailed, "文件删除失败", err)
	}
	return nil
}

// FileExists 检查对象是否存在。
func (s *Service) FileExists(ctx context.Context, fileKey string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fileKey),
	})
	if err == nil {
		return true, nil
	}
	var apiErr *smithy.GenericAPIError
	if e, ok := err.(*smithy.GenericAPIError); ok {
		apiErr = e
		return !(apiErr.Code == "NotFound" || apiErr.Code == "NoSuchKey"), nil
	}
	if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
		return false, nil
	}
	return false, nil
}

// Download 下载对象内容。
func (s *Service) Download(ctx context.Context, fileKey string) ([]byte, error) {
	ok, err := s.FileExists(ctx, fileKey)
	if err != nil || !ok {
		return nil, apperr.New(apperr.CodeStorageDownloadFail, "文件不存在: "+fileKey)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fileKey),
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageDownloadFail, "文件下载失败", err)
	}
	defer out.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageDownloadFail, "文件下载失败", err)
	}
	return buf.Bytes(), nil
}

// FileURL 生成对象访问 URL：{endpoint}/{bucket}/{key}，与 Java getFileUrl 一致。
func (s *Service) FileURL(fileKey string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(s.cfg.Endpoint, "/"), s.cfg.Bucket, fileKey)
}
