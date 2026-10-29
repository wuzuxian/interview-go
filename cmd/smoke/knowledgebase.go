// 知识库全链路冒烟（票 05 知识库模式）：-mode knowledgebase 的实现。链路对齐票面：
//
//	上传（真实 S3 + SHA-256 去重路径）→ 轮询向量化完成（真 PG/pgvector + eino
//	Embedder + Redis Stream worker）→ RAG 查询（改写/分档检索/[S#] 引用回答）→
//	拒答探测（无命中固定文案）→ 异步出题（question-gen Stream → 结构化输出 →
//	replace 小事务）→ 批量启用（DRAFT→ACTIVE，对齐前端"启用"动作；面试只取
//	ACTIVE）→ 面试容量预判 → 创建知识库面试场次（sourceType=KNOWLEDGE_BASE，
//	题目携带参考答案/评分要点/评分规则/来源上下文）→ 模拟作答提交（末题自动
//	交卷）→ 轮询评估（题目自带参考基线评分）→ 报告校验（每题参考答案）→ 清理
//	（删场次 + 删知识库，-keep 跳过）。
//
// 步骤化输出与失败即停非零退出沿既有 smoke 风格（票 09/10）。真实运行需要
// PG/pgvector/Redis/RustFS 与可用 LLM/Embedding Provider；不进 CI（spec 测试
// 决策 5），单测不覆盖流程本身。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// kb 固定拒答文案与出题/面试参数上限（与服务端契约一致，校验用）。
const (
	kbNoResultResponse = "抱歉，在选定的知识库中未检索到相关信息。请换一个更具体的关键词或补充上下文后再试。"
	kbCategory         = "冒烟验证"
)

// runKnowledgeBase 知识库冒烟主流程（-mode knowledgebase）。
func runKnowledgeBase(opts *options, out io.Writer) error {
	base := strings.TrimRight(opts.baseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("参数 -base-url 非法：%q（须包含 http:// 或 https://）", base)
	}
	if opts.kbMain < 1 || opts.kbMain > 20 {
		return fmt.Errorf("参数 -kb-main 非法：%d（须在 1-20）", opts.kbMain)
	}
	if opts.kbFollow < 0 || opts.kbFollow > 5 {
		return fmt.Errorf("参数 -kb-follow 非法：%d（须在 0-5）", opts.kbFollow)
	}
	client := &http.Client{Timeout: opts.timeout}
	sm := &smoke{client: client, base: base, out: out, opts: opts, stepElapsed: map[string]time.Duration{}}
	kb := &kbFlow{sm: sm}

	fmt.Fprintf(out, "== 知识库全链路冒烟（票 05）==\n服务: %s  主问题: %d  追问: %d  难度: %s  超时: %s\n\n",
		base, opts.kbMain, opts.kbFollow, opts.difficulty, opts.timeout)

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"健康检查", sm.stepHealth},
		{"上传知识库文档", kb.stepUpload},
		{"轮询向量化完成", kb.stepWaitVectorized},
		{"RAG 查询", kb.stepQuery},
		{"拒答探测", kb.stepNoResult},
		{"发起出题并轮询完成", kb.stepGenerateQuestions},
		{"批量启用题目", kb.stepActivateQuestions},
		{"面试容量预判", kb.stepCapacity},
		{"创建知识库面试场次", kb.stepCreateSession},
		{"模拟作答提交", kb.stepAnswerAll},
		{"轮询评估状态", kb.stepPollEvaluation},
		{"报告校验", kb.stepReport},
		{"清理场次与知识库", kb.stepCleanup},
	}
	runErr := executeSteps(context.Background(), out, sm, steps)
	// 失败即停时清理步骤不会执行：提示残留知识库，避免旧语料撞去重误伤重跑。
	if runErr != nil && kb.kbID != 0 {
		return fmt.Errorf("%w\n（残留知识库 kbId=%d 未清理，可在管理页删除或 DELETE /api/knowledgebase/%d）",
			runErr, kb.kbID, kb.kbID)
	}
	return runErr
}

// kbFlow 知识库链路状态。
type kbFlow struct {
	sm *smoke

	kbID           int64
	kbName         string
	savedCount     int
	questionIDs    []int64
	capacityOK     bool
	totalQuestions int
}

