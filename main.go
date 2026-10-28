package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	einoembedding "github.com/cloudwego/eino-ext/components/embedding/openai"
	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/config"
	ginserver "github.com/wuzuxian/interview/core/gin"
	gormdb "github.com/wuzuxian/interview/core/gorm"
	logx "github.com/wuzuxian/interview/core/log"
	redisx "github.com/wuzuxian/interview/core/redis"
	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/aibridge"
	"github.com/wuzuxian/interview/internal/httpx"
	"github.com/wuzuxian/interview/internal/interview"
	"github.com/wuzuxian/interview/internal/llmprovider"
	"github.com/wuzuxian/interview/internal/migration"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
	"github.com/wuzuxian/interview/internal/resume"
	"github.com/wuzuxian/interview/internal/storage"
	"github.com/wuzuxian/interview/internal/vector"
	"github.com/wuzuxian/interview/internal/voiceinterview"
)

// AppConfig 业务配置（app.* 段），键与 Java 版 application.yml 对齐。
type AppConfig struct {
	AI struct {
		DefaultProvider                            string                              `config:"default_provider" default:"dashscope"`
		DefaultEmbeddingProvider                   string                              `config:"default_embedding_provider" default:"dashscope"`
		EmbeddingDimensions                        int                                 `config:"embedding_dimensions" default:"1024"`
		EncryptionKey                              string                              `config:"api_key_encryption_key"`
		AllowFallbackKey                           bool                                `config:"allow_fallback_encryption_key" default:"false"`
		StructuredMaxAttempts                      int                                 `config:"structured_max_attempts" default:"2"`
		StructuredIncludeLastError                 bool                                `config:"structured_include_last_error" default:"true"`
		StructuredRetryUseRepairPrompt             bool                                `config:"structured_retry_use_repair_prompt" default:"true"`
		StructuredRetryAppendStrictJSONInstruction bool                                `config:"structured_retry_append_strict_json_instruction" default:"true"`
		StructuredErrorMessageMaxLength            int                                 `config:"structured_error_message_max_length" default:"200"`
		LLMCallTimeout                             time.Duration                       `config:"llm_call_timeout" default:"120s"`
		Providers                                  map[string]llmprovider.SeedProvider `config:"providers"`
	} `config:"ai"`
	Resume struct {
		AllowedTypes []string `config:"allowed_types"`
		MaxFileSize  int64    `config:"max_file_size" default:"10485760"`
	} `config:"resume"`
	// Interview 模拟面试配置段（app.interview），结构定义在 interview 包内与业务同处。
	Interview interview.Config `config:"interview"`
	// VoiceInterview 语音面试配置段（app.voice_interview），结构定义在
	// voiceinterview 包内；默认值随票在 config.go 的 default 标签里逐票补齐。
	VoiceInterview voiceinterview.Config `config:"voice_interview"`
	Storage        struct {
		Endpoint         string `config:"endpoint" default:"http://localhost:9000"`
		AccessKey        string `config:"access_key"`
		SecretKey        string `config:"secret_key"`
		Bucket           string `config:"bucket" default:"interview-guide"`
		Region           string `config:"region" default:"us-east-1"`
		AutoCreateBucket bool   `config:"auto_create_bucket" default:"true"`
	} `config:"storage"`
	CORS struct {
		AllowedOrigins []string `config:"allowed_origins"`
	} `config:"cors"`
	Voice struct {
		ASRAPIKey string `config:"asr_api_key"`
	} `config:"voice"`
	Async struct {
		Recovery struct {
			Enabled             bool          `config:"enabled" default:"true"`
			PendingThreshold    time.Duration `config:"pending_threshold" default:"10m"`
			ProcessingThreshold time.Duration `config:"processing_threshold" default:"15m"`
			Interval            time.Duration `config:"interval" default:"1m"`
			BatchSize           int           `config:"batch_size" default:"10"`
			MaxRecoveryCount    int           `config:"max_recovery_count" default:"3"`
		} `config:"recovery"`
	} `config:"async"`
}

