package interview

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wuzuxian/interview/internal/apperr"
)

// CustomSkillID JD 解析生成的自定义方向 ID（对齐 Java 常量），临时构建、不落库。
const CustomSkillID = "custom"

// skills 资源原样拷贝自 Java 参考实现 app/src/main/resources/skills/，
// 目录名以 skill/ 为前缀避免与包内 Go 文件混淆；all: 前缀必须保留，
// 否则 go:embed 会默认排除 _shared 这类下划线开头的目录。
//
//go:embed all:skill/skills
var embeddedSkillAssets embed.FS

// EmbeddedSkills 返回嵌入的 skills 资源文件系统，根即 skills 目录
// （路径形如 java-backend/SKILL.md、_shared/references/java.md）。
func EmbeddedSkills() fs.FS {
	sub, err := fs.Sub(embeddedSkillAssets, "skill/skills")
	if err != nil {
		// 字面量路径编译期已固定，只有资源目录被误删才会到达这里。
		panic(fmt.Sprintf("skills 嵌入资源缺失: %v", err))
	}
	return sub
}

const (
	skillMetaFile       = "skill.meta.yml"
	sharedReferenceDir  = "_shared/references"
	skillDefinitionFile = "SKILL.md"
)

// frontMatterPattern 与 Java 版正则逐字对齐：提取 SKILL.md 头部 YAML 与正文。
var frontMatterPattern = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n?(.*)$`)

// frontMatterDef SKILL.md front matter（标准字段），对齐 Java SkillFrontMatterDefinition。
type frontMatterDef struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// displayDef skill.meta.yml 展示字段，对齐 Java DisplayDef。
type displayDef struct {
	Icon      string `yaml:"icon"`
	Gradient  string `yaml:"gradient"`
	IconBg    string `yaml:"iconBg"`
	IconColor string `yaml:"iconColor"`
}

// categoryDef skill.meta.yml 分类定义，对齐 Java CategoryDef。
type categoryDef struct {
	Key      string `yaml:"key"`
	Label    string `yaml:"label"`
	Priority string `yaml:"priority"`
	Ref      string `yaml:"ref"`
	Shared   bool   `yaml:"shared"`
}

// metaDef skill.meta.yml（项目自定义字段），对齐 Java SkillMetaDefinition。
type metaDef struct {
	DisplayName string        `yaml:"displayName"`
	Display     *displayDef   `yaml:"display"`
	Categories  []categoryDef `yaml:"categories"`
}

// skillDefinition 运行时聚合结构，对齐 Java SkillDefinition。
type skillDefinition struct {
	name        string
	description string
	persona     *string
	displayName string
	display     *DisplayDTO
	categories  []CategoryDTO
}

// refMapping 全局 category key → 参考文件映射（对齐 Java RefMapping），
// 供 JD 解析出的自定义方向纠正 ref 使用（票 02/07）。
type refMapping struct {
	ref           string
	shared        bool
	sourceSkillID string
}

// Registry 预设方向注册表，启动时从嵌入的 skills 资源构建，之后只读。
// 遍历一律按 skillID 升序（对齐 Java TreeMap 语义）。
type Registry struct {
	byID             map[string]*skillDefinition
	categoryRefIndex map[string]refMapping
}

// LoadRegistry 解析资源根目录下所有 {skillId}/SKILL.md 与 skill.meta.yml，
// 构建方向注册表（对齐 Java @PostConstruct loadPresetSkills）。
// 缺 name 的方向被跳过并记录在 skipped 中；缺 front matter 或 YAML 非法返回错误
// （Java 版为启动失败语义）。fsys 的根即 skills 目录。
func LoadRegistry(fsys fs.FS) (*Registry, []string, error) {
	paths, err := fs.Glob(fsys, "*/"+skillDefinitionFile)
	if err != nil {
		return nil, nil, err
	}

	reg := &Registry{
		byID:             map[string]*skillDefinition{},
		categoryRefIndex: map[string]refMapping{},
	}
	var skipped []string

	for _, p := range paths {
		skillID := path.Dir(p)
		if skillID == "." || skillID == "_shared" {
			// _shared 是参考库不是方向（与 Java 一致）。
			continue
		}

		def, err := parseSkillDefinition(fsys, skillID, p)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(def.name) == "" {
			skipped = append(skipped, skillID)
			continue
		}
		reg.byID[skillID] = def
	}

	reg.buildCategoryRefIndex()
	return reg, skipped, nil
}

// parseSkillDefinition 解析单个方向：front matter（name/description）+
// 正文（persona）+ meta 文件（displayName/display/categories）。
func parseSkillDefinition(fsys fs.FS, skillID, mdPath string) (*skillDefinition, error) {
	markdown, err := fs.ReadFile(fsys, mdPath)
	if err != nil {
		return nil, fmt.Errorf("读取 Skill 文件失败: %s: %w", mdPath, err)
	}

	m := frontMatterPattern.FindSubmatch(markdown)
	if m == nil {
		return nil, fmt.Errorf("Skill 文件格式错误（缺少 front matter）: %s", mdPath)
	}

	var fm frontMatterDef
	if err := yaml.Unmarshal(m[1], &fm); err != nil {
		return nil, fmt.Errorf("解析 Skill front matter 失败: %s: %w", mdPath, err)
	}

	def := &skillDefinition{name: fm.Name, description: fm.Description}
	if body := strings.TrimSpace(string(m[2])); body != "" {
		def.persona = &body
	}

	meta, err := loadSkillMeta(fsys, skillID)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		def.displayName = meta.DisplayName
		if meta.Display != nil {
			def.display = &DisplayDTO{
				Icon:      meta.Display.Icon,
				Gradient:  meta.Display.Gradient,
				IconBg:    meta.Display.IconBg,
				IconColor: meta.Display.IconColor,
			}
		}
		for _, c := range meta.Categories {
			cat := CategoryDTO{
				Key:      c.Key,
				Label:    c.Label,
				Priority: c.Priority,
				Shared:   c.Shared,
			}
			if c.Ref != "" {
				ref := c.Ref
				cat.Ref = &ref
			}
			def.categories = append(def.categories, cat)
		}
	}
	if def.categories == nil {
		def.categories = []CategoryDTO{}
	}
	return def, nil
}

// loadSkillMeta 读取 skill.meta.yml；文件不存在返回 (nil, nil)（对齐 Java：使用默认配置）。
func loadSkillMeta(fsys fs.FS, skillID string) (*metaDef, error) {
	metaPath := skillID + "/" + skillMetaFile
	content, err := fs.ReadFile(fsys, metaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 skill meta 文件失败: %s: %w", metaPath, err)
	}

	var meta metaDef
	if err := yaml.Unmarshal(content, &meta); err != nil {
		return nil, fmt.Errorf("解析 skill meta 文件失败: %s: %w", metaPath, err)
	}
	return &meta, nil
}

// buildCategoryRefIndex 构建 category key → 参考文件映射，启动时一次，之后只读。
func (r *Registry) buildCategoryRefIndex() {
	for _, id := range r.SkillIDs() {
		def := r.byID[id]
		for _, cat := range def.categories {
			if cat.Ref == nil || *cat.Ref == "" || cat.Key == "" {
				continue
			}
			if _, exists := r.categoryRefIndex[cat.Key]; !exists {
				r.categoryRefIndex[cat.Key] = refMapping{ref: *cat.Ref, shared: cat.Shared, sourceSkillID: id}
			}
		}
	}
}

// SkillIDs 返回全部预设方向 ID（升序，对齐 Java TreeMap）。
func (r *Registry) SkillIDs() []string {
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Get 返回单个方向的 DTO；不存在返回 interview 段业务码（404 语义）。
func (r *Registry) Get(skillID string) (*SkillDTO, error) {
	def, ok := r.byID[skillID]
	if !ok {
		return nil, apperr.SkillNotFound(skillID)
	}
	return r.toDTO(skillID, def), nil
}

// List 返回全部预设方向 DTO（按 ID 升序，对齐 Java getAllSkills）。
func (r *Registry) List() []SkillDTO {
	ids := r.SkillIDs()
	out := make([]SkillDTO, 0, len(ids))
	for _, id := range ids {
		out = append(out, *r.toDTO(id, r.byID[id]))
	}
	return out
}

// toDTO 对齐 Java toSkillDTO：name 优先 displayName；isPreset 恒 true；sourceJd 恒 null。
func (r *Registry) toDTO(id string, def *skillDefinition) *SkillDTO {
	name := def.displayName
	if strings.TrimSpace(name) == "" {
		name = def.name
	}
	categories := make([]CategoryDTO, len(def.categories))
	copy(categories, def.categories)
	return &SkillDTO{
		ID:          id,
		Name:        name,
		Description: def.description,
		Categories:  categories,
		IsPreset:    true,
		SourceJd:    nil,
		Persona:     def.persona,
		Display:     def.display,
	}
}

// RefMapping 返回 category key 的参考文件映射（供自定义方向纠正 ref，票 02 使用）。
func (r *Registry) RefMapping(key string) (ref string, shared bool, sourceSkillID string, ok bool) {
	m, exists := r.categoryRefIndex[key]
	if !exists {
		return "", false, "", false
	}
	return m.ref, m.shared, m.sourceSkillID, true
}

// ResolveReferenceLocations 返回参考文件的候选路径（相对 skills 根，按查找优先级），
// 与 Java resolveReferenceLocations 规则逐条对齐：
//  1. shared=true 时共享库 _shared/references/ 优先；
//  2. 方向自身 references/ 与根目录两处；
//  3. shared=false 时方向自身优先、共享库回落；
//  4. custom 方向或 shared=true 时扫全部预设方向（去重，保持首见顺序）。
//
// 不安全路径（路径穿越/绝对路径/非法字符）返回空切片。
func (r *Registry) ResolveReferenceLocations(skillID, ref string, shared bool) []string {
	if !isSafeReferencePath(ref) {
		return []string{}
	}

	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	addSkillLocations := func(id string) {
		if id == "" || id == CustomSkillID {
			return
		}
		add(id + "/references/" + ref)
		add(id + "/" + ref)
	}

	sharedLocation := sharedReferenceDir + "/" + ref
	if shared {
		add(sharedLocation)
	}
	addSkillLocations(skillID)
	if !shared {
		add(sharedLocation)
	}
	if skillID == CustomSkillID || shared {
		for _, preset := range r.SkillIDs() {
			addSkillLocations(preset)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// LocateReference 按候选顺序在 fsys 中定位第一个存在的参考文件。
// 返回其在 fsys 中的路径；不可定位（不安全/不存在/内容为空目录项）返回 false。
// 对齐 Java loadReferenceContent 的“找不到返回空串”语义，供票 07 注入 Prompt。
func (r *Registry) LocateReference(fsys fs.FS, skillID, ref string, shared bool) (string, bool) {
	for _, p := range r.ResolveReferenceLocations(skillID, ref, shared) {
		if _, err := fs.Stat(fsys, p); err == nil {
			return p, true
		}
	}
	return "", false
}

// isSafeReferencePath 对齐 Java 同名方法：拒绝路径穿越、绝对路径与非法字符。
func isSafeReferencePath(ref string) bool {
	if ref == "" ||
		strings.Contains(ref, "..") ||
		strings.HasPrefix(ref, "/") ||
		strings.HasPrefix(ref, "\\") {
		return false
	}
	for _, r := range ref {
		allowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !allowed {
			return false
		}
	}
	return true
}
