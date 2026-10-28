// 自定义方向临时构建：从 Java InterviewSkillService.buildCustomSkill /
// sanitizeCategoryKey / sanitizeCategoryLabel（笔记 04 §8）逐字移植。
package interview

import (
	"log/slog"
	"regexp"
	"strings"
)

// 自定义方向 key/label 约束（对齐 Java 常量，InterviewSkillService.java:45-47）。
const (
	customSkillName        = "自定义面试（JD 解析）"
	customSkillDescription = "基于职位描述提取的面试方向"
	maxCategoryKeyLength   = 50
	maxCategoryLabelLength = 50
)

// crlfPattern label 内换行折叠为空格（对齐 Java [\\r\\n]+）。
var crlfPattern = regexp.MustCompile(`[\r\n]+`)

// BuildCustomSkill 用 JD 解析出的分类临时构建自定义方向（不落库，spec 决策 11）。
// LLM 给出的 ref/shared 仅是建议值：key 命中 categoryRefIndex 时按本地映射纠正
//（未命中时保留 LLM 的 ref，shared 恒按 true 处理——对齐 Java Boolean.TRUE.equals）。
// persona 为 null（无 SKILL.md），出题时不注入人设段。
func (s *Service) BuildCustomSkill(customCategories []CategoryDTO, jdText string) *SkillDTO {
	categories := make([]CategoryDTO, 0, len(customCategories))
	for _, cat := range customCategories {
		safeKey := sanitizeCategoryKey(cat.Key)
		safeLabel := sanitizeCategoryLabel(cat.Label)
		if ref, shared, _, ok := s.reg.RefMapping(safeKey); ok {
			modelRef := ""
			if cat.Ref != nil {
				modelRef = *cat.Ref
			}
			if ref != modelRef || shared != cat.Shared {
				s.logger.Info("JD 分类 reference 已按本地映射纠正",
					slog.String("key", safeKey),
					slog.String("modelRef", modelRef), slog.Bool("modelShared", cat.Shared),
					slog.String("mappedRef", ref), slog.Bool("mappedShared", shared))
			}
			categories = append(categories, CategoryDTO{
				Key: safeKey, Label: safeLabel, Priority: cat.Priority,
				Ref: &ref, Shared: shared,
			})
			continue
		}
		categories = append(categories, CategoryDTO{
			Key: safeKey, Label: safeLabel, Priority: cat.Priority,
			Ref: cat.Ref, Shared: true,
		})
	}

	matched := 0
	for _, c := range categories {
		if c.Ref != nil && *c.Ref != "" {
			matched++
		}
	}
	s.logger.Info("构建自定义 Skill",
		slog.Int("categories", len(categories)), slog.Int("refMatched", matched))

	return &SkillDTO{
		ID:          CustomSkillID,
		Name:        customSkillName,
		Description: customSkillDescription,
		Categories:  categories,
		IsPreset:    false,
		SourceJd:    stringPtr(jdText),
		Persona:     nil,
		Display:     nil,
	}
}

// sanitizeCategoryKey 对齐 Java 同名方法（562-581 行）：
// 空白 → UNKNOWN；trim → 截 50 → 大写 → 非法字符替换为 _；首字符非字母加 CAT_ 前缀。
func sanitizeCategoryKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return "UNKNOWN"
	}
	trimmed := strings.TrimSpace(key)
	if len([]rune(trimmed)) > maxCategoryKeyLength {
		trimmed = string([]rune(trimmed)[:maxCategoryKeyLength])
	}
	upper := strings.ToUpper(trimmed)
	upper = replaceIllegalKeyChars(upper)
	if upper == "" {
		return "UNKNOWN"
	}
	// 确保首字符为字母（匹配 category key 命名规范）。
	if !isASCIILetter(rune(upper[0])) {
		upper = "CAT_" + upper
	}
	return upper
}

// replaceIllegalKeyChars 等价 Java toUpperCase().replaceAll("[^A-Z0-9_]", "_")。
func replaceIllegalKeyChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isASCIILetter(r) || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func isASCIILetter(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

// sanitizeCategoryLabel 对齐 Java 同名方法（583-593 行）：
// 空白 → 未命名；trim → 换行折叠为空格 → 截 50。
func sanitizeCategoryLabel(label string) string {
	if strings.TrimSpace(label) == "" {
		return "未命名"
	}
	trimmed := crlfPattern.ReplaceAllString(strings.TrimSpace(label), " ")
	if len([]rune(trimmed)) > maxCategoryLabelLength {
		trimmed = string([]rune(trimmed)[:maxCategoryLabelLength])
	}
	return trimmed
}
