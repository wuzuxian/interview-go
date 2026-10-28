package interview

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ===== 测试辅助 =====

// mapFS 用内存文件构建 fs.FS，模拟 skills 资源根目录。
func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

// demoSkillFiles 生成一个最小可用的双 Skill 资源布局（s1/s2）。
func demoSkillFiles(extra map[string]string) fstest.MapFS {
	files := map[string]string{
		"s1/SKILL.md":       "---\nname: s1\ndescription: 方向一\n---\n考官一",
		"s1/skill.meta.yml": "displayName: 方向一\ncategories:\n  - key: A\n    label: 甲\n    priority: CORE\n",
		"s2/SKILL.md":       "---\nname: s2\ndescription: 方向二\n---\n考官二",
		"s2/skill.meta.yml": "displayName: 方向二\ncategories: []\n",
	}
	for name, content := range extra {
		files[name] = content
	}
	return mapFS(files)
}

// ===== 嵌入资源与注册表 =====

// 契约回归：前端方向列表页依赖 10 个预设方向的字段与排序（Java 版 TreeMap 按 id 升序），
// _shared 参考库只提供服务端注入、不能作为方向出现。
func TestLoadRegistryFromEmbeddedResources(t *testing.T) {
	reg, skipped, err := LoadRegistry(EmbeddedSkills())
	if err != nil {
		t.Fatalf("加载嵌入资源失败: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("完整资源不应有被跳过的 Skill，got %v", skipped)
	}

	wantIDs := []string{
		"ai-agent-dev", "algorithm", "ali-backend", "bytedance-backend", "frontend",
		"java-backend", "java-backend-tencent", "python-backend", "system-design", "test-development",
	}
	if got := reg.SkillIDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("SkillIDs 排序与 Java TreeMap 不一致:\n got %v\nwant %v", got, wantIDs)
	}

	dto, err := reg.Get("java-backend")
	if err != nil {
		t.Fatalf("Get(java-backend) 失败: %v", err)
	}
	if dto.ID != "java-backend" {
		t.Errorf("id = %q", dto.ID)
	}
	if dto.Name != "Java 后端开发" {
		t.Errorf("name 应取 meta displayName，got %q", dto.Name)
	}
	if dto.Description != "用于 Java 后端面试出题；优先围绕 Java 核心、MySQL、Redis、Spring 与项目实战，追问设计取舍、故障处理和性能优化。" {
		t.Errorf("description 应取 SKILL.md front matter，got %q", dto.Description)
	}
	if !dto.IsPreset {
		t.Error("预设方向 isPreset 必须为 true")
	}
	if dto.SourceJd != nil {
		t.Errorf("预设方向 sourceJd 应为 null，got %v", *dto.SourceJd)
	}
	if dto.Persona == nil || !strings.Contains(*dto.Persona, "Java 后端面试官") {
		t.Errorf("persona 应取 SKILL.md body，got %v", dto.Persona)
	}
	if dto.Display == nil {
		t.Fatal("display 不应为 null")
	}
	wantDisplay := DisplayDTO{
		Icon: "☕", Gradient: "from-blue-500 to-indigo-500",
		IconBg: "bg-blue-100 dark:bg-blue-900/30", IconColor: "text-blue-600 dark:text-blue-400",
	}
	if *dto.Display != wantDisplay {
		t.Errorf("display = %+v, want %+v", *dto.Display, wantDisplay)
	}

	// 分类契约：key/label/priority/ref/shared，PROJECT 无 ref 无 shared。
	if len(dto.Categories) != 6 {
		t.Fatalf("java-backend 应有 6 个分类，got %d", len(dto.Categories))
	}
	wantCats := []CategoryDTO{
		{Key: "JAVA", Label: "Java", Priority: "CORE", Ref: strPtr("java.md"), Shared: true},
		{Key: "MYSQL", Label: "MySQL", Priority: "CORE", Ref: strPtr("mysql.md"), Shared: true},
		{Key: "REDIS", Label: "Redis", Priority: "CORE", Ref: strPtr("redis.md"), Shared: true},
		{Key: "SPRING", Label: "Spring", Priority: "NORMAL", Ref: strPtr("spring.md"), Shared: true},
		{Key: "SYSTEM_DESIGN_SCENARIO", Label: "系统设计/场景题", Priority: "NORMAL", Ref: strPtr("system-design-scenarios.md"), Shared: true},
		{Key: "PROJECT", Label: "项目经历", Priority: "ALWAYS_ONE", Ref: nil, Shared: false},
	}
	for i, want := range wantCats {
		if !equalCategory(dto.Categories[i], want) {
			t.Errorf("categories[%d] = %+v, want %+v", i, dto.Categories[i], want)
		}
	}
}

