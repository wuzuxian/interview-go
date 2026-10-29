// AI 兜底解析（Java InterviewParseService.parseWithAI L298-392 + PARSE_PROMPT
// L65-90 逐字移植）。与 Java 的唯一行为偏离（访谈裁决 Q1）：Java 把 source 直接
// 当 LLM providerId 传注册表（parse 调用点 L116/L306），source=feishu/tencent/
// zoom 时按 id 直查 Provider 几乎必然不存在、异常被吞、AI 兜底失效——是缺陷而非
// 特性；Go 一律走默认 Chat Provider（等价 Java 的 getChatClientOrDefault 默认
// 分支），source 仅参与规则解析的格式选择。
package interviewschedule

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/wuzuxian/interview/internal/interview"
)

// ChatModel 对话模型接缝（非流式；Java ChatClient.prompt().user().call().content()，
// 普通 ChatModel 非 JSON 响应模式）。生产实现为 eino ChatModel（aibridge
// RagChatModelFor 默认 Provider 通道，main 注入），测试注入 stub。
type ChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// ChatModelFactory 按默认 Provider 构建对话模型（knowledgebase.ChatModelFactory
// 同构，main 注入使两包互不依赖）。
type ChatModelFactory func(ctx context.Context) (ChatModel, error)

// parsePrompt 邀约解析提示词（Java PARSE_PROMPT text block 逐字，两个 %s 槽位：
// 当前日期、数据边界声明 + 边界标签包裹的清洗后原文）。
const parsePrompt = `你是一个专业的面试邀约信息提取助手。请仔细分析以下文本，提取面试相关信息。

**提取规则**：
1. companyName（公司名称）：提取面试公司的全称或简称，**必需字段**
2. position（岗位名称）：提取面试岗位的名称，**必需字段**
3. interviewTime（面试时间）：提取面试开始时间并转换为 ISO 8601 格式，**必需字段**
   - 格式：YYYY-MM-DDTHH:MM:SS（例如：2026-04-10T14:00:00）
   - 若只有相对时间（如"明天下午2点"），根据当前日期 %s 推算
4. interviewType（面试形式）：ONSITE（现场）/ VIDEO（视频）/ PHONE（电话）
5. meetingLink（会议链接）：提取完整的会议链接或会议号+密码
6. roundNumber（第几轮面试）：提取数字（1-10），如"二面"提取为2
7. notes（其他备注）：包含面试官姓名（如果不重要可忽略）、时长（**默认30分钟**）等。

**重要提示**：
- 面试官是谁不重要，只需在 notes 中提及。
- 优先保证 companyName、position、interviewTime 的准确性。
- 如果文本中没说时长，默认设置为 30 分钟。

**待解析文本**：
%s

**返回格式**：
纯 JSON 格式，不要包含` + "```" + `json标记，示例：
{"companyName":"阿里巴巴","position":"Java工程师","interviewTime":"2026-04-10T14:00:00","interviewType":"VIDEO","meetingLink":"https://meeting.feishu.cn/xxx","roundNumber":2,"interviewer":"张三","notes":"技术面"}
`

// markdownJSONPattern 提取 ```json 代码块（Java L321 逐字）。
var markdownJSONPattern = regexp.MustCompile("```(?:json)?\\s*([\\s\\S]*?)\\s*```")

