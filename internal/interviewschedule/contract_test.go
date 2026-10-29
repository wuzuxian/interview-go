// HTTP 契约测试（主缝）：httptest 真路由 + 内存 fake Store，锁 6 个 CRUD 接口的
// 路径与方法（含状态接口 PATCH+PUT 双 method）、JSON 字段名、9001 错误码、
// 时间无时区序列化格式、校验 400 文案与列表三选一分支语义。
package interviewschedule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var testNow = time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

func newScheduleRouter(store Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	NewController(NewService(store, logger), NewParseService(logger)).RegisterRoutes(api)
	return r
}

// doJSON 发请求并解析 Result 形状响应。
func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("构造请求体失败: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v; body=%s", err, w.Body.String())
	}
	return w.Code, out
}

// seedRow 直接预置一行日程（绕过创建接口，供查询/更新/删除/状态用例使用）。
func seedRow(t *testing.T, f *fakeStore, id int64, company, status string, at time.Time) {
	t.Helper()
	companyName, position := company, "Java工程师"
	it := NaiveTime(at)
	typ := "VIDEO"
	round := 2
	f.rows[id] = &InterviewScheduleEntity{
		ID: id, CompanyName: companyName, Position: position,
		InterviewTime: it.Time(), InterviewType: &typ, RoundNumber: &round,
		Status: InterviewStatus(status),
	}
	f.order = append(f.order, id)
	_ = companyName
}

func TestCreate强制PENDING与字段契约(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	code, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule", map[string]any{
		"companyName":   "阿里巴巴",
		"position":      "Java工程师",
		"interviewTime": "2026-04-10T14:00",
		// 客户端试图指定状态/轮次：状态必须被忽略，轮次透传
		"status":      "COMPLETED",
		"roundNumber": 3,
		"meetingLink": "https://meeting.feishu.cn/abc",
	})
	if code != http.StatusOK {
		t.Fatalf("HTTP 状态码 = %d, want 200", code)
	}
	if out["code"].(float64) != 200 || out["message"] != "success" {
		t.Fatalf("code/message = %v/%v", out["code"], out["message"])
	}
	data := out["data"].(map[string]any)
	if data["status"] != "PENDING" {
		t.Fatalf("status = %v, want PENDING（创建强制 PENDING）", data["status"])
	}
	if data["companyName"] != "阿里巴巴" || data["position"] != "Java工程师" {
		t.Fatalf("companyName/position = %v/%v", data["companyName"], data["position"])
	}
	if data["roundNumber"].(float64) != 3 {
		t.Fatalf("roundNumber = %v, want 3", data["roundNumber"])
	}
	if data["id"].(float64) != 1 {
		t.Fatalf("id = %v, want 1（identity 从 1 起）", data["id"])
	}
	// 16 位入参归一为 19 位无时区输出（对齐 @JsonFormat yyyy-MM-dd'T'HH:mm:ss）
	if data["interviewTime"] != "2026-04-10T14:00:00" {
		t.Fatalf("interviewTime = %v, want 2026-04-10T14:00:00", data["interviewTime"])
	}
	if data["createdAt"] != "2026-04-01T10:00:00" || data["updatedAt"] != "2026-04-01T10:00:00" {
		t.Fatalf("createdAt/updatedAt = %v/%v（应为无时区格式）", data["createdAt"], data["updatedAt"])
	}
	// 12 个字段全部在响应中（含 null 的可选字段，Java DTO 全字段序列化）
	for _, k := range []string{"id", "companyName", "position", "interviewTime", "interviewType",
		"meetingLink", "roundNumber", "interviewer", "notes", "status", "createdAt", "updatedAt"} {
		if _, ok := data[k]; !ok {
			t.Fatalf("响应缺少字段 %s", k)
		}
	}
}

func TestCreate轮次缺省为1(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	_, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule", map[string]any{
		"companyName": "字节跳动", "position": "后端", "interviewTime": "2026-04-10T09:30:00",
	})
	if got := out["data"].(map[string]any)["roundNumber"]; got != float64(1) {
		t.Fatalf("roundNumber = %v, want 1（缺省回落）", got)
	}
}

func TestCreate校验失败逗号拼接(t *testing.T) {
	r := newScheduleRouter(newFakeStore())
	_, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule", map[string]any{
		"companyName": "  ", "position": "", "interviewTime": "2026-04-10T14:00",
	})
	if out["code"].(float64) != 400 {
		t.Fatalf("code = %v, want 400", out["code"])
	}
	want := "公司名称不能为空, 岗位不能为空"
	if out["message"] != want {
		t.Fatalf("message = %q, want %q（@NotBlank 文案 + \", \" 连接）", out["message"], want)
	}
}

