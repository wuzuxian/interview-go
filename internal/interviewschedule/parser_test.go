// 规则解析器测试：纯函数缝表驱动（真实样例邀约文本、特征探测、逐个兜底、
// 无效产物回落、多格式时间、中文数字轮次）+ parse 接口契约（路径/校验/响应形态）。
package interviewschedule

import (
	"net/http"
	"testing"
	"time"
)

const feishuSample = `【面试邀约】
公司：阿里巴巴
岗位：Java后端开发工程师
时间：2026-04-10 14:30
会议链接：https://meeting.feishu.cn/abc123
第 二 轮面试`

const tencentSample = `腾讯会议
公司：字节跳动
岗位：后端开发工程师
时间：2026/04/15 10:00
会议号：123456789
密码：1234`

const zoomSample = `Zoom 会议邀请
主题：技术面试
时间：2026-04-20 09:30
链接：https://zoom.us/j/9876543210`

func Test飞书样例解析(t *testing.T) {
	r := parseFeishu(feishuSample)
	if !isValidResult(r) {
		t.Fatalf("飞书样例应有效: %+v", r)
	}
	if r.CompanyName != "阿里巴巴" || r.Position != "Java后端开发工程师" {
		t.Fatalf("公司/岗位 = %q/%q", r.CompanyName, r.Position)
	}
	if got := r.InterviewTime.Time(); got != time.Date(2026, 4, 10, 14, 30, 0, 0, time.UTC) {
		t.Fatalf("时间 = %v", got)
	}
	if r.MeetingLink == nil || *r.MeetingLink != "https://meeting.feishu.cn/abc123" {
		t.Fatalf("链接 = %v", r.MeetingLink)
	}
	if r.RoundNumber == nil || *r.RoundNumber != 2 {
		t.Fatalf("轮次 = %v, want 2（「第 二 轮」带空格）", r.RoundNumber)
	}
	if r.InterviewType == nil || *r.InterviewType != "VIDEO" {
		t.Fatalf("类型 = %v, want VIDEO（规则一律 VIDEO）", r.InterviewType)
	}
}

func Test腾讯样例解析(t *testing.T) {
	r := parseTencent(tencentSample)
	if !isValidResult(r) {
		t.Fatalf("腾讯样例应有效: %+v", r)
	}
	if r.CompanyName != "字节跳动" || r.Position != "后端开发工程师" {
		t.Fatalf("公司/岗位 = %q/%q", r.CompanyName, r.Position)
	}
	if got := r.InterviewTime.Time(); got != time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("时间 = %v（斜杠日期应归一）", got)
	}
	// Java matcher.group() 全匹配现状：前缀重复拼入
	want := "会议号: 会议号：123456789 密码: 密码：1234"
	if r.MeetingLink == nil || *r.MeetingLink != want {
		t.Fatalf("meetingLink = %v, want %q（group() 全匹配逐字）", r.MeetingLink, want)
	}
	// 腾讯规则无轮次提取
	if r.RoundNumber != nil {
		t.Fatalf("轮次 = %v, want nil", r.RoundNumber)
	}
}

func TestZoom样例不提取公司岗位(t *testing.T) {
	r := parseZoom(zoomSample)
	// Zoom 规则只提取链接与时间（Java 现状），公司/岗位缺失 → 产物无效
	if isValidResult(r) {
		t.Fatalf("Zoom 产物应无效（无公司岗位）: %+v", r)
	}
	if r.MeetingLink == nil || *r.MeetingLink != "https://zoom.us/j/9876543210" {
		t.Fatalf("链接 = %v", r.MeetingLink)
	}
	if got := r.InterviewTime.Time(); got != time.Date(2026, 4, 20, 9, 30, 0, 0, time.UTC) {
		t.Fatalf("时间 = %v", got)
	}
}

func Test特征探测与逐个兜底(t *testing.T) {
	// 飞书文本 → 特征命中飞书
	if r := tryRuleParsing(feishuSample, ""); !isValidResult(r) {
		t.Fatal("飞书样例应解析成功")
	}
	// 腾讯文本（含「会议号」）→ 特征命中腾讯
	if r := tryRuleParsing(tencentSample, ""); !isValidResult(r) {
		t.Fatal("腾讯样例应解析成功")
	}
	// source 显式指定 → 直走对应解析器（忽略大小写）
	if r := tryRuleParsing(tencentSample, "TENCENT"); !isValidResult(r) {
		t.Fatal("source=tencent 应解析成功")
	}
	// source=zoom 直走 zoom：不提取公司岗位 → 产物无效
	if r := tryRuleParsing(feishuSample, "zoom"); isValidResult(r) {
		t.Fatalf("source=zoom 强制 zoom 解析器，飞书文本应无效: %+v", r)
	}
	// source=other → 走探测
	if r := tryRuleParsing(feishuSample, "other"); !isValidResult(r) {
		t.Fatal("source=other 应走特征探测")
	}
	// 纯时间无公司岗位 → 全部兜底后仍无效
	if r := tryRuleParsing("时间：2026-04-10 14:30", ""); isValidResult(r) {
		t.Fatal("缺公司岗位应无效")
	}
}

