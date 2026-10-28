// 参考题库注入与截断：从 Java InterviewSkillService.buildReferenceSection 系列
// （笔记 04 §6/§7）逐字移植。出题预算 12000 字符、单文件 3000 字符（评估侧 6000 票 08 用）。
package interview

import (
	"io/fs"
	"log/slog"
	"strings"
)

// 截断预算与提示文案（对齐 Java 常量与字符串字面量）。
const (
	MaxReferenceSectionChars           = 12000
	MaxEvaluationReferenceSectionChars = 6000
	MaxSingleReferenceChars            = 3000

	truncatedTotalMarker  = "\n...（references 已截断）"
	truncatedSingleMarker = "\n...（单文件内容已截断）"
	noReferencesText      = "未配置 references。"
)

// BuildReferenceSection 出题侧入口：只注入分配数 > 0 的分类，总预算 12000（Java 328-334 行）。
func (s *Service) BuildReferenceSection(skill *SkillDTO, allocation map[string]int) string {
	return s.buildReferenceSectionInternal(skill, func(cat CategoryDTO) bool {
		return allocation[cat.Key] > 0
	}, MaxReferenceSectionChars)
}

// buildEvaluationReferenceSection 评估侧入口（票 08 使用）：覆盖全部配置了 ref 的分类，预算 6000。
func (s *Service) buildEvaluationReferenceSection(skill *SkillDTO) string {
	return s.buildReferenceSectionInternal(skill, func(CategoryDTO) bool { return true },
		MaxEvaluationReferenceSectionChars)
}

// BuildEvaluationReferenceSectionSafe 评估侧安全入口（票 06，对齐 Java
// buildEvaluationReferenceSectionSafe）：skillId 空 → ""；方向不存在/异常 → 告警降级 ""。
// 题目自带参考基线为空时由评估引擎回退到本段（EvaluateInput.SkillReference）。
func (s *Service) BuildEvaluationReferenceSectionSafe(skillID string) string {
	if strings.TrimSpace(skillID) == "" {
		return ""
	}
	skill, err := s.reg.Get(skillID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("加载评估参考基线失败，降级为无参考",
				slog.String("skillId", skillID), slog.String("error", err.Error()))
		}
		return ""
	}
	return s.buildEvaluationReferenceSection(skill)
}

// buildReferenceSectionInternal 核心实现（对齐 Java 363-403 行）：
// 按分类顺序拼接 "### label (key)\n" + 文件内容（段落间空行分隔）；
// 累计达预算时截断到预算并追加截断提示后停止；无任何内容返回占位文案。
func (s *Service) buildReferenceSectionInternal(skill *SkillDTO, filter func(CategoryDTO) bool, maxChars int) string {
	var sb strings.Builder
	empty := true

	for _, category := range skill.Categories {
		if !filter(category) {
			continue
		}
		if category.Ref == nil || *category.Ref == "" {
			continue
		}

		// custom 模式下非 shared 的 ref 需查 categoryRefIndex 用来源方向拼路径。
		effectiveSkillID := skill.ID
		if skill.ID == CustomSkillID && !category.Shared && category.Ref != nil {
			if _, _, sourceSkillID, ok := s.reg.RefMapping(category.Key); ok {
				effectiveSkillID = sourceSkillID
			}
		}
		referenceContent := s.loadReferenceContent(effectiveSkillID, *category.Ref, category.Shared)
		if referenceContent == "" {
			continue
		}

		if !empty {
			sb.WriteString("\n\n")
		}
		empty = false
		sb.WriteString("### " + category.Label + " (" + category.Key + ")\n")
		sb.WriteString(referenceContent)

		// 累计达预算：截断到预算并追加截断提示（对齐 Java setLength + append 语义）。
		if text := sb.String(); runeLen(text) >= maxChars {
			return truncateRunes(text, maxChars) + truncatedTotalMarker
		}
	}

	if empty {
		return noReferencesText
	}
	return sb.String()
}

// loadReferenceContent 按 ref/shared 规则定位并读取参考文件（对齐 Java 481-498 行）：
// 找不到或内容为空返回 ""（注入时该分类被跳过）。
func (s *Service) loadReferenceContent(skillID, ref string, shared bool) string {
	locations := s.reg.ResolveReferenceLocations(skillID, ref, shared)
	for _, p := range locations {
		content := s.readReferenceContent(p)
		if content != "" {
			return content
		}
	}
	if s.logger != nil {
		s.logger.Warn("未找到 reference",
			slog.String("skillId", skillID), slog.String("ref", ref), slog.Bool("shared", shared))
	}
	return ""
}

// readReferenceContent 读取单个参考文件：trim 后超 3000 字符截断并追加单文件截断提示
// （对齐 Java readReferenceContent，533-549 行）。嵌入资源读取失败按空内容处理（warn 不阻断）。
func (s *Service) readReferenceContent(path string) string {
	content, err := fs.ReadFile(s.fsys, path)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("读取 reference 失败", slog.String("path", path), slog.String("error", err.Error()))
		}
		return ""
	}
	text := strings.TrimSpace(string(content))
	if r := []rune(text); len(r) > MaxSingleReferenceChars {
		return string(r[:MaxSingleReferenceChars]) + truncatedSingleMarker
	}
	return text
}

// runeLen 字符串的字符数（对齐 Java String.length() 的中文 BMP 语义，按 rune 计）。
func runeLen(s string) int { return len([]rune(s)) }

// truncateRunes 截断到 maxChars 个字符。
func truncateRunes(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars])
}