func main() {
	// 1. 配置与日志。
	cfg := config.MustLoad(config.Options{
		Files:        []string{"config.yaml"},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})
	logger := logx.NewFromConfig(cfg, "")
	defer logger.Close()
	logx.SetAsDefault(logger)
	slogger := logger.Logger // 业务层统一使用 *slog.Logger

	var appCfg AppConfig
	if err := cfg.UnmarshalKey("app", &appCfg); err != nil {
		logger.Error("解析 app.* 配置失败", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// 2. 基础设施连接。
	ctx := context.Background()
	db := mustPostgres(cfg, slogger)
	defer db.Close()
	rdb := mustRedis(cfg, slogger)
	defer rdb.Close()

	// 3. 幂等迁移 + AI 基础设施。
	if err := migration.Run(ctx, db.DB, slogger); err != nil {
		logger.Error("数据库迁移失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	crypto, err := llmprovider.NewCryptoService(appCfg.AI.EncryptionKey, appCfg.AI.AllowFallbackKey)
	if err != nil {
		logger.Error("初始化 Provider API Key 加密失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slogger.Info("Provider API Key 加密已初始化", slog.Bool("configured_key", appCfg.AI.EncryptionKey != ""),
		slog.Int("seed_providers", len(appCfg.AI.Providers)))

	// Provider 引导（播种 + 全局默认 + 语音配置）。
	bootstrap := llmprovider.NewBootstrap(db.DB, crypto, slogger)
	bootstrap.DefaultChatProvider = appCfg.AI.DefaultProvider
	bootstrap.DefaultEmbeddingProvider = appCfg.AI.DefaultEmbeddingProvider
	bootstrap.DefaultEmbeddingDimension = appCfg.AI.EmbeddingDimensions
	bootstrap.SeedProviders = appCfg.AI.Providers
	bootstrap.SeedVoiceAPIKey = appCfg.Voice.ASRAPIKey
	if err := bootstrap.Run(); err != nil {
		logger.Error("Provider 引导失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	_ = migration.EnsureVoiceConfigColumns(ctx, db.DB, slogger)

	registry, err := ai.NewRegistry(db.DB, slogger)
	if err != nil {
		slogger.Error("初始化 LLM Provider 注册表失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	structuredCfg := ai.DefaultStructuredConfig()
	if appCfg.AI.StructuredMaxAttempts > 0 {
		structuredCfg.MaxAttempts = appCfg.AI.StructuredMaxAttempts
	}
	structuredCfg.IncludeLastError = appCfg.AI.StructuredIncludeLastError
	structuredCfg.RetryUseRepairPrompt = appCfg.AI.StructuredRetryUseRepairPrompt
	structuredCfg.AppendStrictJSONInstruction = appCfg.AI.StructuredRetryAppendStrictJSONInstruction
	if appCfg.AI.StructuredErrorMessageMaxLength > 0 {
		structuredCfg.ErrorMessageMaxLength = appCfg.AI.StructuredErrorMessageMaxLength
	}
	invoker := ai.NewStructuredInvoker(structuredCfg, slogger)

	bridge := aibridge.New(registry, crypto, appCfg.AI.LLMCallTimeout, slogger)

	// pgvector 向量存储。
	vecStore := vector.NewStore(db.DB, func(ctx context.Context) (*einoembedding.Embedder, error) {
		return bridge.DefaultEmbedder(ctx)
	}, appCfg.AI.EmbeddingDimensions, slogger)
	if err := vecStore.EnsureSchema(ctx); err != nil {
		logger.Warn("pgvector schema 初始化失败（向量能力降级）", slog.String("error", err.Error()))
	}

	// 文件存储。
	storeSvc, err := storage.New(ctx, storage.Config{
		Endpoint:         appCfg.Storage.Endpoint,
		AccessKey:        appCfg.Storage.AccessKey,
		SecretKey:        appCfg.Storage.SecretKey,
		Bucket:           appCfg.Storage.Bucket,
		Region:           appCfg.Storage.Region,
		AutoCreateBucket: appCfg.Storage.AutoCreateBucket,
	}, slogger)
	if err != nil {
		logger.Error("初始化文件存储失败", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// 4. 业务模块组装。
	rl := ratelimit.New(rdb, slogger)
	resumeRepo := resume.NewRepo(db.DB)
	producer := resume.NewStreamProducer(rdb, resumeRepo, slogger)
	grading := resume.NewGradingService(bridge, invoker, slogger)
	history := resume.NewHistoryService(resumeRepo, slogger)
	uploader := resume.NewUploader(resumeRepo, storeSvc, producer, history, slogger,
		appCfg.Resume.AllowedTypes, appCfg.Resume.MaxFileSize)
	deleter := resume.NewDeleteService(resumeRepo, storeSvc, vecStore, slogger)
	consumer := resume.NewConsumer(rdb.Client, resumeRepo, grading, history, uploader, vecStore, slogger)
	consumer.Start(ctx)
	defer consumer.Stop()

	recoveryCfg := resume.RecoveryConfig{
		Enabled:             appCfg.Async.Recovery.Enabled,
		PendingThreshold:    appCfg.Async.Recovery.PendingThreshold,
		ProcessingThreshold: appCfg.Async.Recovery.ProcessingThreshold,
		Interval:            appCfg.Async.Recovery.Interval,
		BatchSize:           appCfg.Async.Recovery.BatchSize,
		MaxRecoveryCount:    appCfg.Async.Recovery.MaxRecoveryCount,
	}
	recovery := resume.NewRecoveryScheduler(resumeRepo, producer, recoveryCfg, slogger)
	recovery.Start(ctx)
	defer recovery.Stop()

	providerSvc := llmprovider.NewService(db.DB, crypto, registry, slogger, appCfg.AI.EmbeddingDimensions)
	providerCtl := llmprovider.NewController(providerSvc, rl)
	resumeCtl := resume.NewController(uploader, deleter, history, storeSvc, rl)

	// 面试模块：启动时解析嵌入的 skills 资源，构建方向注册表；
	// LLM 调用经 ModelResolver 接缝由 *aibridge.Bridge 注入（spec 决策 3，唯一新增接缝）。
	interviewSvc, err := interview.NewService(bridge, invoker, appCfg.Interview, slogger)
	if err != nil {
		logger.Error("初始化面试 Skill 注册表失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	// 出题服务（票 07）：双路并行真实出题，LLM 调用经 ModelResolver（超时走 llm_call_timeout）。
	questionSvc := interview.NewQuestionService(interviewSvc, bridge, invoker, appCfg.Interview.FollowUpCount, slogger)
	// 场次子系统（票 03/04）：repo + Redis 缓存/创建锁 + 服务（rdb.Client 为内嵌原生客户端）。
	sessionCache := interview.NewSessionCache(rdb.Client, appCfg.Interview, slogger)
	sessionRepo := interview.NewRepo(db.DB, appCfg.Interview)

	// 评估链路（票 06）：Redis Stream 评估队列——生产者即 EvaluateEnqueuer 扩展点
	// （末题交卷/提前交卷入队），消费者 + 恢复调度器照 resume 模块的接线方式启停。
	evalStream := interview.NewRedisEvalStream(rdb.Client)
	evalTracker := interview.NewRedisEvalInflight(rdb.Client)
	evalProducer := interview.NewEvaluateStreamProducer(evalStream, evalTracker, sessionRepo, slogger)
	sessionSvc := interview.NewSessionService(interview.SessionServiceDeps{
		Repo:      sessionRepo,
		Cache:     sessionCache,
		Skills:    interviewSvc,
		Questions: questionSvc,
		Cfg:       appCfg.Interview,
		Logger:    slogger,
		Enqueuer:  evalProducer,
	})
	evalEngine := interview.NewEvaluationEngine(appCfg.Interview.Evaluation.EngineConfig(), slogger)
	evalRunner := interview.NewEngineRunner(evalEngine, invoker, bridge, slogger)
	evalConsumer := interview.NewEvaluateConsumer(evalStream, evalTracker, sessionRepo, evalRunner, interviewSvc, slogger)
	evalConsumer.Start(ctx)
	defer evalConsumer.Stop()
	evalRecovery := interview.NewEvalRecoveryScheduler(evalTracker, sessionRepo, evalStream, interview.EvalRecoveryConfig{
		Enabled:             appCfg.Async.Recovery.Enabled,
		PendingThreshold:    appCfg.Async.Recovery.PendingThreshold,
		ProcessingThreshold: appCfg.Async.Recovery.ProcessingThreshold,
		Interval:            appCfg.Async.Recovery.Interval,
		BatchSize:           appCfg.Async.Recovery.BatchSize,
	}, slogger)
	evalRecovery.Start(ctx)
	defer evalRecovery.Stop()

	// 报告/明细节拍（票 06）：存量报告返回、同步兜底评估、明细组装。
	reportSvc := interview.NewReportService(sessionRepo, evalRunner, interviewSvc, sessionCache, slogger)
	interviewCtl := interview.NewController(interviewSvc, sessionSvc, reportSvc, rl, appCfg.Interview.RateLimit)

	// 语音面试模块（票 01：会话域 + REST 契约。票 04：WS 网关——每连接读最新语音
	// 配置，设置页修改热生效；模型回合挂钩留空，内置空实现，票 06 接入真实 LLM+TTS
	// 管道。票 08：评估链路——评估服务复用文字面试的 evalRunner
	// （app.interview.evaluation 引擎配置与降级接缝共用）与 interviewSvc 的方向评估
	// 参考段；repo + Redis 会话缓存 + 评估流读写 + 消费侧与恢复调度已接入。
	// 票 09：暂停超时巡检（30s，4:30 预警/5:00 暂停断连）与僵尸清理
	// （并入 60s 恢复调度器，IN_PROGRESS 超 2h 强制结束）已接入；优雅关闭时
	// voiceGateway.Close() 关闭全部活动连接，走断开自动结束路径）。
	voiceRepo := voiceinterview.NewRepo(db.DB)
	voiceCache := voiceinterview.NewSessionCache(rdb.Client, appCfg.VoiceInterview, slogger)
	voiceStream := voiceinterview.NewRedisEvalStream(rdb.Client)
	voiceEvalSvc := voiceinterview.NewEvaluationService(voiceinterview.EvaluationServiceDeps{
		Store:  voiceRepo,
		Eval:   evalRunner,
		Refs:   interviewSvc,
		Logger: slogger,
	})
	voiceSvc := voiceinterview.NewService(voiceinterview.ServiceDeps{
		Repo:       voiceRepo,
		Cache:      voiceCache,
		Stream:     voiceStream,
		Logger:     slogger,
		EvalDetail: voiceEvalSvc,
		Recovery: voiceinterview.EvalRecoveryParams{
			PendingRequeueAfter:  appCfg.VoiceInterview.EvaluatePendingRequeueAfter,
			ProcessingFailAfter:  appCfg.VoiceInterview.EvaluateProcessingFailAfter,
			StaleInProgressAfter: appCfg.VoiceInterview.StaleInProgressAfter,
		},
	})
	voiceEvalConsumer := voiceinterview.NewVoiceEvaluateConsumer(voiceStream, voiceRepo, voiceEvalSvc, slogger)
	voiceEvalConsumer.Start(ctx)
	defer voiceEvalConsumer.Stop()
	voiceEvalRecovery := voiceinterview.NewEvalRecoveryScheduler(voiceSvc, appCfg.VoiceInterview.CleanupInterval, slogger)
	voiceEvalRecovery.Start(ctx)
	defer voiceEvalRecovery.Stop()
	voiceCtl := voiceinterview.NewController(voiceSvc)
	// 票 06：LLM 流式管道接入网关回合挂钩——流式模型按场次 llmProvider 经
	// *aibridge.Bridge 解析（VoiceStreamModelFor，结构性满足 voiceinterview 接缝）；
	// prompt 方向内容复用 interviewSvc 的 Skill 资源子系统（ADR 0002 静态嵌入偏离）；
	// 简历文本读 resumeRepo；TTS 运行时配置每回合重读（设置页修改热生效）。
	voiceSettings := voiceinterview.NewProviderConfigSource(providerSvc)
	voicePipeline := voiceinterview.NewTurnPipeline(voiceinterview.TurnPipelineDeps{
		Svc:      voiceSvc,
		Store:    voiceRepo,
		Models:   bridge,
		Settings: voiceSettings,
		TTS:      voiceinterview.NewTtsClient(appCfg.VoiceInterview, slogger),
		Prompts:  voiceinterview.NewPromptService(interview.NewPromptSanitizer(slogger), interviewSvc, slogger),
		Resumes:  resumeRepo,
		Cfg:      appCfg.VoiceInterview,
		Logger:   slogger,
	})
	voiceGateway := voiceinterview.NewGateway(voiceinterview.GatewayDeps{
		Service:        voiceSvc,
		Settings:       voiceSettings,
		Config:         appCfg.VoiceInterview,
		AllowedOrigins: appCfg.CORS.AllowedOrigins,
		Turn:           voicePipeline.Turn,
		Logger:         slogger,
	})
	go voiceGateway.WarmupOpeningAudio(ctx) // 开场音频预热（默认关，开启时异步不阻塞启动）
	// 票 09：不活动巡检（30s 节拍：4:30 预警一次、5:00 暂停断连停 ASR）。
	voiceInactivity := voiceinterview.NewInactivitySweeper(voiceGateway, appCfg.VoiceInterview, slogger)
	voiceInactivity.Start(ctx)
	defer voiceInactivity.Stop()

	// 5. HTTP 服务器与路由（与 Java 版全局行为对齐）。
	srv := ginserver.New(
		ginserver.WithConfig(cfg),
		ginserver.WithMiddleware(
			httpx.Recovery(slogger),
			ginserver.AllowCORS(appCfg.CORS.AllowedOrigins...),
		),
		ginserver.WithOnShutdown(func(ctx context.Context) {
			voiceGateway.Close() // 关闭活动 WS：断开路径自动结束进行中的会话并触发评估
			logger.Info("on shutdown: 资源清理完成")
		}),
	)
	srv.NoRoute(httpx.NoRouteHandler())
	srv.NoMethod(httpx.NoMethodHandler())

	api := srv.Group("/api")
	providerCtl.RegisterRoutes(api)
	resumeCtl.RegisterRoutes(api)
	interviewCtl.RegisterRoutes(api)
	voiceCtl.RegisterRoutes(api)
	voiceGateway.RegisterRoutes(srv.Group("/ws"))
	srv.GET("/ping", func(c *gin.Context) {
		result.OK(c, gin.H{"message": "pong"})
	})

	// 6. 启动。
	logger.Info("all modules ready, serving",
		slog.String("addr", srv.Addr()),
		slog.String("postgres", db.Config().Addr()),
		slog.String("redis", rdb.Config().Addr))
	if err := srv.Run(); err != nil {
		logger.Error("server exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// mustPostgres 创建 gorm 数据库连接，失败时记录日志并退出。
func mustPostgres(cfg *config.Config, logger *slog.Logger) *gormdb.DB {
	db, err := gormdb.NewWithConfig(cfg, "", gormdb.WithLogger(logger))
	if err != nil {
		logger.Error("连接 PostgreSQL 失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	return db
}

// mustRedis 创建 Redis 客户端，失败时记录日志并退出。
func mustRedis(cfg *config.Config, logger *slog.Logger) *redisx.Client {
	rdb, err := redisx.NewWithConfig(cfg, "", redisx.WithLogger(logger))
	if err != nil {
		logger.Error("连接 Redis 失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
	return rdb
}
