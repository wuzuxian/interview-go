package interview

// SkillDTO GET /api/interview/skills 与 /{id} 的响应体，
// 字段名与 Java 版 InterviewSkillService.SkillDTO（record 驼峰序列化）逐字一致：
// id/name/description/categories/isPreset/sourceJd/persona/display。
// sourceJd 对预设方向恒为 null（JSON 保留 null，与 Java 默认 Jackson 行为一致）。
type SkillDTO struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Categories  []CategoryDTO `json:"categories"`
	IsPreset    bool          `json:"isPreset"`
	SourceJd    *string       `json:"sourceJd"`
	Persona     *string       `json:"persona"`
	Display     *DisplayDTO   `json:"display"`
}

// CategoryDTO 方向下的考察分类，对应 Java SkillCategoryDTO。
// ref 缺失时序列化为 null；shared 为布尔值（Java Boolean.TRUE.equals 兜底语义）。
type CategoryDTO struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Priority string  `json:"priority"`
	Ref      *string `json:"ref"`
	Shared   bool    `json:"shared"`
}

// JdCategoryDTO POST /api/interview/skills/parse-jd 的响应元素，
// 对应 Java InterviewSkillService.CategoryDTO（record）：key/label/priority/ref/shared。
// ref 与 shared 为可空（Java String/Boolean 语义）：LLM 未给出时序列化为 null 原样透传，
// 纠正 ref/shared 发生在创建会话的 buildCustomSkill（票 07），不在本接口。
type JdCategoryDTO struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Priority string  `json:"priority"`
	Ref      *string `json:"ref"`
	Shared   *bool   `json:"shared"`
}

// DisplayDTO 前端方向卡片展示字段，对应 Java DisplayDTO。
type DisplayDTO struct {
	Icon      string `json:"icon"`
	Gradient  string `json:"gradient"`
	IconBg    string `json:"iconBg"`
	IconColor string `json:"iconColor"`
}
