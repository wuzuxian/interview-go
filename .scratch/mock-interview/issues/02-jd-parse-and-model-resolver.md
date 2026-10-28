# 02: JD 解析 + ModelResolver 接缝

**Spec:** [../spec.md](../spec.md)（决策 3、4、12）

**What to build:** 定义 interview 模块的 ModelResolver 极小接口（按 providerId 解析 ChatCompleter、取默认 providerId），由现有 aibridge 桥实现并在 main 组装注入——这是本模块唯一新增的接缝，后续所有 LLM 消费都走它。移植 JD 解析 prompt（system 含参考文件清单与匹配规则、key 大写下划线约束、防注入段），上线 `POST /api/interview/skills/parse-jd`：JD 文本 ≥50 字校验、IP 限流、用户数据清洗，用真实 LLM 结构化输出 3-7 个分类（key/label/priority CORE|NORMAL/ref/shared）。解析出的自定义方向不落库（前端回传、临时构建，与 Java 一致）。

**Blocked by:** 01（JD 解析 prompt 需要参考文件清单）

**Status:** ready-for-agent

- [ ] ModelResolver 接口由 aibridge 实现，启动时注入 interview 模块；providerId 不存在/被禁用时返回设置模块既有业务码
- [ ] parse-jd 走现有结构化输出器（JSON 模式 + 解析失败带修复提示重试 2 次 + JSON 修复），prompt 从 Java 逐字移植含防注入段
- [ ] JD 少于 50 字被拒绝；响应 data 为分类数组，字段与 Java 契约一致
- [ ] IP 限流生效（窗口参数以 Java 注解为准）
- [ ] prompt 组装有单测（stub ChatCompleter 断言参考文件清单注入与数据边界包裹）
- [ ] 有真实 Provider 时手动验证可解析出 3-7 个方向
