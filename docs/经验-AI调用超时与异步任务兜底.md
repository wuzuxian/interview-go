# 经验：AI 调用超时被兜底成 0 分"假完成" —— 根因分析与修复

> 日期：2026-10-28
> 影响模块：简历管理（resume）· AI 调用层（ai）· 向量化（vector）
> 现象等级：P1（静默数据污染：用户看到"分析完成"，实际是 0 分错误结果）

## 一、现象

简历上传成功，异步分析开始，约 2 分钟后日志报错：

```
10:58:01.410  INFO  Processing 简历分析 task  resumeId=4
11:00:01.417  ERROR 简历分析失败  error="[7002] AI服务响应超时，请稍后重试"
11:00:01.427  INFO  简历评测结果已保存  analysisId=6  score=0     ← 0 分结果被落库
11:00:31.430  WARN  简历向量化失败（不影响分析结果） ... context deadline exceeded
11:00:31.435  INFO  简历分析 task completed  resumeId=4            ← 任务被标记成功
```

用户侧表现：简历显示"分析完成"，评分为 0，摘要是一句"分析过程中出现错误"。

## 二、根因分析

### 直接原因：网络瞬时抖动
出错简历正文仅 3482 字符（比此前成功分析的简历还短），排除"简历过长"假设。
关键证据是**两个独立的外部调用背靠背超时**：LLM 评分调用在整整 120s（10:58:01→11:00:01）
被打断，紧接着 embedding 调用 30s 超时。两者同时失败指向当时到
dashscope 的网络链路瞬时不稳定，而非提示词或模型问题。
事后重试同一条简历，62s 即正常完成（score=72）。

### 根本原因：错误被"兜底响应"吞掉，重试机制全程失灵
原实现（与 Java 版 ResumeGradingService 同构）：

```go
func (g *GradingService) Analyze(...) *AnalysisResponse {
    res, err := g.analyzeStrict(ctx, text)
    if err != nil {
        return createErrorResponse(text) // ← 超时被吞，返回 0 分兜底对象
    }
    return res
}
```

消费者拿到的是"成功的结果"，于是直接落库并标记 COMPLETED——
**Redis Stream 的 3 次重试、恢复调度器、FAILED+手动重分析这一整套
容错体系在这一行代码面前全部失效**。一次 2 分钟的网络抖动
被永久固化为一条假"分析完成"记录。

### 次要因素
1. **超时预算过紧且不可观测**：LLM 调用 120s、向量化 30s；慢而成功的
   响应会被误杀，且日志里没有调用耗时，无法区分"慢成功"与"真超时"。
2. **embedding 输入无上限**：text-embedding-v3 单文本上限 8192 token，
   超长输入既慢又可能被服务端拒绝。

## 三、修复内容

| # | 修复 | 位置 |
|---|------|------|
| 1 | `GradingService.Analyze` 改为返回 error，AI 失败向上传播给 Stream 消费者：重置 PENDING → 重新入队，最多重试 3 次，耗尽后转 FAILED（`analyze_error` 记录原因，前端出现"重新分析"按钮）。**彻底删除 0 分兜底落库** | `internal/resume/grading.go`、`internal/resume/stream.go` |
| 2 | LLM 单次调用超时 120s → 300s（`llm_call_timeout` 可配），并区分"任务级重试"（Stream 3 次）与"单次调用预算"两层 | `internal/ai/structured.go`、`config.yaml` |
| 3 | 向量化上下文 30s → 60s，embedding 输入按 rune 安全截断到 6000 字符 | `internal/vector/vector_store.go`、`internal/resume/stream.go` |
| 4 | 可观测性：LLM 调用记录耗时与提示词长度（`elapsed`/`promptChars`），评分前记录文本长度；超长评分输入截断到 15000 字符 | `internal/ai/structured.go`、`internal/resume/grading.go` |

修复后验证：对同一条简历重分析，日志输出
`结构化输出调用完成 elapsed=62.5s promptChars=14699`，任务 COMPLETED，得分 72，向量化成功。

## 四、经验教训

1. **"兜底值"和"错误"必须区分对待**。只有当失败结果本身对用户仍有价值
   （如部分成功的数据）时才允许降级落库；对外部依赖（LLM/网络）的失败
   应当原样上抛，交给重试机制。判断标准：*如果重试可能成功，就不能提前
   把失败固化成成功*。
2. **重试链路要端到端贯通并实际演练**。消费者层写好了 3 次重试，
   但服务层一行 `return createErrorResponse()` 就让它变成了死代码。
   容错设计要按"错误从产生到被重试的完整路径"检查，而不是只看每一层自身。
3. **超时预算要基于观测数据**。给外部 AI 调用设超时前先测量真实分布
   （小简历 ~60s，长输出可能数分钟），预算不足时宁可配大 + 上层重试，
   也不要把临界值卡在正常样本附近；同时记录耗时日志，否则无法区分
   "慢成功"和"卡死"。
4. **两个无关依赖同时失败 = 先怀疑共同链路**。LLM 和 embedding 是两个
   独立服务，背靠背超时时优先排查共同的网络路径（代理/DNS/带宽），
   而不是各自的重试参数。
5. **改完服务必须确认新二进制真的在跑**。本次验证时重启脚本只等 1s，
   旧进程未释放端口导致新进程 bind 失败，"验证通过"其实来自旧版本。
   重启脚本要等待端口释放，重启后先核对启动日志/行为再跑验证。

## 五、遗留与后续

- 历史上的 0 分记录（analysisId=6）保留在分析历史中作为痕迹，重新分析后
  最新结果已正常；如需清理可删除 `resume_analyses` 中 `overall_score=0 AND summary LIKE '分析过程中出现错误%'` 的记录。
- 可选增强：将 Stream 重试次数与恢复调度器参数接入配置文件（当前为常量）。
