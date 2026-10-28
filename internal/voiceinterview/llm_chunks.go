// 分句音频有序发射器（Java OrderedTtsChunkEmitter L983-1097 同构）：LLM 流式出句
// 期间每句立即并发 TTS（信号量限流），发射协程按句序等待结果——成功且非空下发
// audio_chunk(index, isLast=false)（Java sendAudioChunk 调用逐字：isLast 恒 false，
// 完成信号由 audio_complete 承担）；单句失败/超时跳过不阻塞后续句；全部处理完且
// 至少成功一句才发 audio_complete；零成功静默返回，由管道整段兜底。
package voiceinterview

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ttsEmitterParams 发射器参数。
type ttsEmitterParams struct {
	SessionID      int64
	Ctx            context.Context // 合成请求的取消源（断连/回合取消随管道 ctx 失效）；nil 按 Background
	Speaker        TurnSpeaker
	TTS            *TtsClient
	Settings       TtsSettings
	Timeout        time.Duration // 单句合成等待（Java future.get(ttsTimeoutSec)；max(5s, 配置) 由调用方钳制）
	MaxConcurrency int           // 逐句 TTS 并发上限（Java Semaphore(maxConcurrentTtsPerSession)）
	Logger         *slog.Logger
}

// ttsJob 一个句子的合成任务：句序 + 结果通道（容量 1，合成协程只写一次）。
type ttsJob struct {
	index   int
	outcome chan ttsOutcome
}

// ttsOutcome 单句合成结果。
type ttsOutcome struct {
	pcm []byte
	err error
}

// ttsChunkEmitter 有序分句发射器。
type ttsChunkEmitter struct {
	p    ttsEmitterParams
	ctx  context.Context
	sem  chan struct{} // 并发信号量（acquire 在 Submit 调用线程，向 LLM 流施加背压）
	jobs chan ttsJob   // 句子按提交序进入
	done chan int      // 发射协程结束：emitted 数

	seqMu        sync.Mutex
	seq          int          // 已分配句序（Java AtomicInteger nextIndex）
	emitted      atomic.Int64 // 已成功下发块数（整体超时路径读取）
	completeOnce sync.Once
	stopOnce     sync.Once
}

// newTTSChunkEmitter 创建发射器并启动发射协程（Java 构造器内
// CompletableFuture.supplyAsync(this::drainChunks)）。ctx 用于合成请求的取消
// （断连/回合取消时随管道 ctx 一起失效）。
func newTTSChunkEmitter(p ttsEmitterParams) *ttsChunkEmitter {
	if p.MaxConcurrency < 1 {
		p.MaxConcurrency = 1
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	e := &ttsChunkEmitter{
		p:    p,
		ctx:  ctx,
		sem:  make(chan struct{}, p.MaxConcurrency),
		jobs: make(chan ttsJob, 64),
		done: make(chan int, 1),
	}
	go e.drain()
	return e
}

// Submit 提交一句：分配句序（入队与句序号在同一锁内，保证 jobs 序 = 句序）→
// 信号量限流（阻塞至有空位，对齐 Java acquireUninterruptibly 在提交线程获取）
// → 并发合成。
func (e *ttsChunkEmitter) Submit(sentence string) {
	e.sem <- struct{}{}

	e.seqMu.Lock()
	job := ttsJob{index: e.seq, outcome: make(chan ttsOutcome, 1)}
	e.seq++
	e.jobs <- job
	e.seqMu.Unlock()

	go func() {
		defer func() { <-e.sem }()
		pcm, err := e.p.TTS.Synthesize(e.ctx, e.p.Settings, sentence)
		job.outcome <- ttsOutcome{pcm: pcm, err: err}
	}()
}

// Finish 句子提交完毕（Java finish：totalChunks = nextIndex 并唤醒发射协程；
// channel close 语义等价）。
func (e *ttsChunkEmitter) Finish() {
	e.stopOnce.Do(func() { close(e.jobs) })
}

// AwaitCompletion 等待全部句处理完成，返回成功下发块数；整体超时（Java
// awaitCompletion：max(ttsTimeoutSec+2, (ttsTimeoutSec+1)*max(1, 句数))）时
// 放弃等待，已有下发则补发 audio_complete 后返回当前计数。
func (e *ttsChunkEmitter) AwaitCompletion() int {
	select {
	case n := <-e.done:
		return n
	case <-time.After(e.overallTimeout()):
		n := int(e.emitted.Load())
		if n > 0 {
			e.sendCompleteOnce()
		}
		return n
	}
}

// overallTimeout 整体等待上界（Java awaitCompletion 的 timeoutSec 逐字）。
func (e *ttsChunkEmitter) overallTimeout() time.Duration {
	e.seqMu.Lock()
	n := e.seq
	e.seqMu.Unlock()
	if n < 1 {
		n = 1
	}
	d := (e.p.Timeout + time.Second) * time.Duration(n)
	if min := e.p.Timeout + 2*time.Second; d < min {
		d = min
	}
	return d
}

// drain 发射协程（Java drainChunks 逐句消费）：按句序等待结果，成功非空下发，
// 失败/超时/空音频跳过；全部处理完且 emitted > 0 才发 audio_complete（Java 逐字），
// 最后回传 emitted。
func (e *ttsChunkEmitter) drain() {
	emitted := 0
	for job := range e.jobs {
		var out ttsOutcome
		select {
		case out = <-job.outcome:
		case <-time.After(e.p.Timeout):
			e.logSkip(job.index, "等待合成超时")
			continue
		}
		if out.err != nil {
			e.logSkip(job.index, out.err.Error())
			continue
		}
		if len(out.pcm) == 0 {
			e.logSkip(job.index, "合成返回空音频")
			continue
		}
		e.p.Speaker.SendAudioChunk(PcmToWav(out.pcm), job.index, false)
		emitted++
		e.emitted.Store(int64(emitted))
	}
	if emitted > 0 {
		e.sendCompleteOnce()
	}
	e.done <- emitted
}

func (e *ttsChunkEmitter) sendCompleteOnce() {
	e.completeOnce.Do(func() { e.p.Speaker.SendAudioComplete() })
}

func (e *ttsChunkEmitter) logSkip(index int, reason string) {
	e.p.Logger.Warn("分句 TTS 失败，跳过该句",
		slog.Int64("sessionId", e.p.SessionID), slog.Int("index", index), slog.String("reason", reason))
}
