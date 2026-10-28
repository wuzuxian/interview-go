package interview

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/apperr"
)

// jdParseSystemPrompt JD 解析 system 模板，逐字节拷贝自 Java 参考实现
// app/src/main/resources/prompts/jd-parse-system.st（{referenceFileList} 为占位符）。
//
//go:embed prompts/jd-parse-system.st
var jdParseSystemPrompt string

// minJdLength JD 最短长度，对齐 Java MIN_JD_LENGTH（按字符数计）。
const minJdLength = 50

// jdCategoryList LLM 输出目标结构，对齐 Java CategoryListDTO。
type jdCategoryList struct {
	Categories []JdCategoryDTO `json:"categories"`
}

// jdSchemaDesc 输出 JSON 形状说明，等价 Java BeanOutputConverter#getFormat()
// 为 CategoryListDTO 生成的结构约束（由结构化输出器追加到 system prompt）。
const jdSchemaDesc = "{categories:[{key:string, label:string, priority:string, ref:string, shared:bool}]}"

// ParseJd 解析职位描述为 3-7 个面试考察方向（对齐 Java InterviewSkillService.parseJd）。
// 结果不落库：自定义方向由前端回传、创建会话时临时构建（spec 决策 11）。
func (s *Service) ParseJd(ctx context.Context, jdText string) ([]JdCategoryDTO, error) {
	// Java String.length() 为 UTF-16 码元数，对中文（BMP 字符）等价于字符数，这里按 rune 计。
	if utf8.RuneCountInString(jdText) < minJdLength {
		return nil, apperr.New(apperr.CodeBadRequest,
			fmt.Sprintf("JD 内容太少（至少 %d 字），请补充后重试", minJdLength))
	}

	chatID := s.models.DefaultChatID()
	cm, err := s.models.JSONChatModelFor(ctx, chatID)
	if err != nil {
		// Provider 不存在/禁用：透传 aibridge/设置模块既有业务错误。
		return nil, err
	}

	// system：模板（占位符替换为参考文件清单）+ 防注入段；
	// JSON 格式说明由 ai.InvokeStructured 统一追加在最后。
	system := strings.ReplaceAll(jdParseSystemPrompt, "{referenceFileList}", s.referenceFileList) +
		antiInjectionInstruction
	// user：数据边界声明 + 清洗后的 JD 随机边界包裹（逐字对齐 Java parseJd 的 userPrompt 拼接）。
	user := dataBoundaryInstruction + "\n" +
		"职位描述：\n" +
		s.sanitizer.WrapWithDelimiters("jd", s.sanitizer.Sanitize(jdText))

	out, err := ai.InvokeStructured[jdCategoryList](
		ctx, s.invoker, cm, system, user,
		jdSchemaDesc, "JD 解析", apperr.CodeAIServiceError)
	if err != nil {
		return nil, err
	}
	if out == nil || len(out.Categories) == 0 {
		return nil, apperr.New(apperr.CodeAIServiceError, "JD 解析结果为空，请重试")
	}

	refMatched := 0
	for _, c := range out.Categories {
		if c.Ref != nil && *c.Ref != "" {
			refMatched++
		}
	}
	if s.logger != nil {
		s.logger.Info("JD 解析完成",
			slog.Int("categories", len(out.Categories)),
			slog.Int("refMatched", refMatched),
			slog.Int("jdLength", len(jdText)))
	}
	return out.Categories, nil
}
