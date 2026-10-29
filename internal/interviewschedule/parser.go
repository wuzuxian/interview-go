// 规则解析器（Java InterviewParseService 的正则部分逐字移植）：飞书/腾讯会议/
// Zoom 三套格式。正则、字段提取与选择顺序全部对齐 Java L44-63 / L127-294，
// 含以下逐字现状（勿当 bug"修正"）：
//   - 三套规则的 interviewType 一律置 "VIDEO"；
//   - 腾讯会议 meetingLink 以 matcher.group()（全匹配，含「会议号：」前缀）拼接，
//     产物形如「会议号: 会议号：123456789 密码: 密码：1234」；
//   - Zoom 解析不提取公司/岗位（产物通常无效，走兜底逐个尝试或 AI）；
//   - 中文数字轮次仅识别单个数字字符：「第十二轮」命中「十」→ 10（Java 现状）。
package interviewschedule

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 三套正则逐字对齐 Java Pattern 定义（Java L44-63）。
var (
	// 飞书
	timePatternFeishu     = regexp.MustCompile(`(?:时间|时段)[：:]\s*(\d{4}[-/]\d{2}[-/]\d{2}\s+\d{2}:\d{2})`)
	linkPatternFeishu     = regexp.MustCompile(`https://meeting\.feishu\.cn/[^\s\n]+`)
	companyPatternFeishu  = regexp.MustCompile(`(?:公司|单位|组织)[：:]\s*([^\s\n]{1,50})`)
	positionPatternFeishu = regexp.MustCompile(`(?:岗位|职位|职务)[：:]\s*([^\s\n]{1,50})`)
	roundPatternFeishu    = regexp.MustCompile(`第\s*[一二三四五六七八九十\d]+\s*[轮场]`)

	// 腾讯会议
	timePatternTencent      = regexp.MustCompile(`(\d{4}[-/]\d{2}[-/]\d{2})\s+(\d{2}:\d{2})`)
	meetingIDPatternTencent = regexp.MustCompile(`(?:会议号|ID)[：:]?\s*(\d{9,})`)
	passwordPatternTencent  = regexp.MustCompile(`密码[：:]?\s*(\d{4,})`)
	companyPatternTencent   = regexp.MustCompile(`(?:公司|单位)[：:]\s*([^\s\n]{1,50})`)
	positionPatternTencent  = regexp.MustCompile(`(?:岗位|职位)[：:]\s*([^\s\n]{1,50})`)

	// Zoom
	linkPatternZoom = regexp.MustCompile(`https://zoom\.us/j/[^\s\n]+`)
	datePatternZoom = regexp.MustCompile(`(\d{4}[-/]\d{2}[-/]\d{2})`)
	hourPatternZoom = regexp.MustCompile(`(\d{1,2}:\d{2})`)

	// 轮次数字
	roundNumberPattern = regexp.MustCompile(`[一二三四五六七八九十]|\d`)
	pureDigitsPattern  = regexp.MustCompile(`^\d+$`)
)

// chineseNumbers 中文数字表（Java CHINESE_NUMBERS）。
var chineseNumbers = map[string]int{
	"一": 1, "二": 2, "三": 3, "四": 4, "五": 5,
	"六": 6, "七": 7, "八": 8, "九": 9, "十": 10,
}

// tryRuleParsing 规则解析入口（Java tryRuleParsing L127-161 逐字）：
// source 指定（feishu/tencent/zoom，忽略大小写）→ 直接走对应解析器；
// 无 source → 文本特征探测（各自命中且产物有效即返回）→ 最后无条件逐个兜底
// （feishu → tencent → zoom）。
func tryRuleParsing(rawText, source string) *CreateInterviewRequest {
	switch strings.ToLower(source) {
	case "feishu":
		return parseFeishu(rawText)
	case "tencent":
		return parseTencent(rawText)
	case "zoom":
		return parseZoom(rawText)
	}

	if strings.Contains(rawText, "飞书") || strings.Contains(rawText, "Feishu") || strings.Contains(rawText, "meeting.feishu.cn") {
		if result := parseFeishu(rawText); isValidResult(result) {
			return result
		}
	}
	if strings.Contains(rawText, "腾讯会议") || strings.Contains(rawText, "Tencent Meeting") || strings.Contains(rawText, "会议号") {
		if result := parseTencent(rawText); isValidResult(result) {
			return result
		}
	}
	if strings.Contains(rawText, "Zoom") || strings.Contains(rawText, "zoom.us") {
		if result := parseZoom(rawText); isValidResult(result) {
			return result
		}
	}

	if result := parseFeishu(rawText); isValidResult(result) {
		return result
	}
	if result := parseTencent(rawText); isValidResult(result) {
		return result
	}
	return parseZoom(rawText)
}

// isValidResult 产物有效性（Java L429-434）：公司、岗位、时间三要素非空。
func isValidResult(result *CreateInterviewRequest) bool {
	return result != nil &&
		result.CompanyName != "" &&
		result.Position != "" &&
		!result.InterviewTime.IsZero()
}