// equalCategory 逐字段比较分类（Ref 为指针，需比较指值而非地址）。
func equalCategory(a, b CategoryDTO) bool {
	if a.Key != b.Key || a.Label != b.Label || a.Priority != b.Priority || a.Shared != b.Shared {
		return false
	}
	if (a.Ref == nil) != (b.Ref == nil) {
		return false
	}
	return a.Ref == nil || *a.Ref == *b.Ref
}

// 契约回归：响应 JSON 字段名必须与 Java record 序列化逐字一致
// （SkillDTO/SkillCategoryDTO/DisplayDTO，lowerCamelCase，null 字段保留）。
func TestSkillDTOJSONContract(t *testing.T) {
	reg, _, err := LoadRegistry(EmbeddedSkills())
	if err != nil {
		t.Fatalf("加载嵌入资源失败: %v", err)
	}
	dto, err := reg.Get("java-backend")
	if err != nil {
		t.Fatalf("Get(java-backend) 失败: %v", err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}

	wantTop := []string{"categories", "description", "display", "id", "isPreset", "name", "persona", "sourceJd"}
	gotTop := make([]string, 0, len(m))
	for k := range m {
		gotTop = append(gotTop, k)
	}
	if !reflect.DeepEqual(sortedStrings(gotTop), wantTop) {
		t.Errorf("顶层字段 = %v, want %v", gotTop, wantTop)
	}
	if m["sourceJd"] != nil {
		t.Errorf("sourceJd 应序列化为 null，got %v", m["sourceJd"])
	}
	if m["isPreset"] != true {
		t.Errorf("isPreset 应为 true，got %v", m["isPreset"])
	}

	cats, ok := m["categories"].([]any)
	if !ok || len(cats) == 0 {
		t.Fatalf("categories 序列化异常: %v", m["categories"])
	}
	cat0, _ := cats[0].(map[string]any)
	wantCatKeys := []string{"key", "label", "priority", "ref", "shared"}
	gotCatKeys := make([]string, 0, len(cat0))
	for k := range cat0 {
		gotCatKeys = append(gotCatKeys, k)
	}
	if !reflect.DeepEqual(sortedStrings(gotCatKeys), wantCatKeys) {
		t.Errorf("分类字段 = %v, want %v", gotCatKeys, wantCatKeys)
	}

	disp, _ := m["display"].(map[string]any)
	wantDispKeys := []string{"gradient", "icon", "iconBg", "iconColor"}
	gotDispKeys := make([]string, 0, len(disp))
	for k := range disp {
		gotDispKeys = append(gotDispKeys, k)
	}
	if !reflect.DeepEqual(sortedStrings(gotDispKeys), wantDispKeys) {
		t.Errorf("display 字段 = %v, want %v", gotDispKeys, wantDispKeys)
	}

	// 无 ref 的分类（PROJECT）必须序列化为 "ref": null 而非缺省。
	project, _ := cats[5].(map[string]any)
	if _, exists := project["ref"]; !exists {
		t.Error("PROJECT 分类的 ref 字段缺失（应为 null）")
	} else if project["ref"] != nil {
		t.Errorf("PROJECT 的 ref 应为 null，got %v", project["ref"])
	}
}

// 脏数据表驱动：缺字段、非法 priority、缺 meta 文件等解析行为
// 与 Java loadPresetSkills/parseSkillDefinition 对齐。
func TestLoadRegistryDirtyData(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		check func(t *testing.T, reg *Registry, skipped []string, err error)
	}{
		{
			name: "完整定义_displayName与分类",
			files: map[string]string{
				"demo/SKILL.md":       "---\nname: demo\ndescription: 测试方向\n---\n你是一位测试考官",
				"demo/skill.meta.yml": "displayName: 演示方向\ndisplay:\n  icon: T\n  gradient: from-a to-b\n  iconBg: bg-x\n  iconColor: text-y\ncategories:\n  - key: ALPHA\n    label: 阿尔法\n    priority: ALWAYS_ONE\n  - key: BETA\n    label: 贝塔\n    priority: CORE\n    ref: beta.md\n    shared: true\n  - key: GAMMA\n    label: 伽马\n    priority: URGENT\n",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				dto, gerr := reg.Get("demo")
				if gerr != nil {
					t.Fatalf("Get(demo) 失败: %v", gerr)
				}
				if dto.Name != "演示方向" {
					t.Errorf("name 应取 displayName，got %q", dto.Name)
				}
				if dto.Display == nil || dto.Display.Icon != "T" {
					t.Errorf("display 解析错误: %+v", dto.Display)
				}
				if len(dto.Categories) != 3 {
					t.Fatalf("应有 3 个分类，got %d", len(dto.Categories))
				}
				if dto.Categories[0].Ref != nil || dto.Categories[0].Shared {
					t.Errorf("无 ref/shared 的分类应为 null/false: %+v", dto.Categories[0])
				}
				if dto.Categories[1].Ref == nil || *dto.Categories[1].Ref != "beta.md" || !dto.Categories[1].Shared {
					t.Errorf("BETA 的 ref/shared 解析错误: %+v", dto.Categories[1])
				}
				// 非法 priority 原样透传给前端（分配阶段才按 NORMAL 兜底，属票 07 行为）。
				if dto.Categories[2].Priority != "URGENT" {
					t.Errorf("非法 priority 应原样保留，got %q", dto.Categories[2].Priority)
				}
				if dto.Persona == nil || *dto.Persona != "你是一位测试考官" {
					t.Errorf("persona 应为 SKILL.md body（trim 后），got %v", dto.Persona)
				}
			},
		},
		{
			name: "缺name的Skill被跳过",
			files: map[string]string{
				"demo/SKILL.md": "---\ndescription: 没有名字\n---\n正文",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err != nil {
					t.Fatalf("缺 name 应跳过而非报错: %v", err)
				}
				if len(skipped) != 1 || skipped[0] != "demo" {
					t.Errorf("skipped = %v, want [demo]", skipped)
				}
				if ids := reg.SkillIDs(); len(ids) != 0 {
					t.Errorf("注册表应为空，got %v", ids)
				}
			},
		},
		{
			name: "缺front_matter报错",
			files: map[string]string{
				"demo/SKILL.md": "# Overview\n没有 front matter",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err == nil {
					t.Fatal("缺 front matter 应报错（Java 版启动失败语义）")
				}
			},
		},
		{
			name: "缺meta文件用默认配置",
			files: map[string]string{
				"demo/SKILL.md": "---\nname: demo\ndescription: 测试方向\n---\n考官",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err != nil {
					t.Fatalf("缺 meta 文件不应报错: %v", err)
				}
				dto, gerr := reg.Get("demo")
				if gerr != nil {
					t.Fatalf("Get(demo) 失败: %v", gerr)
				}
				if dto.Name != "demo" {
					t.Errorf("无 displayName 时 name 应回退到 front matter name，got %q", dto.Name)
				}
				if dto.Display != nil {
					t.Errorf("无 meta 时 display 应为 null，got %+v", dto.Display)
				}
				if len(dto.Categories) != 0 {
					t.Errorf("无 meta 时分类应为空，got %d", len(dto.Categories))
				}
			},
		},
		{
			name: "非法meta_YAML报错",
			files: map[string]string{
				"demo/SKILL.md":       "---\nname: demo\ndescription: 测试方向\n---\n考官",
				"demo/skill.meta.yml": "display: [broken",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err == nil {
					t.Fatal("非法 YAML 应报错（Java 版 SnakeYAML 启动失败语义）")
				}
			},
		},
		{
			name: "_shared目录不注册",
			files: map[string]string{
				"_shared/SKILL.md": "---\nname: _shared\ndescription: 参考库\n---\n共享",
				"real/SKILL.md":    "---\nname: real\ndescription: 正常方向\n---\n正文",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				if ids := reg.SkillIDs(); !reflect.DeepEqual(ids, []string{"real"}) {
					t.Errorf("_shared 不应注册，got %v", ids)
				}
			},
		},
		{
			name: "空YAML与空body",
			files: map[string]string{
				"demo/SKILL.md":       "---\nname: demo\ndescription: 测试方向\n---\n",
				"demo/skill.meta.yml": "",
			},
			check: func(t *testing.T, reg *Registry, skipped []string, err error) {
				if err != nil {
					t.Fatalf("空 meta/空 body 不应报错: %v", err)
				}
				dto, gerr := reg.Get("demo")
				if gerr != nil {
					t.Fatalf("Get(demo) 失败: %v", gerr)
				}
				if dto.Persona != nil {
					t.Errorf("空 body 的 persona 应为 null，got %q", *dto.Persona)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, skipped, err := LoadRegistry(mapFS(tc.files))
			tc.check(t, reg, skipped, err)
		})
	}
}

// ===== ref/shared 参考文件定位（票 07 注入的准备） =====

// 与 Java resolveReferenceLocations 逐规则对齐：
// shared 优先 _shared/references；非 shared 先 skill 内（references/ 与根目录两处）再回落 _shared；
// custom 或 shared 时扫全部预设 Skill。
func TestResolveReferenceLocations(t *testing.T) {
	reg, _, err := LoadRegistry(demoSkillFiles(nil))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	cases := []struct {
		name    string
		skillID string
		ref     string
		shared  bool
		want    []string
	}{
		{
			name:    "shared先共享库再本skill再全量扫",
			skillID: "s1", ref: "java.md", shared: true,
			want: []string{
				"_shared/references/java.md",
				"s1/references/java.md", "s1/java.md",
				"s2/references/java.md", "s2/java.md",
			},
		},
		{
			name:    "非shared先本skill再回落共享库",
			skillID: "s1", ref: "x.md", shared: false,
			want: []string{
				"s1/references/x.md", "s1/x.md",
				"_shared/references/x.md",
			},
		},
		{
			name:    "custom跳过自身并扫全部预设",
			skillID: CustomSkillID, ref: "y.md", shared: false,
			want: []string{
				"_shared/references/y.md",
				"s1/references/y.md", "s1/y.md",
				"s2/references/y.md", "s2/y.md",
			},
		},
		{
			name: "路径穿越不安全返回空", skillID: "s1", ref: "../evil.md", shared: false,
			want: []string{},
		},
		{
			name: "绝对路径不安全返回空", skillID: "s1", ref: "/abs.md", shared: false,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reg.ResolveReferenceLocations(tc.skillID, tc.ref, tc.shared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("候选路径:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// 真实嵌入资源上的定位：共享库直取、skill 根目录回落（ai-agent-dev.md 不在 references/ 下）、
// 缺失与不安全路径不可定位。
func TestLocateReferenceOnEmbeddedResources(t *testing.T) {
	reg, _, err := LoadRegistry(EmbeddedSkills())
	if err != nil {
		t.Fatalf("加载嵌入资源失败: %v", err)
	}
	fsys := EmbeddedSkills()

	cases := []struct {
		name      string
		skillID   string
		ref       string
		shared    bool
		wantPath  string
		wantFound bool
	}{
		{"共享库直取", "java-backend", "java.md", true, "_shared/references/java.md", true},
		{"skill根目录回落", "ai-agent-dev", "ai-agent-dev.md", false, "ai-agent-dev/ai-agent-dev.md", true},
		{"共享算法资料", "algorithm", "algorithm-data-structures.md", true, "_shared/references/algorithm-data-structures.md", true},
		{"缺失文件不可定位", "java-backend", "nope.md", false, "", false},
		{"不安全路径不可定位", "java-backend", "../evil.md", true, "", false},
		{"custom可在共享库定位", CustomSkillID, "java.md", false, "_shared/references/java.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, ok := reg.LocateReference(fsys, tc.skillID, tc.ref, tc.shared)
			if ok != tc.wantFound || path != tc.wantPath {
				t.Errorf("LocateReference(%s,%s,%v) = (%q,%v), want (%q,%v)",
					tc.skillID, tc.ref, tc.shared, path, ok, tc.wantPath, tc.wantFound)
			}
		})
	}
}

// ===== Service 与业务码 =====

// 契约回归：不存在的方向必须返回 interview 段业务码（404 语义），消息与 Java 版一致。
func TestServiceGetUnknownSkill(t *testing.T) {
	svc, err := NewService(discardLogger())
	if err != nil {
		t.Fatalf("NewService 失败: %v", err)
	}

	if got := len(svc.ListSkills()); got != 10 {
		t.Errorf("ListSkills 应返回 10 个预设方向，got %d", got)
	}

	_, err = svc.GetSkill("no-such-skill")
	if err == nil {
		t.Fatal("不存在的方向应返回业务错误")
	}
	ae := apperr.From(err)
	if ae.Code != apperr.CodeSkillNotFound {
		t.Errorf("业务码 = %d, want %d", ae.Code, apperr.CodeSkillNotFound)
	}
	if !strings.Contains(ae.Message, "未找到面试主题") {
		t.Errorf("消息应与 Java 版一致，got %q", ae.Message)
	}

	if _, ok := svc.LocateReference("java-backend", "java.md", true); !ok {
		t.Error("Service.LocateReference 应能定位共享库参考文件")
	}
}

// ===== 辅助 =====

func strPtr(s string) *string { return &s }

// discardLogger 丢弃全部日志输出，供测试使用。
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