func TestCreate非法时间格式与非法JSON(t *testing.T) {
	r := newScheduleRouter(newFakeStore())
	_, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule", map[string]any{
		"companyName": "A", "position": "B", "interviewTime": "2026/04/10 14:00",
	})
	if out["code"].(float64) != 400 || out["message"] != "请求参数错误" {
		t.Fatalf("非法时间格式 → %v/%v, want 400/请求参数错误", out["code"], out["message"])
	}
	req := httptest.NewRequest(http.MethodPost, "/api/interview-schedule", bytes.NewBufferString("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out2 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out2)
	if out2["code"].(float64) != 400 || out2["message"] != "请求参数错误" {
		t.Fatalf("非法 JSON → %v/%v, want 400/请求参数错误", out2["code"], out2["message"])
	}
}

func TestGetById契约(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	seedRow(t, store, 5, "美团", "PENDING", time.Date(2026, 4, 12, 15, 30, 0, 0, time.UTC))
	code, out := doJSON(t, r, http.MethodGet, "/api/interview-schedule/5", nil)
	if code != http.StatusOK || out["code"].(float64) != 200 {
		t.Fatalf("code = %d/%v", code, out["code"])
	}
	data := out["data"].(map[string]any)
	if data["id"] != float64(5) || data["status"] != "PENDING" || data["interviewType"] != "VIDEO" {
		t.Fatalf("data = %v", data)
	}
	if data["interviewTime"] != "2026-04-12T15:30:00" {
		t.Fatalf("interviewTime = %v（无时区格式）", data["interviewTime"])
	}
}

func TestGetById不存在9001(t *testing.T) {
	r := newScheduleRouter(newFakeStore())
	_, out := doJSON(t, r, http.MethodGet, "/api/interview-schedule/9", nil)
	if out["code"].(float64) != 9001 {
		t.Fatalf("code = %v, want 9001", out["code"])
	}
	if out["message"] != "面试日程不存在: 9" {
		t.Fatalf("message = %q", out["message"])
	}
}

func Test路径ID非法(t *testing.T) {
	r := newScheduleRouter(newFakeStore())
	for _, path := range []string{"/api/interview-schedule/abc", "/api/interview-schedule/-1",
		"/api/interview-schedule/99999999999999999999"} {
		_, out := doJSON(t, r, http.MethodGet, path, nil)
		if out["code"].(float64) != 400 || out["message"] != "无效的ID参数" {
			t.Fatalf("%s → %v/%v, want 400/无效的ID参数", path, out["code"], out["message"])
		}
	}
}

func TestList三选一分支(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	seedRow(t, store, 1, "A", "PENDING", time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC))
	seedRow(t, store, 2, "B", "COMPLETED", time.Date(2026, 4, 11, 10, 0, 0, 0, time.UTC))
	seedRow(t, store, 3, "C", "PENDING", time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC))

	// 无参 → 全量
	_, out := doJSON(t, r, http.MethodGet, "/api/interview-schedule", nil)
	if got := len(out["data"].([]any)); got != 3 {
		t.Fatalf("全量 = %d, want 3", got)
	}

	// 仅 status → 按状态
	_, out = doJSON(t, r, http.MethodGet, "/api/interview-schedule?status=PENDING", nil)
	if got := len(out["data"].([]any)); got != 2 {
		t.Fatalf("按状态 = %d, want 2", got)
	}

	// 非法 status → 400
	_, out = doJSON(t, r, http.MethodGet, "/api/interview-schedule?status=FOO", nil)
	if out["code"].(float64) != 400 || out["message"] != "无效的面试状态: FOO" {
		t.Fatalf("非法状态 → %v/%v", out["code"], out["message"])
	}

	// start+end 同时存在 → 范围优先、双端闭区间，status 被忽略
	_, out = doJSON(t, r, http.MethodGet,
		"/api/interview-schedule?status=COMPLETED&start=2026-04-10T10:00:00&end=2026-04-11T10:00:00", nil)
	got := out["data"].([]any)
	if len(got) != 2 {
		t.Fatalf("范围 = %d, want 2（双端闭区间，status 被忽略）", len(got))
	}

	// 仅 start（无 end）→ 范围不生效，且无 status → 全量
	_, out = doJSON(t, r, http.MethodGet, "/api/interview-schedule?start=2026-04-10T10:00:00", nil)
	if got := len(out["data"].([]any)); got != 3 {
		t.Fatalf("仅 start = %d, want 3", got)
	}

	// 非法时间参数 → 400
	_, out = doJSON(t, r, http.MethodGet, "/api/interview-schedule?start=not-a-time", nil)
	if out["code"].(float64) != 400 {
		t.Fatalf("非法时间 → %v, want 400", out["code"])
	}
}

