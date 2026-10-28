// 开场白（票 05）：按方向从嵌入的 voice-interview-opening.yml 资产取开场文案，
// 判定逻辑与 Java VoiceInterviewWebSocketHandler.buildOpeningQuestion（L283-307）
// 逐字对齐：skill-questions 按方向命中 → 用之；algorithm-skills 命中 → 算法默认题
// （配置为空时代码内常量兜底）；其余方向 → 后端默认题。资产文件为 Java 仓库
// app/src/main/resources/voice-interview-opening.yml 的逐字节拷贝（cmp 校验一致），
// 对应 Java spring.config.import: optional:classpath:voice-interview-opening.yml。
package voiceinterview

import (
	_ "embed"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed assets/voice-interview-opening.yml
var openingAssetYml []byte

// 配置为空时的兜底文案（Java Handler 常量 DEFAULT_OPENING_QUESTION_ALGORITHM /
// DEFAULT_OPENING_QUESTION_BACKEND，L95-98 逐字；「」为原文 \u300C/\u300D）。
const (
	defaultOpeningQuestionAlgorithm = "你好，我是本场面试官。第一个问题：请你口述一道算法题，不写代码，只讲「问题建模、数据结构选型、步骤、复杂度、边界处理」。"
	defaultOpeningQuestionBackend   = "你好，我是本场面试官。第一个问题：请用 1 分钟介绍一个你深度参与的项目，按三点回答：业务目标、你负责的核心模块、核心技术栈。说完我会立刻追问一个关键技术决策。"
)

// OpeningConfig 开场白资产结构（Java VoiceInterviewProperties.OpeningConfig）。
type OpeningConfig struct {
	SkillQuestions    map[string]string `yaml:"skill-questions"`
	AlgorithmSkills   []string          `yaml:"algorithm-skills"`
	AlgorithmQuestion string            `yaml:"algorithm-question"`
	BackendQuestion   string            `yaml:"backend-question"`
}

var (
	openingOnce sync.Once
	openingCfg  OpeningConfig
)

// embeddedOpeningConfig 解析嵌入的开场白资产（进程内仅解析一次）。资产随二进制
// 编译，解析失败属编程错误，直接 panic（对齐 Spring 配置文件损坏即启动失败）。
func embeddedOpeningConfig() OpeningConfig {
	openingOnce.Do(func() {
		var doc struct {
			App struct {
				VoiceInterview struct {
					Opening OpeningConfig `yaml:"opening"`
				} `yaml:"voice-interview"`
			} `yaml:"app"`
		}
		if err := yaml.Unmarshal(openingAssetYml, &doc); err != nil {
			panic("voiceinterview: 开场白资产解析失败: " + err.Error())
		}
		openingCfg = doc.App.VoiceInterview.Opening
	})
	return openingCfg
}

// isBlank Java String::isBlank 语义（null/空白字符序列为空已由零值覆盖）。
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// buildOpeningQuestion 按方向解析开场文案（Java buildOpeningQuestion L283-307 逐字）：
// skill-questions 命中且非空白 → 用之；skillId 命中 algorithm-skills →
// algorithm-question（空白则算法兜底常量）；其余 → backend-question（空白则后端
// 兜底常量）。skillID 传空串（Java 侧 skillId 为 null 时取 ""）走默认分支。
func buildOpeningQuestion(skillID string, opening OpeningConfig) string {
	if bySkill, ok := opening.SkillQuestions[skillID]; ok && !isBlank(bySkill) {
		return bySkill
	}
	algorithmSkills := opening.AlgorithmSkills
	for _, s := range algorithmSkills {
		if s == skillID {
			if !isBlank(opening.AlgorithmQuestion) {
				return opening.AlgorithmQuestion
			}
			return defaultOpeningQuestionAlgorithm
		}
	}
	if !isBlank(opening.BackendQuestion) {
		return opening.BackendQuestion
	}
	return defaultOpeningQuestionBackend
}

// openingAudioCache 开场音频内存缓存（Java Handler openingAudioCache L82 +
// getOpeningWavAudio L263-281）：键为开场文案——文案由方向唯一决定，按文案缓存
// 与 Java 逐字一致，等价实现"同方向重复建连不重复合成"。
type openingAudioCache struct {
	mu   sync.Mutex
	byID map[string][]byte
}

func newOpeningAudioCache() *openingAudioCache {
	return &openingAudioCache{byID: make(map[string][]byte)}
}

// get 缓存命中返回 WAV 字节，未命中返回 nil。
func (c *openingAudioCache) get(text string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byID[text]
}

// put 回填缓存（Java：仅非空结果写入）。
func (c *openingAudioCache) put(text string, wav []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[text] = wav
}

// size 当前缓存条数（预热完成日志用）。
func (c *openingAudioCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byID)
}