// parseWithAI AI 兜底解析：异常全吞（模型构建/调用失败、空返回、非 JSON 均返回
// nil，走「解析失败」形态，Java L388-391 catch 语义）。
// 【偏离清单第 10 条】AI 把必需字段返回为空串时：Java isValidResult 仅判非 null
// → 仍成功 0.8 并产出空公司名；Go 按空串无效落「解析失败」（Go string 无 null
// 语义，不值得为不可达边角引入存在性追踪；规则路径的正则要求非空，不受影响）。
func (s *ParseService) parseWithAI(ctx context.Context, rawText string) *CreateInterviewRequest {
	// 【偏离清单第 1 条，访谈裁决 Q1】Java 此处把 source 当 providerId 传注册表
	// （parse 调用点 L116/L306），Go 一律走默认 Provider（文件头注释详述）。
	if s.models == nil {
		s.logger.Error("AI 解析不可用：未注入对话模型")
		return nil
	}
	chatModel, err := s.models(ctx)
	if err != nil {
		s.logger.Error("构建 AI 解析模型失败", slog.String("error", err.Error()))
		return nil
	}

	currentDate := time.Now().Format("2006-01-02")
	safeRawText := s.sanitizer.Sanitize(rawText)
	prompt := fmt.Sprintf(parsePrompt, currentDate,
		interview.DataBoundaryInstruction+"\n"+
			s.sanitizer.WrapWithDelimiters("parse-input", safeRawText))

	resp, err := chatModel.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil {
		s.logger.Error("AI 解析调用失败", slog.String("error", err.Error()))
		return nil
	}
	content := ""
	if resp != nil {
		content = resp.Content
	}
	if strings.TrimSpace(content) == "" {
		s.logger.Error("AI 解析返回内容为空")
		return nil
	}

	// 提取 ```json 代码块（Java L319-326：含 ``` 才尝试，未命中保留原文）。
	jsonContent := strings.TrimSpace(content)
	if strings.Contains(jsonContent, "```") {
		if m := markdownJSONPattern.FindStringSubmatch(jsonContent); m != nil {
			jsonContent = strings.TrimSpace(m[1])
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonContent), &parsed); err != nil {
		s.logger.Error("AI 返回内容不是合法 JSON", slog.String("error", err.Error()))
		return nil
	}
	if len(parsed) == 0 {
		s.logger.Error("JSON 解析返回空结果")
		return nil
	}

	request := &CreateInterviewRequest{RoundNumber: intPtr(1)} // Java 字段默认 1
	if v := mapString(parsed, "companyName"); v != nil {
		request.CompanyName = strings.TrimSpace(*v)
	}
	if v := mapString(parsed, "position"); v != nil {
		request.Position = strings.TrimSpace(*v)
	}
	if v := mapString(parsed, "interviewTime"); v != nil {
		timeStr := strings.TrimSpace(*v)
		var (
			t   time.Time
			err error
		)
		if len(timeStr) == 16 { // YYYY-MM-DDTHH:MM → 补 ":00"（Java L350-353）
			t, err = time.ParseInLocation("2006-01-02T15:04:05", timeStr+":00", time.UTC)
		} else {
			t, err = time.ParseInLocation("2006-01-02T15:04:05.999999999", timeStr, time.UTC)
			if err != nil {
				t, err = time.ParseInLocation("2006-01-02T15:04", timeStr, time.UTC)
			}
		}
		if err != nil {
			s.logger.Error("AI 返回的时间格式不正确", slog.String("interviewTime", timeStr))
		} else {
			request.InterviewTime = FlexTime(t)
		}
	}
	if v := mapString(parsed, "interviewType"); v != nil {
		request.InterviewType = strPtr(strings.TrimSpace(*v))
	}
	if v := mapString(parsed, "meetingLink"); v != nil {
		request.MeetingLink = strPtr(strings.TrimSpace(*v))
	}
	if raw, ok := parsed["roundNumber"]; ok && raw != nil {
		// Java：toString 后 Integer.parseInt，失败回落默认 1（L368-375）。
		if n, err := strconv.Atoi(fmt.Sprintf("%v", raw)); err == nil {
			*request.RoundNumber = n
		}
	}
	if v := mapString(parsed, "interviewer"); v != nil {
		request.Interviewer = strPtr(strings.TrimSpace(*v))
	}
	if v := mapString(parsed, "notes"); v != nil {
		request.Notes = strPtr(strings.TrimSpace(*v))
	}

	s.logger.Info("AI 解析成功", slog.String("companyName", request.CompanyName))
	return request
}

// mapString 取字符串字段（Java result.get(x).toString()；数字/布尔同样转字符串）。
func mapString(m map[string]any, key string) *string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	s := fmt.Sprintf("%v", v)
	return &s
}