func TestParseDateTime多格式(t *testing.T) {
	cases := []struct {
		in   string
		want *time.Time
	}{
		{"2026-04-10 14:30", tp(2026, 4, 10, 14, 30)},
		{"2026/04/10 14:30", tp(2026, 4, 10, 14, 30)},
		{"2026-04-10 14:30:45", tp(2026, 4, 10, 14, 30, 45)},
		// 16/19 位走空格格式分支：T 分隔在该分支直接失败返回 nil（Java 逐字：
		// len==16/19 先按 yyyy-MM-dd HH:mm[:ss] 解析，异常被 catch → null，
		// ISO 兜底对 16/19 位长度不生效）
		{"2026-04-10T14:30:45", nil},
		{"2026-04-10T14:30", nil},
		// 非 16/19 位 → ISO 本地格式兜底（Java LocalDateTime.parse，含分数秒）
		{"2026-04-10T14:30:45.123", tp(2026, 4, 10, 14, 30, 45)},
		{"bad", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := parseDateTime(c.in)
		if c.want == nil {
			if got != nil {
				t.Fatalf("%q → %v, want nil", c.in, got)
			}
			continue
		}
		if got == nil || !got.Truncate(time.Second).Equal(*c.want) {
			t.Fatalf("%q → %v, want %v", c.in, got, c.want)
		}
	}
}

func tp(y int, m time.Month, d, h, min int, sec ...int) *time.Time {
	s := 0
	if len(sec) > 0 {
		s = sec[0]
	}
	t := time.Date(y, m, d, h, min, s, 0, time.UTC)
	return &t
}

func TestParseRoundNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"第二轮", 2},
		{"第 十 场", 10},
		{"第3轮", 3},
		{"第十轮", 10},  // 命中「十」→ 10
		{"第十二轮", 10}, // 仅识别单个数字字符（Java 现状）
		{"", 1},
		{"没有轮次", 1},
	}
	for _, c := range cases {
		if got := parseRoundNumber(c.in); got != c.want {
			t.Fatalf("parseRoundNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParse接口规则路径契约(t *testing.T) {
	store := newFakeStore()
	r := newScheduleRouter(store)

	code, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule/parse", map[string]any{
		"rawText": feishuSample,
		"source":  "feishu",
	})
	if code != http.StatusOK || out["code"].(float64) != 200 {
		t.Fatalf("HTTP/code = %d/%v", code, out["code"])
	}
	data := out["data"].(map[string]any)
	if data["success"] != true || data["confidence"] != 0.95 || data["parseMethod"] != "rule" || data["log"] != "规则解析成功" {
		t.Fatalf("响应形态 = %v", data)
	}
	got := data["data"].(map[string]any)
	if got["companyName"] != "阿里巴巴" || got["interviewTime"] != "2026-04-10T14:30:00" {
		t.Fatalf("解析产物 = %v", got)
	}
	if got["interviewType"] != "VIDEO" {
		t.Fatalf("interviewType = %v", got["interviewType"])
	}
}

func TestParse空文本与校验(t *testing.T) {
	r := newScheduleRouter(newFakeStore())
	// rawText 缺失/空白 → 400「文本不能为空」（@NotBlank）
	for _, body := range []map[string]any{
		{},
		{"rawText": "   "},
	} {
		_, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule/parse", body)
		if out["code"].(float64) != 400 || out["message"] != "文本不能为空" {
			t.Fatalf("%v → %v/%v", body, out["code"], out["message"])
		}
	}
	// 无规则可解析 → 解析失败形态（AI 兜底票 03 接入）
	_, out := doJSON(t, r, http.MethodPost, "/api/interview-schedule/parse", map[string]any{
		"rawText": "随便聊聊天",
	})
	data := out["data"].(map[string]any)
	if data["success"] != false || data["confidence"] != 0.0 || data["parseMethod"] != "none" || data["log"] != "解析失败" {
		t.Fatalf("失败形态 = %v", data)
	}
	if data["data"] != nil {
		t.Fatalf("失败形态 data = %v, want null", data["data"])
	}
}
