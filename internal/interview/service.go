package interview

import (
	"io/fs"
	"log/slog"
)

// Service Skill 子系统服务：启动时解析嵌入资源构建方向注册表，之后只读。
// 对应 Java 版 InterviewSkillService 的资源加载与查询部分
// （JD 解析在票 02，参考内容注入在票 07）。
type Service struct {
	reg    *Registry
	fsys   fs.FS
	logger *slog.Logger
}

// NewService 从嵌入的 skills 资源构建服务（对齐 Java @PostConstruct loadPresetSkills 的启动语义）。
// 注册表构建失败（front matter 缺失、YAML 非法）返回错误，由启动方决定退出。
func NewService(logger *slog.Logger) (*Service, error) {
	fsys := EmbeddedSkills()
	reg, skipped, err := LoadRegistry(fsys)
	if err != nil {
		return nil, err
	}
	for _, id := range skipped {
		logger.Warn("跳过无效 Skill（缺少 name）", slog.String("skillId", id))
	}
	logger.Info("预设 Skill 注册表已加载",
		slog.Int("count", len(reg.SkillIDs())),
		slog.Int("skipped", len(skipped)))
	return &Service{reg: reg, fsys: fsys, logger: logger}, nil
}

// ListSkills GET /api/interview/skills：全部预设方向（按 ID 升序）。
func (s *Service) ListSkills() []SkillDTO { return s.reg.List() }

// GetSkill GET /api/interview/skills/{id}；不存在返回 interview 段业务码。
func (s *Service) GetSkill(skillID string) (*SkillDTO, error) { return s.reg.Get(skillID) }

// Categories 方向的分类列表（兜底题模板生成用）；方向不存在返回 (nil, false)。
func (s *Service) Categories(skillID string) ([]CategoryDTO, bool) {
	return s.reg.Categories(skillID)
}

// LocateReference 按 ref/shared 规则定位参考题库文件，返回嵌入资源中的路径。
// 为票 07 的 Prompt 注入预留；找不到返回 false（对齐 Java loadReferenceContent 空串语义）。
func (s *Service) LocateReference(skillID, ref string, shared bool) (string, bool) {
	return s.reg.LocateReference(s.fsys, skillID, ref, shared)
}