// kbDocument 冒烟语料：多主题 Markdown（保证检索/出题/拒答三态可验证）。
func kbDocument() string {
	return `# 后端面试复习笔记（冒烟语料）

## Java 基础

HashMap 在 JDK 8 之后采用数组 + 链表 + 红黑树结构：链表长度超过 8 且数组容量达到 64 时树化，
查询复杂度从 O(n) 降到 O(log n)。扩容采用 2 倍容量并按 hash & newCap-1 重排，JDK 8 引入了
尾插法避免并发扩容成环，但 HashMap 本身仍不是线程安全的，并发场景应使用 ConcurrentHashMap。

ConcurrentHashMap 在 JDK 8 摒弃分段锁，改用 CAS + synchronized 锁住桶头结点，粒度更细，
size 统计依赖 baseCount + CounterCell 分散计数。

## JVM

JVM 内存区域分为堆、虚拟机栈、本地方法栈、程序计数器与方法区（元空间）。垃圾回收以分代假说
为基础：新生代使用复制算法（Eden + 两个 Survivor），老年代使用标记-整理或标记-清除。
G1 把堆划分为等长 Region，按停顿目标优先回收价值最高的区域；ZGC 通过着色指针与读屏障
把停顿压到亚毫秒级。

## 并发编程

synchronized 依赖对象头的 Mark Word 实现锁升级：无锁 → 偏向锁 → 轻量级锁（CAS 自旋）→
重量级锁（操作系统互斥量）。volatile 保证可见性与禁止指令重排，但不保证原子性。
AQS 用 volatile int state + CLH 变体队列实现独占与共享两种同步语义，ReentrantLock、
Semaphore、CountDownLatch 都构建其上。

## Spring

Spring 事务传播行为中 REQUIRED 是默认值：有事务则加入，没有则新建；REQUIRES_NEW 挂起当前
事务另起新事务；NESTED 在当前事务内设置保存点。事务失效的常见原因包括：自调用（绕过代理）、
方法非 public、异常被捕获吞掉、数据库引擎不支持事务。

Bean 生命周期：实例化 → 属性填充 → Aware 回调 → BeanPostProcessor 前置 → 初始化
（InitializingBean/init-method）→ 后置处理 → 使用 → 销毁回调。循环依赖靠三级缓存中的
early reference 暴露半成品解决，构造器循环依赖无解。

## MySQL

InnoDB 默认隔离级别是可重复读（RR），通过 Next-Key Lock（记录锁 + 间隙锁）在 RR 下阻止
幻读。redo log 保证崩溃恢复（物理日志，WAL），undo log 支持回滚与 MVCC 一致性读，
binlog 用于主从复制与归档。索引下推（ICP）把 where 条件下推到存储层过滤，减少回表。

## Redis

Redis 单线程命令执行配合 IO 多路复用；6.0 引入多线程 IO 但命令执行仍是单线程。
持久化：RDB 是某一刻的全量快照，AOF 追加写命令并以 everysec 刷盘权衡性能与安全。
缓存穿透用布隆过滤器或缓存空值，击穿用互斥锁或逻辑过期，雪崩用过期时间加随机抖动。

## 分布式

CAP 定理下分布式系统只能同时满足一致性、可用性与分区容忍性中的两项；BASE 是 AP 路线的
工程化折中。分布式锁基于 Redis SET NX PX + 唯一令牌 + Lua 校验删除实现，或直接采用
etcd/ZooKeeper 的租约与 watch 语义。幂等设计常用唯一业务号 + 去重表或状态机约束。
`
}

