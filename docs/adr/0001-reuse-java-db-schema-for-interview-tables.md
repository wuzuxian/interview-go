# 模拟面试表沿用 Java 版表结构与既有数据

本仓库与 Java 项目 interview-guide 共用同一个 PostgreSQL 库。模拟面试相关表（文字面试 `interview_sessions`、`interview_answers`；语音面试 `voice_interview_sessions`、`voice_interview_messages`、`voice_interview_evaluations`）完全沿用 Java 版的表结构（同表名同列名，GORM Entity 按 Java 列定义）与库中已有数据；Go 侧的启动期幂等 DDL 迁移**不覆盖**这些表，schema 演进改由 DBA 在生产环境操作。

原因：前后端契约与历史数据（Java 版产生的场次）都必须兼容，重新设计表结构只有迁移成本没有收益。

## Considered Options

- 重新设计 Go 风格表结构 + 迁移脚本 —— 拒绝：破坏与 Java 数据的兼容，且已复刻模块均走对齐路线。
- Go 迁移里幂等补列 —— 拒绝（用户明确要求）：schema 所有权移交 DBA，应用只假设表已存在。

## Consequences

- Go Entity 未声明的 Java 列（如后期加的恢复/栅栏类列）对 GORM 不可见，按需逐步引用。
- 在没有跑过 Java 版迁移的全新环境，需先由 DBA 建上述模拟面试表（含语音三张表），应用启动不负责创建。
