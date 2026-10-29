// HTTP 契约缝（主缝）：httptest 真路由 + 依赖全 fake，锁上传/重新向量化的
// Result JSON 字段名、6xxx 错误码与限流路由。契约来源：Java
// KnowledgeBaseController（upload 3/3、revectorize 2/2）逐字。
package knowledgebase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// errScripter 恒返回错误的 Redis 脚本端（ratelimit fail-open 放行，限流数值不进 CI）。
type errScripter struct{}

func errCmd() *redis.Cmd {
	cmd := redis.NewCmd(context.Background())
	cmd.SetErr(errors.New("redis down"))
	return cmd
}

func (errScripter) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return errCmd()
}

func (errScripter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return errCmd()
}

func (errScripter) EvalRO(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return errCmd()
}

func (errScripter) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return errCmd()
}

func (errScripter) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(context.Background())
	cmd.SetErr(errors.New("redis down"))
	return cmd
}

func (errScripter) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	cmd.SetErr(errors.New("redis down"))
	return cmd
}

// contractFixture httptest 真服务端。
type contractFixture struct {
	f        *uploadFixture
	engine   *gin.Engine
	recorder *httptest.ResponseRecorder
}

func newContractFixture(t *testing.T) *contractFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &contractFixture{f: newUploadFixture(4), recorder: httptest.NewRecorder()}
	f.engine = gin.New()
	api := f.engine.Group("/api")
	ctl := NewController(f.f.svc,
		NewListService(f.f.store, f.f.storage, discardLogger()),
		NewDeleteService(f.f.store, f.f.storage, discardLogger()),
		nil,
		ratelimit.New(errScripter{}, discardLogger()))
	ctl.RegisterRoutes(api)
	return f
}

// postMultipart 构造 multipart 上传请求。
func (h *contractFixture) postMultipart(t *testing.T, fileName string, data []byte, extra map[string]string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fw, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	for k, v := range extra {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("写字段失败: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebase/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	h.engine.ServeHTTP(h.recorder, req)
}

// resultBody 解析 Result{code,message,data}。
func (h *contractFixture) resultBody(t *testing.T) (int, string, map[string]any) {
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
		Data    map[string]any  `json:"data"`
		Extra   json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 Result 失败: %v, body=%s", err, raw)
	}
	return out.Code, out.Message, out.Data
}

// TestUpload契约成功响应字段名与Java逐字 对应 Java Map.of 键集。
func TestUpload契约成功响应字段名与Java逐字(t *testing.T) {
	h := newContractFixture(t)
	h.postMultipart(t, "面试笔记.md", []byte("# 笔记内容"), map[string]string{"name": "我的笔记", "category": "Java"})

	code, _, data := h.resultBody(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d, want 200", code)
	}
	// 顶层键集：knowledgeBase/storage/enqueueAccepted/message/duplicate。
	for _, key := range []string{"knowledgeBase", "storage", "enqueueAccepted", "message", "duplicate"} {
		if _, ok := data[key]; !ok {
			t.Errorf("data 缺少键 %q, got %v", key, data)
		}
	}
	kb, ok := data["knowledgeBase"].(map[string]any)
	if !ok {
		t.Fatalf("knowledgeBase = %v", data["knowledgeBase"])
	}
	for _, key := range []string{"id", "name", "category", "fileSize", "vectorStatus"} {
		if _, ok := kb[key]; !ok {
			t.Errorf("knowledgeBase 缺少键 %q, got %v", key, kb)
		}
	}
	if kb["vectorStatus"] != "PENDING" || kb["name"] != "我的笔记" || kb["category"] != "Java" {
		t.Errorf("knowledgeBase = %v", kb)
	}
	storage, ok := data["storage"].(map[string]any)
	if !ok {
		t.Fatalf("storage = %v", data["storage"])
	}
	for _, key := range []string{"fileKey", "fileUrl"} {
		if _, ok := storage[key]; !ok {
			t.Errorf("storage 缺少键 %q, got %v", key, storage)
		}
	}
	if data["enqueueAccepted"] != true || data["duplicate"] != false || data["message"] != "" {
		t.Errorf("enqueueAccepted/message/duplicate = %v/%v/%v", data["enqueueAccepted"], data["message"], data["duplicate"])
	}
}

// TestUpload契约重复上传 对应 Java handleDuplicateKnowledgeBase 响应。
func TestUpload契约重复上传(t *testing.T) {
	h := newContractFixture(t)
	data := []byte("# 相同内容")
	h.postMultipart(t, "a.md", data, nil)
	h.recorder = httptest.NewRecorder()
	h.postMultipart(t, "b.md", data, nil)

	code, message, respData := h.resultBody(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d", code)
	}
	if message != "success" {
		t.Fatalf("message = %q", message)
	}
	if respData["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", respData["duplicate"])
	}
	if respData["message"] != "检测到重复文件，已返回现有知识库" {
		t.Errorf("message.data = %v", respData["message"])
	}
}