// parseFeishu 飞书格式（Java parseFeishu L165-210）。
func parseFeishu(rawText string) *CreateInterviewRequest {
	request := &CreateInterviewRequest{}
	if m := timePatternFeishu.FindStringSubmatch(rawText); m != nil {
		if t := parseDateTime(m[1]); t != nil {
			request.InterviewTime = NaiveTime(*t)
		}
	}
	if link := linkPatternFeishu.FindString(rawText); link != "" {
		request.MeetingLink = &link
	}
	if m := companyPatternFeishu.FindStringSubmatch(rawText); m != nil {
		request.CompanyName = strings.TrimSpace(m[1])
	}
	if m := positionPatternFeishu.FindStringSubmatch(rawText); m != nil {
		request.Position = strings.TrimSpace(m[1])
	}
	if round := roundPatternFeishu.FindString(rawText); round != "" {
		request.RoundNumber = intPtr(parseRoundNumber(round))
	}
	request.InterviewType = strPtr("VIDEO")
	return request
}

// parseTencent 腾讯会议格式（Java parseTencent L214-261）：meetingLink 由全匹配
// 拼接（Java matcher.group() 现状，见文件头注释）。
func parseTencent(rawText string) *CreateInterviewRequest {
	request := &CreateInterviewRequest{}
	if m := timePatternTencent.FindStringSubmatch(rawText); m != nil {
		if t := parseDateTime(m[1] + " " + m[2]); t != nil {
			request.InterviewTime = NaiveTime(*t)
		}
	}
	var meetingLink strings.Builder
	if m := meetingIDPatternTencent.FindString(rawText); m != "" {
		meetingLink.WriteString("会议号: ")
		meetingLink.WriteString(m)
	}
	if m := passwordPatternTencent.FindString(rawText); m != "" {
		meetingLink.WriteString(" 密码: ")
		meetingLink.WriteString(m)
	}
	if meetingLink.Len() > 0 {
		s := meetingLink.String()
		request.MeetingLink = &s
	}
	if m := companyPatternTencent.FindStringSubmatch(rawText); m != nil {
		request.CompanyName = strings.TrimSpace(m[1])
	}
	if m := positionPatternTencent.FindStringSubmatch(rawText); m != nil {
		request.Position = strings.TrimSpace(m[1])
	}
	request.InterviewType = strPtr("VIDEO")
	return request
}

// parseZoom Zoom 格式（Java parseZoom L265-294；不提取公司/岗位）。
func parseZoom(rawText string) *CreateInterviewRequest {
	request := &CreateInterviewRequest{}
	if link := linkPatternZoom.FindString(rawText); link != "" {
		request.MeetingLink = &link
	}
	date := datePatternZoom.FindStringSubmatch(rawText)
	hour := hourPatternZoom.FindStringSubmatch(rawText)
	if date != nil && hour != nil {
		if t := parseDateTime(date[1] + " " + hour[1]); t != nil {
			request.InterviewTime = NaiveTime(*t)
		}
	}
	request.InterviewType = strPtr("VIDEO")
	return request
}

// parseDateTime 时间解析（Java parseDateTime L396-409）：`/`→`-` 后按长度分发
// （16 位 yyyy-MM-dd HH:mm、19 位带秒、否则 ISO 本地格式兜底）；失败返回 nil
// （该字段留空，不中断其他字段提取）。
func parseDateTime(timeStr string) *time.Time {
	timeStr = strings.ReplaceAll(timeStr, "/", "-")
	var (
		t   time.Time
		err error
	)
	switch len(timeStr) {
	case 16:
		t, err = time.ParseInLocation("2006-01-02 15:04", timeStr, time.UTC)
	case 19:
		t, err = time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.UTC)
	default:
		t, err = time.ParseInLocation("2006-01-02T15:04:05.999999999", timeStr, time.UTC)
		if err != nil {
			t, err = time.ParseInLocation("2006-01-02T15:04", timeStr, time.UTC)
		}
	}
	if err != nil {
		slog.Error("时间解析失败", slog.String("time", timeStr), slog.String("error", err.Error()))
		return nil
	}
	return &t
}

// parseRoundNumber 轮次解析（Java parseRoundNumber L411-427）：纯数字直接取；
// 否则取首个中文数字/数字字符查表；兜底 1（「第十二轮」命中「十」→ 10，Java 现状）。
func parseRoundNumber(text string) int {
	if text == "" {
		return 1
	}
	text = strings.TrimSpace(text)
	if pureDigitsPattern.MatchString(text) {
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
	}
	if num := roundNumberPattern.FindString(text); num != "" {
		if n, ok := chineseNumbers[num]; ok {
			return n
		}
		if n, err := strconv.Atoi(num); err == nil {
			return n
		}
	}
	return 1
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }
