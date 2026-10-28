// 语音面试 prompt 组装，对齐 Java VoiceInterviewPromptService（L18-53）与
// DashscopeLlmService.buildPromptContext（L160-182）：
//
//	system = 面试官人设 + 方向内容（静态嵌入，Java 为 SkillsTool 工具调用——
//	         ADR 0002 增补的有意偏离，首句人设逐字保留）+ 语音输出约束（逐字）
//	         + 候选人简历（随机边界标签包裹）+ 防注入段（复用 interview 包常量）
//	user   = 【之前的对话】逐条 sanitize 拼接 + 【当前对话】+ 用户：边界标签包裹
package voiceinterview

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/wuzuxian/interview/internal/interview"
)

// voiceResponseConstraints 语音输出约束（Java VOICE_RESPONSE_CONSTRAINTS text block
// 逐字，含尾随换行）。
const voiceResponseConstraints = "【语音面试输出约束】\n" +
	"1. 每轮只问 1 个主问题，必要时最多补 1 个短追问。\n" +
	"2. 总长度控制在 2-4 句，避免长段落、列表、Markdown、代码块。\n" +
	"3. 不要重复开场白，不要复述上一轮已问过的完整问题。\n" +
	"4. 若候选人回答过短或含糊，直接追问一个具体的技术细节或给出提示引导，不要简单确认后停止。\n" +
	"5. 当候选人明确要求换题时，立即切换到新的技术方向，不要停留在当前话题。\n" +
	"6. 语气简洁直接，适配口语对话。\n"

// voiceSkillInstruction 静态嵌入版角色加载段（Java SKILL_TOOL_INSTRUCTION 的无工具
// 偏离：首句人设逐字保留；工具调用两行替换为方向内容嵌入说明，见 docs/adr/0002）。
const voiceSkillInstruction = "你是一位 %s 方向的面试官。\n" +
	"以下【方向内容】是该技能 SKILL.md 的完整面试官角色设定与出题规则，后续对话应基于该角色进行。\n\n" +
	"【方向内容】\n%s"

// skillPersonaMissing 方向内容缺失时的占位（方向不存在或未配置正文时降级，日志告警）。
const skillPersonaMissing = "（该方向内容暂缺，请按通用技术面试官的角色与出题规则提问）"

// SkillPersonaSource 方向内容来源（复用 internal/interview 的 Skill 资源子系统，
// 生产实现 *interview.Service，测试注入 stub）。
type SkillPersonaSource interface {
	GetSkill(skillID string) (*interview.SkillDTO, error)
}

// PromptService 语音面试 prompt 组装器。
type PromptService struct {
	sanitizer *interview.PromptSanitizer
	skills    SkillPersonaSource // 可 nil：跳过方向内容嵌入
	logger    *slog.Logger
}

// NewPromptService 创建 prompt 组装器。
func NewPromptService(sanitizer *interview.PromptSanitizer, skills SkillPersonaSource, logger *slog.Logger) *PromptService {
	return &PromptService{sanitizer: sanitizer, skills: skills, logger: logger}
}

// SystemPrompt 对齐 Java generateSystemPromptWithContext L34-53：
// 方向段（skillId 非空白时）+ "\n\n" + 语音约束 + 简历段（resumeText 非空串时，
// Java 判 null/isEmpty）+ 防注入段收尾。
func (s *PromptService) SystemPrompt(skillID, resumeText string) string {
	var b strings.Builder

	if strings.TrimSpace(skillID) != "" {
		b.WriteString(fmt.Sprintf(voiceSkillInstruction, skillID, s.skillContent(skillID)))
	}

	b.WriteString("\n\n")
	b.WriteString(voiceResponseConstraints)

	if resumeText != "" {
		safeResume := s.sanitizer.Sanitize(resumeText)
		b.WriteString("\n\n【实时语音面试 - 候选人简历内容】\n")
		b.WriteString("你已查阅过候选人简历。首轮仅用一句话说明已查阅，并立即进入首个问题。\n\n")
		b.WriteString("【简历解析文本】\n")
		b.WriteString(s.sanitizer.WrapWithDelimiters("resume", safeResume))
	}

	b.WriteString(interview.AntiInjectionInstruction)
	return b.String()
}

// UserPrompt 对齐 Java buildPromptContext L171-181：历史非空时
// 【之前的对话】+ 逐条 sanitize 行 + 空行 + 【当前对话】；末尾
// 「用户：」+ 输入 sanitize 后边界标签包裹。
func (s *PromptService) UserPrompt(userInput string, history []string) string {
	var b strings.Builder
	if len(history) > 0 {
		b.WriteString("【之前的对话】\n")
		for _, message := range history {
			b.WriteString(s.sanitizer.Sanitize(message))
			b.WriteString("\n")
		}
		b.WriteString("\n【当前对话】\n")
	}
	b.WriteString("用户：")
	b.WriteString(s.sanitizer.WrapWithDelimiters("input", s.sanitizer.Sanitize(userInput)))
	return b.String()
}

// skillContent 方向内容静态嵌入（Java 无此方法：SkillsTool 工具调用由模型发起，
// Go 在组装期直接读取 SKILL.md 正文，ADR 0002）。加载失败降级占位文案，不阻断回合。
func (s *PromptService) skillContent(skillID string) string {
	if s.skills == nil {
		return skillPersonaMissing
	}
	def, err := s.skills.GetSkill(skillID)
	if err != nil || def == nil || def.Persona == nil || strings.TrimSpace(*def.Persona) == "" {
		if s.logger != nil {
			s.logger.Warn("语音面试方向内容加载失败，降级占位",
				slog.String("skillId", skillID), slog.String("error", voiceErrText(err)))
		}
		return skillPersonaMissing
	}
	return *def.Persona
}

func voiceErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