func TestUpdate拷贝业务字段status不变(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	seedRow(t, store, 5, "美团", "COMPLETED", time.Date(2026, 4, 12, 15, 30, 0, 0, time.UTC))
	_, out := doJSON(t, r, http.MethodPut, "/api/interview-schedule/5", map[string]any{
		"companyName": "美团优选", "position": "Go工程师", "interviewTime": "2026-04-13T10:00",
		"interviewType": "PHONE", "roundNumber": 1,
	})
	if out["code"].(float64) != 200 {
		t.Fatalf("code = %v", out["code"])
	}
	data := out["data"].(map[string]any)
	if data["companyName"] != "美团优选" || data["position"] != "Go工程师" ||
		data["interviewType"] != "PHONE" || data["interviewTime"] != "2026-04-13T10:00:00" {
		t.Fatalf("业务字段未拷贝: %v", data)
	}
	if data["status"] != "COMPLETED" {
		t.Fatalf("status = %v, want COMPLETED（更新不得改状态）", data["status"])
	}
	// 未传的可选字段被覆盖为 null（BeanUtils.copyProperties 全字段拷贝语义）
	if _, has := data["meetingLink"]; !has {
		t.Fatal("meetingLink 字段应存在（null）")
	}

	// 不存在 → 9001
	_, out = doJSON(t, r, http.MethodPut, "/api/interview-schedule/99", map[string]any{
		"companyName": "X", "position": "Y", "interviewTime": "2026-04-13T10:00",
	})
	if out["code"].(float64) != 9001 {
		t.Fatalf("更新不存在 → %v, want 9001", out["code"])
	}
}

func TestDelete不校验存在性(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	seedRow(t, store, 5, "美团", "PENDING", testNow)
	_, out := doJSON(t, r, http.MethodDelete, "/api/interview-schedule/5", nil)
	if out["code"].(float64) != 200 {
		t.Fatalf("删除存在行 → %v", out["code"])
	}
	if v, has := out["data"]; has && v != nil {
		t.Fatalf("data = %v, want null", v)
	}
	if len(store.rows) != 0 {
		t.Fatalf("行未删除: %v", store.rows)
	}
	// 删除不存在的 id 同样返回成功（Java deleteById 语义）
	_, out = doJSON(t, r, http.MethodDelete, "/api/interview-schedule/42", nil)
	if out["code"].(float64) != 200 {
		t.Fatalf("删除不存在行 → %v, want 200", out["code"])
	}
}

func TestUpdateStatus双Method与无流转校验(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)
	seedRow(t, store, 5, "美团", "PENDING", testNow)

	// PATCH：PENDING → COMPLETED（任意跳转）
	code, out := doJSON(t, r, http.MethodPatch, "/api/interview-schedule/5/status?status=COMPLETED", nil)
	if code != http.StatusOK || out["code"].(float64) != 200 {
		t.Fatalf("PATCH → %d/%v", code, out["code"])
	}
	if out["data"].(map[string]any)["status"] != "COMPLETED" {
		t.Fatalf("status = %v", out["data"].(map[string]any)["status"])
	}

	// PUT：任意跳转 CANCELLED → PENDING（无状态机校验）
	_, out = doJSON(t, r, http.MethodPut, "/api/interview-schedule/5/status?status=PENDING", nil)
	if out["data"].(map[string]any)["status"] != "PENDING" {
		t.Fatalf("PUT 后 status = %v", out["data"].(map[string]any)["status"])
	}

	// 非法取值 → 400
	_, out = doJSON(t, r, http.MethodPatch, "/api/interview-schedule/5/status?status=FOO", nil)
	if out["code"].(float64) != 400 || out["message"] != "无效的面试状态: FOO" {
		t.Fatalf("非法状态 → %v/%v", out["code"], out["message"])
	}

	// 缺参 → 400
	_, out = doJSON(t, r, http.MethodPatch, "/api/interview-schedule/5/status", nil)
	if out["code"].(float64) != 400 || out["message"] != "缺少必填参数: status" {
		t.Fatalf("缺参 → %v/%v", out["code"], out["message"])
	}

	// 不存在 → 9001
	_, out = doJSON(t, r, http.MethodPatch, "/api/interview-schedule/9/status?status=COMPLETED", nil)
	if out["code"].(float64) != 9001 || out["message"] != fmt.Sprintf("面试日程不存在: %d", 9) {
		t.Fatalf("状态更新不存在 → %v/%v", out["code"], out["message"])
	}
}

func TestNaiveTime序列化反序列化(t *testing.T) {
	// 16 位入参 → 19 位出参
	var v NaiveTime
	if err := v.UnmarshalJSON([]byte(`"2026-04-10T14:00"`)); err != nil {
		t.Fatalf("16 位解析失败: %v", err)
	}
	b, _ := v.MarshalJSON()
	if string(b) != `"2026-04-10T14:00:00"` {
		t.Fatalf("序列化 = %s", b)
	}
	// null → 零值（校验层兜「不能为空」）
	var z NaiveTime
	if err := z.UnmarshalJSON([]byte(`null`)); err != nil || !z.IsZero() {
		t.Fatalf("null → %v/%v", z, err)
	}
}
