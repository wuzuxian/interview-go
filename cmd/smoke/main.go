// Command smoke 真实全链路冒烟命令（票 09 文字模式 / 票 10 语音模式 / 票 05 知识库模式）。
//
// 发布前人工验收项（spec Testing Decisions 验收方式 B + 真实冒烟条目）：
//   - text 模式（默认）：走真实 Provider 完成 创建（真实出题）→ 模拟作答 →
//     交卷 → 轮询评估 → 报告校验 → 明细校验 → PDF 导出；
//   - voice 模式：走真实语音链路完成 前置检查 voice_config → 创建语音场次 →
//     WS 开场白 → TTS 开场音频重采样回喂 ASR → 提交 → 字幕/流式文本/音频块
//     断言 → 结束 → 轮询评估 → 报告校验；
//   - knowledgebase 模式：走真 PG/pgvector/Redis/S3/模型服务完成 上传 →
//     向量化 → RAG 查询 → 拒答 → 出题 → 启用 → 容量 → 知识库面试 → 作答
//     交卷 → 轮询评估 → 报告校验。
//
// 每步打印校验点与耗时；任一步失败非零退出并输出可定位信息（步骤名 + 响应摘要）。
//
// 前置条件（-h 查看完整说明）：
//   - 服务已运行（本命令只探测，不负责拉起）；LLM 调用 43-74s，服务需以
//     APP_SERVER_WRITE_TIMEOUT=180s 启动，否则客户端会先超时；
//   - config.yaml 的 app.ai.providers / 数据库 llm_provider_config 有可用真实 Provider
//     （如 dashscope/qwen3.5-flash）；Postgres/Redis 已运行；
//   - voice 模式另需设置页 voice_config 已配置真实 ASR/TTS DashScope Key；
//   - knowledgebase 模式另需 S3（RustFS）已运行且 Embedding/Chat Provider 可用。
//
// 用法：go run ./cmd/smoke [-mode text|voice|knowledgebase] [flags]
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuzuxian/interview/internal/interview"
)

const (
	defaultBaseURL   = "http://127.0.0.1:8080"
	defaultTimeout   = 180 * time.Second // 覆盖单次 LLM 调用 43-74s 的实测区间
	defaultEvalWait  = 5 * time.Minute   // 评估轮询上限（票面：超时 5 分钟）
	pollInterval     = 3 * time.Second
	progressInterval = 15 * time.Second // 轮询进度打印间隔
	bodyExcerptRunes = 300              // 错误信息中的响应摘要长度
)

// options 命令行参数。
type options struct {
	mode          string
	baseURL       string
	timeout       time.Duration
	evalWait      time.Duration
	questionCount int
	skillID       string
	difficulty    string
	provider      string
	outDir        string
	keep          bool
	kbMain        int
	kbFollow      int
}

