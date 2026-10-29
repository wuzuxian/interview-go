// RTF 基础剥离器与知识库文本清洗的表驱动单测（纯函数缝，票 01 验收标准）。
package docparse

import (
	"strings"
	"testing"
)

// TestParseRTF剥离 RTF 控制序列后输出可读纯文本。
func TestParseRTF剥离(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"最小文档", `{\rtf1\ansi 你好，世界}`, "你好，世界"},
		{"段落与换行", `{\rtf1 第一段\par 第二段\line 第三段}`, "第一段\n第二段\n第三段"},
		{"制表符", `{\rtf1 A\tab B}`, "A\tB"},
		{"花括号与反斜杠转义", `{\rtf1 a\{b\}c\\d}`, `a{b}c\d`},
		{"嵌套组文本保留", `{\rtf1 {\b 粗体{\i 斜体}文本}}`, "粗体斜体文本"},
		{"字体表跳过", `{\rtf1{\fonttbl{\f0 SimSun;}{\f1 Arial;}}正文开始}`, "正文开始"},
		{"颜色表与样式表跳过", `{\rtf1{\colortbl;\red0\green0\blue0;}{\stylesheet{\s0 Normal;}}内容}`, "内容"},
		{"忽略组跳过", `{\rtf1{\*\generator Riched20}正文}`, "正文"},
		{"图片组跳过", `{\rtf1 前{\pict{\*\picprop}89504e470d0a}后}`, "前后"},
		{"unicode转义", `{\rtf1 \u20320?好}`, "你好"},
		{"unicode负数补码", `{\rtf1 \u-39456?\u25928?}`, "无效"},
		{"unicode带uc1回退字节跳过", `{\rtf1\uc1\u20320?好}`, "你好"},
		{"hex转义cp1252", `{\rtf1 caf\'e9}`, "café"},
		{"常用符号控制字", `{\rtf1 a\emdash b\endash c\rdblquote}`, "a—b–c”"},
		{"引号符号", `{\rtf1 \lquote x\rquote y}`, "‘x’y"},
		{"控制字后无空格时正文紧邻", `{\rtf1 \b1粗体\b0普通}`, "粗体普通"},
		{"空文档", `{\rtf1\ansi\deff0{\fonttbl{\f0\fnil\fcharset134 SimSun;}}}`, ""},
		{"未知目标组跳过页眉页脚", `{\rtf1{\header\pard 页眉\par}正文{\footer\pard 页脚\par}}`, "正文"},
		{"反斜杠后换行等价段落", "{\\rtf1 第一行\\\n第二行}", "第一行\n第二行"},
		{"数字参数控制字", `{\rtf1\fs24 字号文字}`, "字号文字"},
		{"非RTF输入按纯文本兜底", "只是普通文本", "只是普通文本"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := ParseRTF([]byte(c.input))
			if got != c.want {
				t.Errorf("ParseRTF = %q, want %q", got, c.want)
			}
		})
	}
}

// TestParseRTF真实样例 一份多段落中文 RTF 的端到端剥离。
func TestParseRTF真实样例(t *testing.T) {
	rtf := `{\rtf1\ansi\ansicpg936\deff0{\fonttbl{\f0\fnil\fcharset134 宋体;}}
{\*\generator Riched20}
\fs24 第一段：介绍 JVM 内存模型与垃圾回收。\par
第二段：讨论 Redis 持久化\u26041?案选择。\par
{\b 第三段：分布式事务的两阶段提交。}\par
}`
	got := ParseRTF([]byte(rtf))
	for _, want := range []string{"第一段：介绍 JVM 内存模型与垃圾回收。", "第二段：讨论 Redis 持久化方案选择。", "第三段：分布式事务的两阶段提交。"} {
		if !strings.Contains(got, want) {
			t.Errorf("剥离结果缺少 %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "宋体") || strings.Contains(got, "Riched20") {
		t.Errorf("剥离结果不应含字体表/生成器内容, got %q", got)
	}
}

// TestParse路由RTF 内容嗅探为 RTF 时走剥离器；扩展名兜底同样生效。
func TestParse路由RTF(t *testing.T) {
	data := []byte(`{\rtf1 剥离我\par}`)
	res, err := Parse(data, "doc.rtf")
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if !strings.Contains(res.Text, "剥离我") {
		t.Errorf("解析文本 = %q, want 含 剥离我", res.Text)
	}

	// octet-stream + .rtf 扩展名兜底。
	res, err = Parse([]byte(`{\rtf1 扩展名兜底}`), "doc.rtf")
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if !strings.Contains(res.Text, "扩展名兜底") {
		t.Errorf("解析文本 = %q, want 含 扩展名兜底", res.Text)
	}
}

// TestCleanTextJava 与 Java TextCleaningService.cleanText 逐规则对齐。
func TestCleanTextJava(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"空文本返回空串", "   \n\t ", ""},
		{"移除控制字符保留换行制表", "a\x00b\x01c\nd\te", "abc\nd\te"},
		{"移除图片文件名整行", "正文一\nimage123.png\n正文二", "正文一\n\n正文二"},
		{"行内图片文件名保留", "引用 image123.png 的说明", "引用 image123.png 的说明"},
		{"移除图片链接", "截图 https://example.com/a/b.png?v=1 展示", "截图  展示"},
		{"移除文件协议路径", "临时文件 file:///tmp/tika-123/upload 内容", "临时文件  内容"},
		{"移除符号分隔线", "标题\n------\n正文", "标题\n\n正文"},
		{"换行符统一", "一\r\n二\r三", "一\n二\n三"},
		{"去行尾空格保留空行", "行一 \t\n\n行二  ", "行一\n\n行二"},
		{"压缩三连以上换行", "a\n\n\n\n\nb", "a\n\nb"},
		{"首尾空白修剪", "\n\n 正文 \n\t", "正文"},
		{"大写扩展名图片链接", "看 HTTP://A.COM/X.PNG 图", "看  图"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := CleanTextJava(c.input); got != c.want {
				t.Errorf("CleanTextJava = %q, want %q", got, c.want)
			}
		})
	}
}
