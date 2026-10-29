// 出入参 DTO：字段名与 Java InterviewScheduleDTO / CreateInterviewRequest /
// ParseRequest / ParseResponse 逐字对齐（camelCase json tag）；时间序列化对齐
// Java LocalDateTime（无时区概念，存什么是什么）。
package interviewschedule

import (
	"fmt"
	"strings"
	"time"
)

// NaiveTime 无时区时间（对齐 Java LocalDateTime 线协议）：
// 序列化恒为 yyyy-MM-dd'T'HH:mm:ss（DTO interviewTime 的 @JsonFormat、
// createdAt/updatedAt 的 Jackson ISO 无时区形态）；反序列化接受 16 位
// （yyyy-MM-dd'T'HH:mm）与 19 位（带 :ss）两种（创建请求 @JsonFormat
// pattern="yyyy-MM-dd'T'HH:mm[:ss]"），解析为 UTC 承载墙钟值，全链路不做时区换算。
type NaiveTime time.Time

const (
	naiveTimeFormatSec = "2006-01-02T15:04:05"
	naiveTimeFormatMin = "2006-01-02T15:04"
)

// MarshalJSON 序列化为无时区 ISO 字符串。
func (t NaiveTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).Format(naiveTimeFormatSec) + `"`), nil
}

// UnmarshalJSON 接受 16/19 位两种格式；null/空串落零值（由参数校验兜「不能为空」）。
func (t *NaiveTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*t = NaiveTime{}
		return nil
	}
	for _, layout := range []string{naiveTimeFormatSec, naiveTimeFormatMin} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = NaiveTime(v)
			return nil
		}
	}
	return fmt.Errorf("无法解析时间（期望 yyyy-MM-dd'T'HH:mm[:ss]）: %s", s)
}

// IsZero 是否零值（请求校验用，对齐 Java @NotNull）。
func (t NaiveTime) IsZero() bool { return time.Time(t).IsZero() }

// Time 转标准库时间。
func (t NaiveTime) Time() time.Time { return time.Time(t) }

// FlexTime 可选秒时间（对齐 Java CreateInterviewRequest.interviewTime 的
// @JsonFormat pattern="yyyy-MM-dd'T'HH:mm[:ss]"）：序列化秒为 0 时省略 :ss，
// 否则带秒；反序列化与 NaiveTime 相同。仅用于解析响应/请求体中的
// CreateInterviewRequest（InterviewScheduleDTO 用 NaiveTime 恒带秒）。
type FlexTime time.Time

// MarshalJSON 秒为 0 时省略 :ss（[:ss] 可选节语义）。
func (t FlexTime) MarshalJSON() ([]byte, error) {
	v := time.Time(t)
	if v.Second() == 0 {
		return []byte(`"` + v.Format(naiveTimeFormatMin) + `"`), nil
	}
	return []byte(`"` + v.Format(naiveTimeFormatSec) + `"`), nil
}

// UnmarshalJSON 同 NaiveTime（16/19 位两种格式）。
func (t *FlexTime) UnmarshalJSON(b []byte) error {
	var n NaiveTime
	if err := n.UnmarshalJSON(b); err != nil {
		return err
	}
	*t = FlexTime(n.Time())
	return nil
}

// IsZero 是否零值。
func (t FlexTime) IsZero() bool { return time.Time(t).IsZero() }

// Time 转标准库时间。
func (t FlexTime) Time() time.Time { return time.Time(t) }

// CreateInterviewRequest 创建/更新请求（Java CreateInterviewRequest，8 业务字段；
// 更新走同一请求体，status 不可经此修改）。字段校验消息逐字对齐 Java @NotBlank/@NotNull。
// 同时作为解析响应 data（Java parse 返回 CreateInterviewRequest）。
type CreateInterviewRequest struct {
	CompanyName   string   `json:"companyName"`
	Position      string   `json:"position"`
	InterviewTime FlexTime `json:"interviewTime"`
	InterviewType *string  `json:"interviewType"` // ONSITE/VIDEO/PHONE，纯字符串透传不校验取值
	MeetingLink   *string  `json:"meetingLink"`
	RoundNumber   *int     `json:"roundNumber"` // Java 字段默认 1（解析产物恒非 null）
	Interviewer   *string  `json:"interviewer"`
	Notes         *string  `json:"notes"`
}

// InterviewScheduleDTO 日程读写体（Java InterviewScheduleDTO，12 字段逐字）。
type InterviewScheduleDTO struct {
	ID            int64           `json:"id"`
	CompanyName   string          `json:"companyName"`
	Position      string          `json:"position"`
	InterviewTime NaiveTime       `json:"interviewTime"`
	InterviewType *string         `json:"interviewType"`
	MeetingLink   *string         `json:"meetingLink"`
	RoundNumber   *int            `json:"roundNumber"`
	Interviewer   *string         `json:"interviewer"`
	Notes         *string         `json:"notes"`
	Status        InterviewStatus `json:"status"`
	CreatedAt     NaiveTime       `json:"createdAt"`
	UpdatedAt     NaiveTime       `json:"updatedAt"`
}

// ParseRequest 邀约解析请求（Java ParseRequest）。
type ParseRequest struct {
	RawText string  `json:"rawText"` // @NotBlank「文本不能为空」
	Source  *string `json:"source"`  // feishu/tencent/zoom/other，可选
}

// ParseResponse 邀约解析响应（Java ParseResponse，5 字段逐字）。
type ParseResponse struct {
	Success     bool                    `json:"success"`
	Data        *CreateInterviewRequest `json:"data"` // 失败为 null
	Confidence  float64                 `json:"confidence"`
	ParseMethod string                  `json:"parseMethod"` // rule / ai / none
	Log         string                  `json:"log"`
}
