// 开场白资产与文案判定单测（票 05）：嵌入资产逐字解析（方向清单与默认题原文）、
// buildOpeningQuestion 三级判定表（对齐 Java buildOpeningQuestion L283-307）、
// 兜底常量与音频缓存行为。
package voiceinterview

import "testing"

// 资产文件为 Java voice-interview-opening.yml 逐字节拷贝；此处锁定解析结果：
// 10 个方向键、算法方向清单与两段默认题原文（YAML 折叠标量按空格拼行、
// clip 收尾保留单个换行，SnakeYAML 同语义）。
func TestOpeningAsset嵌入资产解析(t *testing.T) {
	oc := embeddedOpeningConfig()

	wantSkills := []string{
		"java-backend", "java-backend-tencent", "bytedance-backend", "ali-backend",
		"python-backend", "frontend", "system-design", "algorithm",
		"test-development", "ai-agent-dev",
	}
	if len(oc.SkillQuestions) != len(wantSkills) {
		t.Fatalf("skill-questions 应有 %d 个方向，实际 %d: %v", len(wantSkills), len(oc.SkillQuestions), oc.SkillQuestions)
	}
	for _, k := range wantSkills {
		if _, ok := oc.SkillQuestions[k]; !ok {
			t.Errorf("缺少方向 %s", k)
		}
	}
	if len(oc.AlgorithmSkills) != 2 || oc.AlgorithmSkills[0] != "bytedance-backend" || oc.AlgorithmSkills[1] != "algorithm" {
		t.Errorf("algorithm-skills 应为 [bytedance-backend algorithm]: %v", oc.AlgorithmSkills)
	}

	wantJavaBackend := "你好，我是本场面试官。第一个问题：请用 1 分钟介绍一个你深度参与的后端项目，" +
		" 按”业务目标、核心链路、你的职责”回答，说完我会立刻追问一个关键技术决策。\n"
	if got := oc.SkillQuestions["java-backend"]; got != wantJavaBackend {
		t.Errorf("java-backend 文案原文不符:\n got %q\nwant %q", got, wantJavaBackend)
	}
	wantAlgorithmQuestion := "你好，我是本场面试官。先做一道算法与数据结构热身题：请你从”哈希表/堆/栈/队列/树/图”里选两个，" +
		" 结合一道你熟悉的题，口述”为什么选这个结构、核心步骤、时间复杂度、空间复杂度、边界条件与反例”。" +
		" 本场不需要写代码，重点看你的思路和取舍。\n"
	if oc.AlgorithmQuestion != wantAlgorithmQuestion {
		t.Errorf("algorithm-question 原文不符:\n got %q\nwant %q", oc.AlgorithmQuestion, wantAlgorithmQuestion)
	}
	wantBackendQuestion := "你好，我是本场面试官。第一个问题：请用 1 分钟介绍一个你深度参与的项目，" +
		" 按三点回答：业务目标、你负责的核心模块、核心技术栈。说完我会立刻追问一个关键技术决策。\n"
	if oc.BackendQuestion != wantBackendQuestion {
		t.Errorf("backend-question 原文不符:\n got %q\nwant %q", oc.BackendQuestion, wantBackendQuestion)
	}
}

// buildOpeningQuestion 三级判定（Java buildOpeningQuestion L283-307）。
func TestBuildOpeningQuestion方向判定(t *testing.T) {
	oc := embeddedOpeningConfig()

	cases := []struct {
		name    string
		skillID string
		want    string
	}{
		{"方向命中用方向文案", "java-backend", oc.SkillQuestions["java-backend"]},
		{"腾讯方向命中自身文案", "java-backend-tencent", oc.SkillQuestions["java-backend-tencent"]},
		// 资产中 bytedance-backend/algorithm 同时在 skill-questions 与
		// algorithm-skills：skill-questions 一级判定先命中（Java L287-291 在前）。
		{"算法组合方向命中自身文案", "bytedance-backend", oc.SkillQuestions["bytedance-backend"]},
		{"纯算法方向命中自身文案", "algorithm", oc.SkillQuestions["algorithm"]},
		{"未知方向走后端默认题", "ruby-backend", oc.BackendQuestion},
		{"方向为空走后端默认题", "", oc.BackendQuestion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildOpeningQuestion(tc.skillID, oc); got != tc.want {
				t.Errorf("buildOpeningQuestion(%q) 方向文案不符:\n got %q\nwant %q", tc.skillID, got, tc.want)
			}
		})
	}

	// 仅在 algorithm-skills 而不在 skill-questions 的方向 → 算法默认题
	// （Java L293-302 二级判定）。
	custom := OpeningConfig{
		SkillQuestions:    map[string]string{"java-backend": oc.SkillQuestions["java-backend"]},
		AlgorithmSkills:   []string{"rust-alg"},
		AlgorithmQuestion: oc.AlgorithmQuestion,
		BackendQuestion:   oc.BackendQuestion,
	}
	if got := buildOpeningQuestion("rust-alg", custom); got != oc.AlgorithmQuestion {
		t.Errorf("algorithm-skills 命中应走算法默认题: %q", got)
	}
}

// 配置项为空白时的兜底：算法方向 → 算法兜底常量；其余 → 后端兜底常量；
// 方向条目存在但文案空白 → 视同未命中继续下落（Java isBlank 判定）。
func TestBuildOpeningQuestion空白配置兜底常量(t *testing.T) {
	blank := OpeningConfig{
		SkillQuestions:  map[string]string{"ghost-skill": "   "},
		AlgorithmSkills: []string{"ghost-alg"},
	}
	if got := buildOpeningQuestion("ghost-alg", blank); got != defaultOpeningQuestionAlgorithm {
		t.Errorf("算法默认题空白应兜底常量: %q", got)
	}
	if got := buildOpeningQuestion("ruby-backend", blank); got != defaultOpeningQuestionBackend {
		t.Errorf("后端默认题空白应兜底常量: %q", got)
	}
	if got := buildOpeningQuestion("ghost-skill", blank); got != defaultOpeningQuestionBackend {
		t.Errorf("方向条目空白应视同未命中走后端兜底: %q", got)
	}
}

// 开场音频缓存：命中返回原字节、未命中返回 nil、put 后可见。
func TestOpeningAudioCache读写(t *testing.T) {
	c := newOpeningAudioCache()
	if got := c.get("文案"); got != nil {
		t.Errorf("未命中应返回 nil: %v", got)
	}
	wav := []byte{1, 2, 3}
	c.put("文案", wav)
	got := c.get("文案")
	if len(got) != 3 || got[0] != 1 {
		t.Errorf("命中应返回缓存字节: %v", got)
	}
}