// stepUpload 生成语料并 multipart 上传（票面步骤：上传 → 向量化入口）。
func (k *kbFlow) stepUpload(ctx context.Context) error {
	sm := k.sm
	k.kbName = "冒烟知识库-" + newRequestID()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fw, err := writer.CreateFormFile("file", "冒烟语料.md")
	if err != nil {
		return fmt.Errorf("构造 multipart 失败: %v", err)
	}
	if _, err := fw.Write([]byte(kbDocument() + "\n\n---\n\n运行标识：" + newRequestID() + "\n")); err != nil {
		return fmt.Errorf("写入语料失败: %v", err)
	}
	_ = writer.WriteField("name", k.kbName)
	_ = writer.WriteField("category", kbCategory)
	if err := writer.Close(); err != nil {
		return fmt.Errorf("收尾 multipart 失败: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sm.base+"/api/knowledgebase/upload", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := sm.client.Do(req)
	if err != nil {
		return fmt.Errorf("上传请求失败（服务未运行？前置条件见 -h）: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取上传响应失败: %v", err)
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("上传 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var out struct {
		KnowledgeBase struct {
			ID           int64  `json:"id"`
			VectorStatus string `json:"vectorStatus"`
		} `json:"knowledgeBase"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return fmt.Errorf("解析上传响应 data 失败: %v", err)
	}
	if out.KnowledgeBase.ID == 0 {
		return fmt.Errorf("上传响应缺少 knowledgeBase.id: %s", string(env.Data))
	}
	if out.Duplicate {
		// 语料已附唯一运行标识行，命中 SHA-256 去重即为极小概率内容碰撞；
		// 此时重试会生成新的运行标识，必然成功——故保留「请重试」话术。
		return fmt.Errorf("冒烟语料意外命中重复（SHA-256 去重），请重试")
	}
	k.kbID = out.KnowledgeBase.ID
	sm.logf("kbId=%d vectorStatus=%s", k.kbID, out.KnowledgeBase.VectorStatus)
	return nil
}

// stepWaitVectorized 轮询详情直至 vectorStatus=COMPLETED 且 chunkCount>0。
func (k *kbFlow) stepWaitVectorized(ctx context.Context) error {
	sm := k.sm
	deadline := time.Now().Add(sm.opts.evalWait)
	lastProgress := time.Time{}
	for {
		raw, err := sm.do(ctx, http.MethodGet, "/api/knowledgebase/"+itoa64(k.kbID), nil)
		if err == nil {
			if env, perr := parseEnvelope(raw); perr == nil {
				var detail struct {
					VectorStatus string `json:"vectorStatus"`
					VectorError  string `json:"vectorError"`
					ChunkCount   int    `json:"chunkCount"`
				}
				if uerr := json.Unmarshal(env.Data, &detail); uerr == nil {
					switch detail.VectorStatus {
					case "COMPLETED":
						if detail.ChunkCount <= 0 {
							return fmt.Errorf("向量化完成但 chunkCount=%d，切片异常", detail.ChunkCount)
						}
						sm.logf("vectorStatus=COMPLETED chunkCount=%d", detail.ChunkCount)
						return nil
					case "FAILED":
						return fmt.Errorf("向量化失败 vectorError=%q", detail.VectorError)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("向量化超时（%s 内未达 COMPLETED）", sm.opts.evalWait)
		}
		if lastProgress.IsZero() || time.Since(lastProgress) >= progressInterval {
			sm.logf("向量化中 …（剩余 %s）", time.Until(deadline).Round(time.Second))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stepQuery RAG 查询：回答非空且与语料相关（命中内容关键词）。
func (k *kbFlow) stepQuery(ctx context.Context) error {
	raw, err := k.sm.do(ctx, http.MethodPost, "/api/knowledgebase/query", map[string]any{
		"knowledgeBaseIds": []int64{k.kbID},
		"question":         "HashMap 在 JDK 8 里用了什么数据结构？和 ConcurrentHashMap 有什么区别？",
	})
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("RAG 查询 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var out struct {
		Answer            string `json:"answer"`
		KnowledgeBaseName string `json:"knowledgeBaseName"`
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return fmt.Errorf("解析查询响应失败: %v", err)
	}
	if strings.TrimSpace(out.Answer) == "" {
		return fmt.Errorf("回答为空")
	}
	if out.Answer == kbNoResultResponse {
		return fmt.Errorf("回答命中固定拒答文案，检索应命中语料: %s", out.Answer)
	}
	k.sm.logf("answer %d 字（knowledgeBaseName=%s）", len([]rune(out.Answer)), out.KnowledgeBaseName)
	return nil
}

// stepNoResult 拒答探测：与语料无关的问题应得到固定拒答文案。
func (k *kbFlow) stepNoResult(ctx context.Context) error {
	raw, err := k.sm.do(ctx, http.MethodPost, "/api/knowledgebase/query", map[string]any{
		"knowledgeBaseIds": []int64{k.kbID},
		"question":         "量子色动力学中胶子的自相互作用如何用威尔逊圈表述？",
	})
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("拒答查询 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var out struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return fmt.Errorf("解析拒答响应失败: %v", err)
	}
	if out.Answer != kbNoResultResponse {
		return fmt.Errorf("期望固定拒答文案，实际: %.80s", out.Answer)
	}
	k.sm.logf("命中固定拒答文案（%d 字）", len([]rune(out.Answer)))
	return nil
}

// stepGenerateQuestions 发起异步出题并轮询 generation-status 至 COMPLETED。
// 题量给足余量（kbMain+2，封顶 6），确认 savedCount ≥ 需求。
// stepGenerateQuestions 发起出题并轮询完成；追问池不足时重新出题吸收模型方差
// （模型偶发不产出 followUps，容量/建场将不可用；重试最多 3 轮，仍不足如实报错）。
func (k *kbFlow) stepGenerateQuestions(ctx context.Context) error {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := k.submitAndPollGeneration(ctx); err != nil {
			return err
		}
		capable, err := k.countFollowUpCapable(ctx)
		if err != nil {
			return err
		}
		if capable >= k.neededQuestions() {
			return nil
		}
		if attempt == maxAttempts {
			return fmt.Errorf("出题 %d 轮后追问池仍不足：带 ≥%d 个追问的题仅 %d 道（需求 %d）",
				attempt, k.sm.opts.kbFollow, capable, k.neededQuestions())
		}
		k.sm.logf("追问池不足（%d/%d），第 %d 轮重新出题吸收模型方差", capable, k.neededQuestions(), attempt+1)
	}
	return nil
}

// countFollowUpCapable 统计追问池满足 followUpCount 档位的题目数（容量/建场的选题门槛）。
func (k *kbFlow) countFollowUpCapable(ctx context.Context) (int, error) {
	raw, err := k.sm.do(ctx, http.MethodGet, "/api/knowledgebase/"+itoa64(k.kbID)+"/questions", nil)
	if err != nil {
		return 0, err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return 0, fmt.Errorf("查询题目列表 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var questions []struct {
		FollowUps []json.RawMessage `json:"followUps"`
	}
	if err := json.Unmarshal(env.Data, &questions); err != nil {
		return 0, fmt.Errorf("解析题目列表失败: %v", err)
	}
	capable := 0
	for _, q := range questions {
		if len(q.FollowUps) >= k.sm.opts.kbFollow {
			capable++
		}
	}
	return capable, nil
}

// submitAndPollGeneration 单轮出题：提交任务并轮询到 COMPLETED/FAILED/超时。
func (k *kbFlow) submitAndPollGeneration(ctx context.Context) error {
	sm := k.sm
	generateCount := k.neededQuestions() + 2
	if generateCount > 6 {
		generateCount = 6
	}
	if err := sm.doJSON(ctx, http.MethodPost,
		"/api/knowledgebase/"+itoa64(k.kbID)+"/questions/generate", map[string]any{
			"difficulty":    sm.opts.difficulty,
			"questionCount": generateCount,
			"followUpCount": sm.opts.kbFollow,
			"categoryLimit": 3,
			"llmProvider":   nullableProvider(sm.opts.provider),
		}, nil); err != nil {
		return err
	}

	deadline := time.Now().Add(sm.opts.evalWait)
	lastProgress := time.Time{}
	for {
		raw, err := sm.do(ctx, http.MethodGet,
			"/api/knowledgebase/"+itoa64(k.kbID)+"/questions/generation-status", nil)
		if err == nil {
			if env, perr := parseEnvelope(raw); perr == nil {
				var status struct {
					Status     string `json:"questionGenStatus"`
					SavedCount int    `json:"savedCount"`
					Skipped    int    `json:"skippedCount"`
					Message    string `json:"message"`
					Error      string `json:"error"`
				}
				if uerr := json.Unmarshal(env.Data, &status); uerr == nil {
					switch status.Status {
					case "COMPLETED":
						if status.SavedCount < k.neededQuestions() {
							return fmt.Errorf("出题完成但 savedCount=%d 不足需求 %d（追问档 %d，skipped=%d，message=%q）",
								status.SavedCount, k.neededQuestions(), sm.opts.kbFollow, status.Skipped, status.Message)
						}
						k.savedCount = status.SavedCount
						sm.logf("questionGenStatus=COMPLETED savedCount=%d skipped=%d message=%q",
							status.SavedCount, status.Skipped, status.Message)
						return nil
					case "FAILED":
						return fmt.Errorf("出题失败 error=%q", status.Error)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("出题超时（%s 内未达 COMPLETED）", sm.opts.evalWait)
		}
		if lastProgress.IsZero() || time.Since(lastProgress) >= progressInterval {
			sm.logf("出题中 …（剩余 %s）", time.Until(deadline).Round(time.Second))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stepActivateQuestions 把生成题目批量启用（DRAFT→ACTIVE，对齐前端"启用"动作：
// 面试选题只取 ACTIVE）。
func (k *kbFlow) stepActivateQuestions(ctx context.Context) error {
	raw, err := k.sm.do(ctx, http.MethodGet,
		"/api/knowledgebase/"+itoa64(k.kbID)+"/questions?status=DRAFT", nil)
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("查询题目列表 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var questions []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(env.Data, &questions); err != nil {
		return fmt.Errorf("解析题目列表失败: %v", err)
	}
	if len(questions) < k.neededQuestions() {
		return fmt.Errorf("DRAFT 题目 %d 少于需求 %d", len(questions), k.neededQuestions())
	}
	for _, q := range questions {
		if err := k.sm.doJSON(ctx, http.MethodPut,
			fmt.Sprintf("/api/knowledgebase/questions/%d/status", q.ID),
			map[string]any{"status": "ACTIVE"}, nil); err != nil {
			return fmt.Errorf("启用题目 %d 失败: %v", q.ID, err)
		}
		k.questionIDs = append(k.questionIDs, q.ID)
	}
	k.sm.logf("已启用 %d 道题（DRAFT→ACTIVE）", len(questions))
	return nil
}

// stepCapacity 面试容量预判：目标难度/主问题数/追问档位必须可选。
func (k *kbFlow) stepCapacity(ctx context.Context) error {
	sm := k.sm
	raw, err := sm.do(ctx, http.MethodGet,
		"/api/knowledgebase/"+itoa64(k.kbID)+fmt.Sprintf("/interview-capacity?difficulty=%s&mainQuestionCount=%d", sm.opts.difficulty, sm.opts.kbMain), nil)
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("容量查询 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var capacity struct {
		KnowledgeBaseID int64  `json:"knowledgeBaseId"`
		Difficulty      string `json:"difficulty"`
		FollowUpOptions []struct {
			FollowUpCount          int  `json:"followUpCount"`
			AvailableQuestionCount int  `json:"availableQuestionCount"`
			Selectable             bool `json:"selectable"`
		} `json:"followUpOptions"`
	}
	if err := json.Unmarshal(env.Data, &capacity); err != nil {
		return fmt.Errorf("解析容量响应失败: %v", err)
	}
	if capacity.Difficulty != sm.opts.difficulty {
		return fmt.Errorf("容量难度回显 = %q, want %q", capacity.Difficulty, sm.opts.difficulty)
	}
	for _, opt := range capacity.FollowUpOptions {
		if opt.FollowUpCount == sm.opts.kbFollow {
			if !opt.Selectable {
				return fmt.Errorf("追问档 %d 不可选（可用 %d < 主问题数 %d），请调大出题量或调低追问档",
					opt.FollowUpCount, opt.AvailableQuestionCount, sm.opts.kbMain)
			}
			k.capacityOK = true
			sm.logf("追问档 %d 可选（可用题 %d ≥ 主问题数 %d）", opt.FollowUpCount, opt.AvailableQuestionCount, sm.opts.kbMain)
			return nil
		}
	}
	return fmt.Errorf("容量响应缺少追问档 %d: %s", sm.opts.kbFollow, string(env.Data))
}

// stepCreateSession 创建知识库面试场次并断言契约：
// knowledgeBaseId/interviewCategory/status=CREATED、题目带参考基线与来源上下文。
func (k *kbFlow) stepCreateSession(ctx context.Context) error {
	sm := k.sm
	var session struct {
		SessionID            string  `json:"sessionId"`
		ResumeText           string  `json:"resumeText"`
		TotalQuestions       int     `json:"totalQuestions"`
		CurrentQuestionIndex int     `json:"currentQuestionIndex"`
		Status               string  `json:"status"`
		KnowledgeBaseID      *int64  `json:"knowledgeBaseId"`
		InterviewCategory    *string `json:"interviewCategory"`
		Questions            []struct {
			QuestionIndex       int      `json:"questionIndex"`
			Question            string   `json:"question"`
			IsFollowUp          bool     `json:"isFollowUp"`
			ParentQuestionIndex *int     `json:"parentQuestionIndex"`
			ReferenceAnswer     *string  `json:"referenceAnswer"`
			KeyPoints           []string `json:"keyPoints"`
			SourceContext       *string  `json:"sourceContext"`
		} `json:"questions"`
	}
	body := map[string]any{
		"knowledgeBaseId":   k.kbID,
		"difficulty":        sm.opts.difficulty,
		"mainQuestionCount": sm.opts.kbMain,
		"followUpCount":     sm.opts.kbFollow,
	}
	if sm.opts.provider != "" {
		body["llmProvider"] = sm.opts.provider
	}
	if err := sm.doJSON(ctx, http.MethodPost, "/api/knowledgebase-interviews/sessions", body, &session); err != nil {
		return err
	}
	if session.SessionID == "" || session.Status != "CREATED" {
		return fmt.Errorf("场次形状不符: sessionId=%q status=%q", session.SessionID, session.Status)
	}
	if session.KnowledgeBaseID == nil || *session.KnowledgeBaseID != k.kbID {
		return fmt.Errorf("knowledgeBaseId 回显不符: %v, want %d", session.KnowledgeBaseID, k.kbID)
	}
	if session.ResumeText != "" {
		return fmt.Errorf("知识库场次 resumeText 应为空串: %q", session.ResumeText)
	}
	if session.TotalQuestions != len(session.Questions) {
		return fmt.Errorf("totalQuestions=%d 与 questions=%d 不一致", session.TotalQuestions, len(session.Questions))
	}
	mainCount := 0
	withReference := 0
	for _, q := range session.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("题干为空的题目: index=%d", q.QuestionIndex)
		}
		if q.SourceContext == nil || strings.TrimSpace(*q.SourceContext) == "" {
			return fmt.Errorf("题目缺少来源上下文 sourceContext: index=%d", q.QuestionIndex)
		}
		if q.IsFollowUp {
			if q.ParentQuestionIndex == nil {
				return fmt.Errorf("追问缺少 parentQuestionIndex: index=%d", q.QuestionIndex)
			}
			continue
		}
		mainCount++
		if q.ReferenceAnswer != nil && strings.TrimSpace(*q.ReferenceAnswer) != "" {
			withReference++
		}
	}
	if mainCount != sm.opts.kbMain {
		return fmt.Errorf("主问题数 = %d, want %d", mainCount, sm.opts.kbMain)
	}
	if withReference == 0 {
		return fmt.Errorf("主问题均未携带参考答案（评估将退化为无基线）")
	}
	k.totalQuestions = session.TotalQuestions
	sm.sessionID = session.SessionID
	sm.totalQuestions = session.TotalQuestions
	sm.logf("sessionId=%s 题目 %d 条（主 %d，参考答案 %d 条，来源上下文全覆盖）",
		session.SessionID, session.TotalQuestions, mainCount, withReference)
	return nil
}

// stepAnswerAll 循环取题并提交模拟作答（末题提交自动交卷）。
func (k *kbFlow) stepAnswerAll(ctx context.Context) error {
	sm := k.sm
	answered := 0
	for {
		var current struct {
			Completed bool `json:"completed"`
			Question  *struct {
				QuestionIndex int `json:"questionIndex"`
			} `json:"question"`
			Message string `json:"message"`
		}
		if err := sm.doJSON(ctx, http.MethodGet,
			"/api/interview/sessions/"+sm.sessionID+"/question", nil, &current); err != nil {
			return err
		}
		if current.Completed {
			break
		}
		if current.Question == nil {
			return fmt.Errorf("未完成但 question 缺失")
		}
		var submit struct {
			HasNextQuestion bool `json:"hasNextQuestion"`
			CurrentIndex    int  `json:"currentIndex"`
			TotalQuestions  int  `json:"totalQuestions"`
		}
		if err := sm.doJSON(ctx, http.MethodPost,
			"/api/interview/sessions/"+sm.sessionID+"/answers", map[string]any{
				"questionIndex": current.Question.QuestionIndex,
				"answer":        cannedAnswer(current.Question.QuestionIndex),
			}, &submit); err != nil {
			return err
		}
		answered++
		if submit.TotalQuestions != k.totalQuestions {
			return fmt.Errorf("totalQuestions=%d, want %d", submit.TotalQuestions, k.totalQuestions)
		}
		if !submit.HasNextQuestion {
			sm.logf("末题提交自动交卷（共 %d 题）", submit.TotalQuestions)
			break
		}
	}
	if answered != k.totalQuestions {
		return fmt.Errorf("提交 %d 题, want %d", answered, k.totalQuestions)
	}
	return nil
}

// stepPollEvaluation 轮询 evaluateStatus 至 COMPLETED（细节端点）。
func (k *kbFlow) stepPollEvaluation(ctx context.Context) error {
	return k.sm.stepPollEvaluation(ctx)
}

// stepReport 报告校验：总分/逐题分数/参考答案齐全（题目自带基线覆盖）。
func (k *kbFlow) stepReport(ctx context.Context) error {
	sm := k.sm
	raw, err := sm.do(ctx, http.MethodGet,
		"/api/interview/sessions/"+sm.sessionID+"/report", nil)
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("报告 %v，响应摘要: %s", err, excerpt(raw, bodyExcerptRunes))
	}
	var report struct {
		SessionID       string `json:"sessionId"`
		TotalQuestions  int    `json:"totalQuestions"`
		OverallScore    int    `json:"overallScore"`
		QuestionDetails []struct {
			QuestionIndex int `json:"questionIndex"`
			Score         int `json:"score"`
		} `json:"questionDetails"`
		ReferenceAnswers []struct {
			QuestionIndex   int      `json:"questionIndex"`
			ReferenceAnswer string   `json:"referenceAnswer"`
			KeyPoints       []string `json:"keyPoints"`
		} `json:"referenceAnswers"`
	}
	if err := json.Unmarshal(env.Data, &report); err != nil {
		return fmt.Errorf("解析报告失败: %v", err)
	}
	if report.SessionID != sm.sessionID {
		return fmt.Errorf("报告 sessionId=%q, want %q", report.SessionID, sm.sessionID)
	}
	if report.TotalQuestions != k.totalQuestions {
		return fmt.Errorf("报告 totalQuestions=%d, want %d", report.TotalQuestions, k.totalQuestions)
	}
	if len(report.QuestionDetails) != k.totalQuestions {
		return fmt.Errorf("逐题明细 %d 条, want %d", len(report.QuestionDetails), k.totalQuestions)
	}
	if len(report.ReferenceAnswers) != k.totalQuestions {
		return fmt.Errorf("参考答案 %d 条, want %d", len(report.ReferenceAnswers), k.totalQuestions)
	}
	nonEmpty := 0
	for _, ref := range report.ReferenceAnswers {
		if strings.TrimSpace(ref.ReferenceAnswer) != "" {
			nonEmpty++
		}
	}
	if nonEmpty == 0 {
		return fmt.Errorf("报告参考答案全为空（题目自带基线未生效）")
	}
	sm.logf("报告总分 %d，参考答案 %d/%d 条非空", report.OverallScore, nonEmpty, k.totalQuestions)
	return nil
}

// stepCleanup 清理场次与知识库（-keep 跳过；知识库删除走事务外删向量与 S3 补偿链）。
func (k *kbFlow) stepCleanup(ctx context.Context) error {
	sm := k.sm
	if sm.opts.keep {
		sm.logf("-keep 指定，保留场次 %s 与知识库 %d", sm.sessionID, k.kbID)
		return nil
	}
	if err := sm.doJSON(ctx, http.MethodDelete,
		"/api/interview/sessions/"+sm.sessionID, nil, nil); err != nil {
		return fmt.Errorf("删除场次失败: %v", err)
	}
	if err := sm.doJSON(ctx, http.MethodDelete,
		"/api/knowledgebase/"+itoa64(k.kbID), nil, nil); err != nil {
		return fmt.Errorf("删除知识库失败: %v", err)
	}
	sm.logf("场次 %s 与知识库 %d 已删除", sm.sessionID, k.kbID)
	return nil
}

// neededQuestions 创建场次所需主问题数（出题量与激活校验共用）。
func (k *kbFlow) neededQuestions() int { return k.sm.opts.kbMain }

// nullableProvider 空串转 JSON null（llmProvider 可选）。
func nullableProvider(p string) any {
	if p == "" {
		return nil
	}
	return p
}

// itoa64 int64 路径参数格式化。
func itoa64(v int64) string { return fmt.Sprintf("%d", v) }