func main() {
	opts := parseFlags()
	var err error
	switch opts.mode {
	case "text":
		err = run(opts, os.Stdout)
	case "voice":
		err = runVoice(opts, os.Stdout)
	case "knowledgebase":
		err = runKnowledgeBase(opts, os.Stdout)
	default:
		err = fmt.Errorf("参数 -mode 非法：%q（可选 text|voice|knowledgebase）", opts.mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n冒烟失败: %v\n", err)
		os.Exit(1)
	}
	switch opts.mode {
	case "voice":
		fmt.Println("\n冒烟通过：语音面试全链路（前置检查→创建→WS 开场白→TTS 音频重采样回喂 ASR→提交→字幕/流式文本/音频块→结束→评估→报告→清理）全部校验点通过。")
	case "knowledgebase":
		fmt.Println("\n冒烟通过：知识库全链路（上传→向量化→RAG 查询→拒答→出题→启用→容量→知识库面试→作答交卷→评估→报告→清理）全部校验点通过。")
	default:
		fmt.Println("\n冒烟通过：真实 LLM 全链路（创建→出题→作答→交卷→评估→报告→明细→PDF）全部校验点通过。")
	}
}

// parseFlags 解析命令行参数并注册帮助文案（前置条件说明，票面验收项）。
func parseFlags() *options {
	opts := &options{}
	flag.StringVar(&opts.mode, "mode", "text",
		"冒烟模式：text=文字面试全链路（默认）；voice=语音面试全链路（需 voice_config 已配置真实 ASR/TTS Key）；knowledgebase=知识库全链路（需 PG/pgvector/Redis/S3 与真实模型服务）")
	flag.StringVar(&opts.baseURL, "base-url", defaultBaseURL,
		"服务地址（须包含协议，如 http://127.0.0.1:8080）")
	flag.DurationVar(&opts.timeout, "timeout", defaultTimeout,
		"单个 HTTP 请求/WS 等待超时（覆盖出题/报告等 LLM 调用，实测 43-74s；服务端需以 APP_SERVER_WRITE_TIMEOUT=180s 启动）")
	flag.DurationVar(&opts.evalWait, "eval-timeout", defaultEvalWait,
		"评估状态轮询上限（超时判定失败）")
	flag.IntVar(&opts.questionCount, "question-count", 3,
		"主问题数量（3-20；建议 3-4 控制真实 LLM 成本；仅 text 模式使用）")
	flag.StringVar(&opts.skillID, "skill", "java-backend",
		"面试方向 skillId（须为服务端已注册方向，如 java-backend；两种模式共用）")
	flag.StringVar(&opts.difficulty, "difficulty", "mid",
		"难度 junior|mid|senior（两种模式共用）")
	flag.StringVar(&opts.provider, "provider", "",
		"llmProvider（留空使用服务端默认 Provider；两种模式共用）")
	flag.StringVar(&opts.outDir, "out", os.TempDir(),
		"PDF 导出目录（默认系统临时目录；仅 text 模式使用）")
	flag.BoolVar(&opts.keep, "keep", false,
		"保留本次冒烟创建的场次（默认结束时 DELETE 清理；三种模式共用）")
	flag.IntVar(&opts.kbMain, "kb-main", 2,
		"知识库面试主问题数量（1-20；仅 knowledgebase 模式使用）")
	flag.IntVar(&opts.kbFollow, "kb-follow", 1,
		"知识库面试每题追问数量（0-5；仅 knowledgebase 模式使用）")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprint(out, `smoke - 真实全链路冒烟命令（票 09 文字模式 / 票 10 语音模式 / 票 05 知识库模式，发布前人工验收项）

-mode text（默认）文字链路：
  健康检查 → 创建场次（真实出题）→ 题目结构校验 → 模拟作答（含 1 题留空
  覆盖未答题路径）→ 末题自动交卷 → 轮询评估至 COMPLETED → 报告结构校验 →
  问答明细校验 → PDF 导出校验（%PDF 魔数 + 非空）→ DELETE 清理（可用 -keep 跳过）。

-mode voice 语音链路（无麦克风端到端自检）：
  前置检查 voice_config（真实 ASR/TTS Key 缺失即清晰报错）→ 健康检查 →
  创建语音场次 → WS 连接收开场白（welcome / asr_ready / 开场白文本 /
  真实 TTS 开场音频非空）→ 开场音频线性插值重采样 16kHz → 200ms 帧逐帧喂
  ASR（TTS→重采样→ASR 回环）→ 断言字幕非空 → control submit（携带字幕
  修正文本）→ 断言 subtitle(final=true)/text(final=true)/audio_chunk/
  audio_complete → end_interview → 轮询评估至 COMPLETED → 报告必备字段
  校验 → DELETE 清理（可用 -keep 跳过）。

-mode knowledgebase 知识库链路（票 05）：
  健康检查 → 生成语料并 multipart 上传 → 轮询向量化完成（chunkCount>0）→
  RAG 查询（回答非空且不命中拒答）→ 拒答探测（无关问题固定拒答文案）→
  异步出题轮询 COMPLETED（savedCount 达标）→ 批量启用题目（DRAFT→ACTIVE，
  对齐前端"启用"；面试只取 ACTIVE）→ 面试容量预判（目标档位 selectable）→
  创建知识库面试场次（sourceType 语义字段 + 参考答案/评分要点/评分规则/
  来源上下文）→ 模拟作答提交（末题自动交卷）→ 轮询评估至 COMPLETED →
  报告校验（总分/逐题/参考答案齐全）→ DELETE 清理（场次 + 知识库，
  可用 -keep 跳过；参数 -kb-main/-kb-follow）。

每步打印校验点与耗时；任一步失败以非零退出码结束，错误信息含步骤名与响应摘要。

前置条件：
  1. 服务已运行且可达（本命令只探测并提示，不负责拉起服务）：
       APP_SERVER_WRITE_TIMEOUT=180s go run .
     注意：真实 LLM 调用 43-74s，服务必须以 APP_SERVER_WRITE_TIMEOUT=180s 启动，
     否则默认 30s 写超时会在模型返回前断开连接。
  2. 已配置可用真实 Provider：config.yaml 的 app.ai.providers（预设播种）或
     数据库 llm_provider_config 表（如 dashscope/qwen3.5-flash），且 API Key 有效；
     可用 -provider 指定，留空用服务端默认。
  3. Postgres / Redis 已运行（dev 环境默认 127.0.0.1:5432 / 6379）。
  4. （仅 voice 模式）设置页已配置语音服务：voice_config 内 ASR/TTS 参数与
     真实 DashScope API Key（GET /api/llm-provider/voice/asr|tts 可核对）。

用法：
  go run ./cmd/smoke [flags]

参数：
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	return opts
}

// run 文字冒烟主流程（票 09，-mode text）：逐步执行并在失败时返回含步骤名的错误。
func run(opts *options, out io.Writer) error {
	base := strings.TrimRight(opts.baseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("参数 -base-url 非法：%q（须包含 http:// 或 https://）", opts.baseURL)
	}
	if opts.questionCount < 3 || opts.questionCount > 20 {
		return fmt.Errorf("参数 -question-count 非法：%d（须在 3-20）", opts.questionCount)
	}
	client := &http.Client{Timeout: opts.timeout}
	sm := &smoke{client: client, base: base, out: out, opts: opts, stepElapsed: map[string]time.Duration{}}

	fmt.Fprintf(out, "== 真实 LLM 全链路冒烟（票 09）==\n服务: %s  题量: %d  方向: %s  难度: %s  超时: %s\n\n",
		base, opts.questionCount, opts.skillID, opts.difficulty, opts.timeout)

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"健康检查", sm.stepHealth},
		{"创建面试场次（真实出题）", sm.stepCreate},
		{"题目结构校验", sm.stepValidateQuestions},
		{"模拟作答", sm.stepAnswer},
		{"交卷", sm.stepComplete},
		{"轮询评估状态", sm.stepPollEvaluation},
		{"报告结构校验", sm.stepReport},
		{"问答明细校验", sm.stepDetails},
		{"PDF 导出校验", sm.stepExportPDF},
		{"清理场次", sm.stepCleanup},
	}
	return executeSteps(context.Background(), out, sm, steps)
}

// executeSteps 步骤化执行框架（text/voice 两种模式共用）：逐步打印、失败即停、
// 通过后汇总各步耗时。
func executeSteps(ctx context.Context, out io.Writer, sm *smoke,
	steps []struct {
		name string
		fn   func(context.Context) error
	}) error {
	for i, s := range steps {
		fmt.Fprintf(out, "[%d/%d] %s ...\n", i+1, len(steps), s.name)
		start := time.Now()
		err := s.fn(ctx)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Fprintf(out, "    ✗ 失败（耗时 %s）\n", elapsed)
			return fmt.Errorf("步骤 [%s] 失败（耗时 %s）: %w", s.name, elapsed, err)
		}
		sm.stepElapsed[s.name] = elapsed
		fmt.Fprintf(out, "    ✓ 通过（耗时 %s）\n\n", elapsed)
	}
	fmt.Fprintln(out, "== 各步耗时汇总 ==")
	for i, s := range steps {
		fmt.Fprintf(out, "  [%d/%d] %-28s %s\n", i+1, len(steps), s.name, sm.stepElapsed[s.name])
	}
	return nil
}

// smoke 冒烟流程状态与 HTTP 封装。
type smoke struct {
	client      *http.Client
	base        string
	out         io.Writer
	opts        *options
	stepElapsed map[string]time.Duration

	sessionID      string
	totalQuestions int
	skippedIndex   int  // 模拟作答时留空的题号（覆盖未答题路径）
	autoCompleted  bool // 末题提交是否已自动交卷
}

func (sm *smoke) logf(format string, args ...any) {
	fmt.Fprintf(sm.out, "    · "+format+"\n", args...)
}

// ===== HTTP 封装 =====

// do 发送请求并读取 body；传输层错误与非 2xx 状态都附响应摘要。
func (sm *smoke) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, sm.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := sm.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s %s 失败（服务未运行？前置条件见 -h）: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 %s %s 响应失败: %v", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s 返回 HTTP %d，响应摘要: %s",
			method, path, resp.StatusCode, excerpt(raw, bodyExcerptRunes))
	}
	return raw, nil
}

// doJSON 发送请求并解包 Result{code,message,data}（业务错误 code!=200 视为失败）。
func (sm *smoke) doJSON(ctx context.Context, method, path string, body any, out any) error {
	raw, err := sm.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("%s %s %v，响应摘要: %s", method, path, err, excerpt(raw, bodyExcerptRunes))
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("解析 %s %s 响应 data 失败: %v", method, path, err)
		}
	}
	return nil
}

// ===== 各步骤 =====

// stepHealth 健康检查（票面步骤 a）：GET /ping，404 时回退 /api 前缀探测。
func (sm *smoke) stepHealth(ctx context.Context) error {
	raw, err := sm.do(ctx, http.MethodGet, "/ping", nil)
	if err == nil {
		if env, perr := parseEnvelope(raw); perr == nil {
			sm.logf("GET /ping → code=%d", env.Code)
			return nil
		}
	}
	// /ping 不可用（老版本路由/前缀差异）：回退 /api 探测。
	raw2, err2 := sm.do(ctx, http.MethodGet, "/api/interview/skills", nil)
	if err2 != nil {
		return fmt.Errorf("服务不可达（/ping 与 /api 探测均失败）；请确认服务已启动且地址正确（-base-url %s）；"+
			"/ping 错误: %v；/api 探测错误: %v", sm.opts.baseURL, err, err2)
	}
	env, perr := parseEnvelope(raw2)
	if perr != nil {
		return fmt.Errorf("/api 探测响应异常: %v，响应摘要: %s", perr, excerpt(raw2, bodyExcerptRunes))
	}
	sm.logf("GET /api/interview/skills → code=%d（/ping 不可用，/api 前缀探测通过）", env.Code)
	return nil
}

// stepCreate 创建面试场次（票面步骤 b）：真实出题、幂等 requestId、无简历。
func (sm *smoke) stepCreate(ctx context.Context) error {
	req := interview.CreateRequest{
		QuestionCount: sm.opts.questionCount,
		SkillID:       &sm.opts.skillID,
		Difficulty:    &sm.opts.difficulty,
		RequestID:     newRequestID(),
	}
	if sm.opts.provider != "" {
		req.LLMProvider = &sm.opts.provider
	}
	var sess interview.SessionDTO
	if err := sm.doJSON(ctx, http.MethodPost, "/api/interview/sessions", req, &sess); err != nil {
		return err
	}
	sm.sessionID = sess.SessionID
	sm.totalQuestions = sess.TotalQuestions
	sm.logf("sessionId=%s requestId=%s 总题数=%d（含追问）status=%s",
		sess.SessionID, req.RequestID, sess.TotalQuestions, sess.Status)
	return nil
}

// stepValidateQuestions 题目结构校验（票面步骤 c）。
func (sm *smoke) stepValidateQuestions(ctx context.Context) error {
	var sess interview.SessionDTO
	if err := sm.doJSON(ctx, http.MethodGet, "/api/interview/sessions/"+sm.sessionID, nil, &sess); err != nil {
		return err
	}
	if err := validateSession(&sess, sm.opts.questionCount); err != nil {
		return err
	}
	mains, followUps := 0, 0
	for _, q := range sess.Questions {
		if q.IsFollowUp {
			followUps++
		} else {
			mains++
		}
	}
	sm.totalQuestions = len(sess.Questions)
	sm.logf("主问题=%d（请求 %d）追问=%d，question/category/topicSummary/追问标记齐全",
		mains, sm.opts.questionCount, followUps)
	return nil
}

// stepAnswer 模拟作答（票面步骤 d）：逐题 POST 提交简短真实答案，
// 有意识留空一题（非末题）覆盖未答题路径（评估按 0 分计，链路应照样跑通）。
func (sm *smoke) stepAnswer(ctx context.Context) error {
	total := sm.totalQuestions
	sm.skippedIndex = total / 2 // 中间某题留空；保证不是末题，末题提交仍可触发自动交卷
	if sm.skippedIndex == total-1 {
		sm.skippedIndex = total - 2
	}
	for i := 0; i < total; i++ {
		if i == sm.skippedIndex {
			sm.logf("第 %d/%d 题留空不作答（覆盖未答题路径）", i+1, total)
			continue
		}
		body := map[string]any{"questionIndex": i, "answer": cannedAnswer(i)}
		var resp interview.SubmitAnswerResponse
		if err := sm.doJSON(ctx, http.MethodPost,
			"/api/interview/sessions/"+sm.sessionID+"/answers", body, &resp); err != nil {
			return fmt.Errorf("提交第 %d 题答案失败: %w", i, err)
		}
		if i == total-1 {
			sm.autoCompleted = !resp.HasNextQuestion
		}
		sm.logf("第 %d/%d 题已提交（hasNext=%v currentIndex=%d）", i+1, total, resp.HasNextQuestion, resp.CurrentIndex)
	}
	return nil
}

// stepComplete 交卷（票面步骤 e）：末题提交已自动交卷则跳过手动交卷。
func (sm *smoke) stepComplete(ctx context.Context) error {
	if sm.autoCompleted {
		sm.logf("末题提交已自动交卷，跳过手动交卷")
		return nil
	}
	if err := sm.doJSON(ctx, http.MethodPost,
		"/api/interview/sessions/"+sm.sessionID+"/complete", nil, nil); err != nil {
		return err
	}
	sm.logf("已手动交卷（末题未触发自动交卷）")
	return nil
}

// stepPollEvaluation 轮询评估状态至 COMPLETED（票面步骤 f）：
// 超时 eval-timeout（默认 5 分钟），每 15s 打印进度；FAILED 判定失败并透出评估错误。
func (sm *smoke) stepPollEvaluation(ctx context.Context) error {
	deadline := time.Now().Add(sm.opts.evalWait)
	lastProgress := time.Time{}
	for {
		var detail interview.InterviewDetailDTO
		if err := sm.doJSON(ctx, http.MethodGet,
			"/api/interview/sessions/"+sm.sessionID+"/details", nil, &detail); err != nil {
			return err
		}
		status := ""
		if detail.EvaluateStatus != nil {
			status = *detail.EvaluateStatus
		}
		switch status {
		case "COMPLETED":
			sm.logf("evaluateStatus=COMPLETED（评估完成）")
			return nil
		case "FAILED":
			errMsg := ""
			if detail.EvaluateError != nil {
				errMsg = *detail.EvaluateError
			}
			return fmt.Errorf("评估失败 evaluateStatus=FAILED, evaluateError=%q", errMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("评估超时（%s 内未达 COMPLETED，最后状态=%q）", sm.opts.evalWait, orNone(status))
		}
		if lastProgress.IsZero() || time.Since(lastProgress) >= progressInterval {
			sm.logf("evaluateStatus=%s …（剩余 %s）", orNone(status),
				time.Until(deadline).Round(time.Second))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stepReport 拉取报告并校验结构（票面步骤 g）。
func (sm *smoke) stepReport(ctx context.Context) error {
	var rep interview.ReportDTO
	if err := sm.doJSON(ctx, http.MethodGet,
		"/api/interview/sessions/"+sm.sessionID+"/report", nil, &rep); err != nil {
		return err
	}
	if err := validateReport(&rep); err != nil {
		return err
	}
	for _, w := range reportWarnings(&rep) {
		sm.logf("告警: %s", w)
	}
	cats := make([]string, 0, len(rep.CategoryScores))
	for _, cs := range rep.CategoryScores {
		cats = append(cats, fmt.Sprintf("%s=%d(%d题)", cs.Category, cs.Score, cs.QuestionCount))
	}
	sm.logf("overallScore=%d（0-100）分类得分: %s", rep.OverallScore, strings.Join(cats, " "))
	sm.logf("逐题 feedback 非空（%d 题）、referenceAnswers %d 条", len(rep.QuestionDetails), len(rep.ReferenceAnswers))
	return nil
}

// stepDetails 拉取明细并校验问答明细（票面步骤 h）。
func (sm *smoke) stepDetails(ctx context.Context) error {
	var detail interview.InterviewDetailDTO
	if err := sm.doJSON(ctx, http.MethodGet,
		"/api/interview/sessions/"+sm.sessionID+"/details", nil, &detail); err != nil {
		return err
	}
	if err := validateDetails(&detail, sm.sessionID, sm.totalQuestions, sm.skippedIndex); err != nil {
		return err
	}
	answered := 0
	for i, a := range detail.Answers {
		if i != sm.skippedIndex && a.UserAnswer != nil {
			answered++
		}
	}
	sm.logf("问答明细 %d 条：已作答 %d、未作答 1（第 %d 题），逐题 feedback 非空",
		len(detail.Answers), answered, sm.skippedIndex+1)
	return nil
}

// stepExportPDF 导出 PDF 并校验（票面步骤 i）：%PDF 魔数 + 非空 + %%EOF，
// 保存到 -out 目录。
func (sm *smoke) stepExportPDF(ctx context.Context) error {
	raw, err := sm.do(ctx, http.MethodGet,
		"/api/interview/sessions/"+sm.sessionID+"/export", nil)
	if err != nil {
		return err
	}
	// 导出失败按裁决 #10 返回 Result 包装（JSON），此处识别并透出业务错误。
	if len(raw) > 0 && raw[0] == '{' {
		if env, perr := parseEnvelope(raw); perr == nil {
			return fmt.Errorf("导出返回业务错误 code=%d message=%q", env.Code, env.Message)
		}
	}
	if err := validatePDF(raw); err != nil {
		return err
	}
	path := filepath.Join(sm.opts.outDir, "smoke-report_"+sm.sessionID+".pdf")
	if err := os.MkdirAll(sm.opts.outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("保存 PDF 失败: %v", err)
	}
	sm.logf("PDF 合法（%%PDF 魔数 + %%EOF）大小=%dB 已保存: %s", len(raw), path)
	return nil
}

// stepCleanup 清理本次冒烟创建的场次（票面步骤 j，可选加分）：
// 只删除本场次，-keep 跳过；删除失败按裁决 #5 的非幂等语义不重试、判定失败。
func (sm *smoke) stepCleanup(ctx context.Context) error {
	if sm.opts.keep {
		sm.logf("-keep 指定，保留场次 %s", sm.sessionID)
		return nil
	}
	if err := sm.doJSON(ctx, http.MethodDelete,
		"/api/interview/sessions/"+sm.sessionID, nil, nil); err != nil {
		return err
	}
	sm.logf("场次 %s 已删除", sm.sessionID)
	return nil
}

// ===== 小工具 =====

// newRequestID 生成幂等键：[A-Za-z0-9_-]{8,64}（服务端 requestIdPattern）。
func newRequestID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "smoke-" + time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(buf)
}

// cannedAnswer 模拟作答的简短真实答案（避免"不知道"式 0 分答案；
// 内容通用但有信息量，保证评估链路拿到有效输入）。
func cannedAnswer(index int) string {
	answers := []string{
		"我在项目里用过连接池管理数据库连接，配合预编译语句避免注入，同时用 context 控制查询超时，防止慢查询拖垮服务。",
		"我会先用压测定位瓶颈，再从索引优化、缓存热点数据、异步削峰三个方向改进，并用监控指标验证效果。",
		"排查线上问题时我先看日志与监控定位范围，用链路追踪缩小到具体服务，再结合代码变更做根因分析并沉淀复盘文档。",
		"设计接口时我会明确入参校验、错误码约定和幂等语义，保证向前兼容，并为关键路径补充单元测试与集成测试。",
		"并发场景我用互斥锁或原子操作保护共享状态，尽量通过 channel 传递数据减少共享，配合压力测试验证竞态安全。",
		"我通常按业务边界拆分服务，用消息队列解耦非核心链路，核心链路保持同步强一致，并为失败场景设计重试与补偿。",
	}
	return answers[index%len(answers)]
}

// orNone 空状态显示占位符（进度打印用）。
func orNone(s string) string {
	if s == "" {
		return "(未设置)"
	}
	return s
}