// TestUpload契约类型不支持400 错误码与文案逐字。
func TestUpload契约类型不支持400(t *testing.T) {
	h := newContractFixture(t)
	h.postMultipart(t, "virus.exe", []byte("MZ binary"), nil)

	code, message, _ := h.resultBody(t)
	if code != 400 {
		t.Fatalf("code = %d, want 400", code)
	}
	if message != "不支持的文件类型: application/vnd.microsoft.portable-executable，支持的类型：PDF、DOCX、DOC、TXT、MD等" {
		t.Errorf("message = %q", message)
	}
}

// TestUpload契约缺文件400 multipart 无 file 字段。
func TestUpload契约缺文件400(t *testing.T) {
	h := newContractFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebase/upload", bytes.NewBufferString("--x--"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	h.engine.ServeHTTP(h.recorder, req)

	code, message, _ := h.resultBody(t)
	if code != 400 || message != "请选择要上传的知识库文件" {
		t.Errorf("code = %d, message = %q", code, message)
	}
}

// TestRevectorize契约NotFound6001 不存在 → 6001 + 逐字文案。
func TestRevectorize契约NotFound6001(t *testing.T) {
	h := newContractFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebase/999/revectorize", nil)
	h.engine.ServeHTTP(h.recorder, req)

	code, message, _ := h.resultBody(t)
	if code != 6001 {
		t.Fatalf("code = %d, want 6001", code)
	}
	if message != "知识库不存在" {
		t.Errorf("message = %q", message)
	}
}

// TestRevectorize契约成功 成功返回 code=200。
func TestRevectorize契约成功(t *testing.T) {
	h := newContractFixture(t)
	// 直接落一行 PENDING 实体。
	kb := sampleKB(0)
	id := h.f.store.addKB(kb)

	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/revectorize", nil)
	h.engine.ServeHTTP(h.recorder, req)

	code, message, _ := h.resultBody(t)
	if code != result.CodeSuccess {
		t.Fatalf("code = %d, message = %q", code, message)
	}
}

// TestRevectorize契约投递失败500 投递失败如实返回。
func TestRevectorize契约投递失败500(t *testing.T) {
	h := newContractFixture(t)
	id := h.f.store.addKB(sampleKB(0))
	h.f.stream.addErr = errors.New("redis down")

	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebase/"+itoa(id)+"/revectorize", nil)
	h.engine.ServeHTTP(h.recorder, req)

	code, message, _ := h.resultBody(t)
	if code != 500 || message != "任务投递失败，请稍后重试" {
		t.Errorf("code = %d, message = %q", code, message)
	}
}

// TestUploadDTOContract 响应 DTO JSON 键集与 Java Map 键名逐字（黄金清单）。
func TestUploadDTOContract(t *testing.T) {
	assertKeys(t, jsonKeys(t, UploadResponse{}), sortedKeys(
		"knowledgeBase", "storage", "enqueueAccepted", "message", "duplicate",
	))
	assertKeys(t, jsonKeys(t, UploadKnowledgeBaseDTO{}), sortedKeys(
		"id", "name", "category", "fileSize", "vectorStatus",
	))
	assertKeys(t, jsonKeys(t, UploadStorageDTO{}), sortedKeys(
		"fileKey", "fileUrl",
	))
}

// assertKeys 断言 JSON 键集逐名相等。
func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if !equalStrings(got, want) {
		t.Errorf("JSON 键集与契约不一致\n got: %v\nwant: %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// jsonKeys JSON 对象键集（先例 internal/interview/contract_test.go）。
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return sortedKeys(keys...)
}
