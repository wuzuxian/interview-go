# 01: Skill 资源子系统（方向列表/详情）

**Spec:** [../spec.md](../spec.md)（决策 1、11）

**What to build:** 把 Java 参考实现的 skills 资源（10 个预设方向目录 + _shared 参考库）原样拷贝进本仓库并 go:embed 打包；启动时解析每个方向的 SKILL.md front matter 与 meta YAML（分类 key/label/priority/ref/shared、考官人设、展示字段），构建方向注册表。上线 `GET /api/interview/skills` 与 `GET /api/interview/skills/{id}`，响应字段与 Java 契约逐字对齐（含 isPreset、display 图标/渐变等）。本票同时建立 interview 业务模块包骨架（controller/service 分层）与路由注册。无 LLM、无 DB。

**Blocked by:** None (can start immediately)

**Status:** claimed

- [ ] `GET /api/interview/skills` 返回 10 个预设方向，字段与 Java 版一致（id/name/description/categories/isPreset/display）
- [ ] 分类含 key、label、priority（ALWAYS_ONE/CORE/NORMAL）、ref、shared
- [ ] `GET /skills/{id}` 返回单方向；不存在的方向返回 apperr 业务码 404 语义
- [ ] front matter/YAML 解析有表驱动单测（含缺字段、非法 priority 等脏数据用例）
- [ ] 参考题库文件可按 ref/shared 规则解析定位（为票 07 的注入做准备），文件存在性校验有测试
