# 经验：recommendation 为空导致前端崩溃 —— 跨团队数据契约与 PDF 中文提取

> 日期：2026-10-28
> 影响模块：简历管理（resume）· 文档解析（docparse）
> 现象等级：P1（简历详情页整页白屏崩溃 + PDF 乱码导致评分失真）

## 一、现象

用户上传中文简历 PDF，分析"完成"，打开详情页整页白屏，控制台报：

```
Uncaught Error: Objects are not valid as a React child
(found: object with keys {category, priority, issue, recommendation})
    at SuggestionSection (AnalysisPanel.tsx:735)
```

同时接口返回的 `resumeText` 里成段乱码（`:Ä +AVA!?öö`、`yM`、`ÁR` 等），
LLM 的评分理由甚至写着"简历存在严重的字符乱码问题导致核心内容无法阅读"——
简历被乱码拖累，只得了 42 分。

## 二、根因分析

### 根因 1：前端模板的隐式契约被一条空 `recommendation` 击穿

`AnalysisPanel.tsx` 的渲染模板：

```tsx
<p>{s.recommendation || s}</p>   {/* ← 崩溃点 */}
```

`s.recommendation` 为**空字符串**时为 falsy，回退渲染 `{s}` —— 整个对象被当作
React 子节点，React 直接抛错、整棵树卸载白屏。该响应里第三条建议恰好是：

```json
{"category":"技能","priority":"高","issue":"技能描述过度夸大…；"掌握RAG/MCP","recommendation":""}
```

`issue` 在句中被截断、`recommendation` 为空——典型的 **LLM 输出在 max token
边缘被截断**（服务端 JSON mode 补齐了括号所以仍能解析），结构上合法、内容上残缺。

同类隐式契约还有一处：前端按 `s.priority === '高'|'中'|'低'` 分组过滤，
priority 为其它值（如 LLM 输出 "critical"）的建议会被**静默丢弃**不渲染。

这类契约在接口文档里不存在，只活在模板表达式里——出问题才被发现。

### 根因 2：纯 Go PDF 提取器对中文 CID 内嵌字体产生乱码

原实现用 `ledongthuc/pdf` 纯 Go 提取。该简历 PDF 混用字体：正文字体带
ToUnicode 映射可正常提取，**标题/标签字体（姓名、性别、工作经验等短字段）
的 CID→Unicode 映射解析失败**，产出 `:Ä +AVA!?öö` 一类乱码。
后端对解析结果毫无质检，乱码直接进库、直接进提示词，模型被迫给
"格式严重问题"扣分——**评分失真是解析缺陷的二次伤害**。

## 三、修复内容

| # | 修复 | 位置 |
|---|------|------|
| 1 | 新增 `NormalizeSuggestions`：trim 字段、priority 归一（high/critical→高 等变体映射，其余归"中"）、空 category→"其他"、空 issue→占位、**空 recommendation 必填兜底文案**、全空条目丢弃 | `internal/resume/suggestion.go` |
| 2 | 归一化在**写入路径**（评分落库前）与**读取路径**（详情 DTO、最新分析 DTO——重复上传回放与 PDF 导出共用）同时生效，已入库的脏数据无需清洗即恢复正常展示 | `internal/resume/grading.go`、`internal/resume/service.go` |
| 3 | PDF 提取优先调用 poppler `pdftotext -enc UTF-8`（60s 超时，临时文件入/stdout 出），探测顺序：PATH → `~/.local/poppler/bin/pdftotext`；不可用或输出为空时回退纯 Go 方案 | `internal/docparse/parse.go` |
| 4 | 回归测试：`suggestion_test.go` 覆盖崩溃样本/非法优先级/空 issue；`parse_test.go` 用真实乱码 PDF 做乱码特征断言（Latin-1 补充区字符计数） | 同目录 `*_test.go` |

修复后对 resume 5 重分析（清空正文触发消费者从存储重新解析）：
文本完全干净（"姓名: 吴祖贤"逐字正确），5 条建议全部有非空 recommendation，
前端详情页正常渲染，得分 62（不再被乱码扣分）。

### poppler 安装（无 root 环境）

```bash
# micromamba 静态二进制 → ~/.local/micromamba（bz2 解压可用 python3 -c "import bz2..."）
~/.local/micromamba/bin/micromamba create -y -p ~/.local/poppler -c conda-forge poppler
# 产物: ~/.local/poppler/bin/pdftotext
```

## 四、经验教训

1. **LLM 结构化输出的 schema 校验要覆盖"内容完整性"而不只是"JSON 合法性"**。
   JSON mode + 补齐括号意味着"能解析"≠"没截断"。对关键字段（本例
   recommendation、issue）做空值/长度断言并归一化，是解析层的职责。
2. **对接不可修改的前端时，要把模板表达式当接口契约读**。
   `a || b` 这类 falsy 回退是 JS 惯用法，但对"对象 vs 字符串"的联合类型，
   空字符串会翻转渲染分支。后端字段约束文档里应写明"非空"这类硬性前提，
   并在读取层做防御，而不是假设数据永远干净。
3. **外部输入进 LLM 提示词前要过质检**。乱码、截断、控制字符都会被模型
   当作"简历本身的问题"参与扣分——解析缺陷会伪装成业务评分结果，
   且模型一本正经地解释乱码原因，极具迷惑性。入库/入提示词前对提取文本
   做可读性检查（如字符集分布统计）值得做成常规步骤。
4. **中文 PDF 文本提取选型：优先 poppler pdftotext**。纯 Go 方案
   （ledongthuc/pdf 等）对内嵌 CID 子集字体的 ToUnicode 映射支持不完整，
   中文简历场景乱码率高；pdftotext 表现稳定且免 root 可装到用户目录。
   外部二进制依赖要设计探测 + 回退（部署机无 poppler 时降级为纯 Go）。
5. **运维脚本里 pkill -f 是自伤高发区**：模式匹配会命中包含同样字符串的
   当前 shell 命令行（如 `go build -o /tmp/xxx-server .`），把自己杀掉导致
   后半段命令静默丢失。用 `-x` 精确匹配、或把模式拆成变量拼接规避，
   重启后必须核对启动日志确认新版本真的在跑（本系列第二次踩中，已固化
   到经验：验证前先确认进程身份）。

## 五、遗留与后续

- 提取文本可读性质检（乱码占比阈值 → 自动回退 pdftotext/标记低置信度）尚未实现，
  当前依赖 pdftotext 优先路径天然规避。
- `pdftotext` 探测结果进程内缓存（sync.Once），部署机运行期新装 poppler 需重启进程生效。
