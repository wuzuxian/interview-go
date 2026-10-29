// 检索片段编号与参考片段尾注（Java RagSources 逐字对齐）：
// 实际进入 Prompt 的片段按顺序标为 [S1]、[S2]…；参考片段列表由服务端在流末追加。
package knowledgebase

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wuzuxian/interview/internal/vector"
)

// collapseWhitespace Java replaceAll("[\\p{Cntrl}\\p{Z}\\s]+", " ")：
// 控制字符 + 分隔符 + 空白压为单个空格。
var collapseWhitespace = regexp.MustCompile(`[\p{Cc}\p{Z}\s]+`)

// escapeChars 需反斜杠转义的 Markdown 特殊字符（Java text() 的转义集合）。
const escapeChars = "\\`*_{}[]()#+-.!|<>~&"

// ragSourceContext 检索片段上下文（Java RagSources.context）：
// [S1] 来源：{来源描述}\n{片段内容}，片段间以 "\n\n---\n\n" 分隔。
func ragSourceContext(docs []vector.KBDoc) string {
	var b strings.Builder
	for i, doc := range docs {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		fmt.Fprintf(&b, "[S%d] 来源：%s\n%s", i+1, ragSourceDescription(doc), doc.Content)
	}
	return b.String()
}

// ragSourceFooter 服务端参考片段列表（Java RagSources.footer）：
// 空片段返回空串；否则以 "\n\n---\n\n参考片段（本次检索提供）：\n\n" 开头逐行列出。
func ragSourceFooter(docs []vector.KBDoc) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n\n参考片段（本次检索提供）：\n\n")
	for i, doc := range docs {
		fmt.Fprintf(&b, "- [S%d] %s\n", i+1, ragSourceDescription(doc))
	}
	return b.String()
}

// ragSourceDescription 单个片段的来源描述（Java description）：
// 来源名（缺省「知识库片段」）+（知识库 {kb_id}）+，原文件：{文件名}+，第 n/m 个片段。
func ragSourceDescription(doc vector.KBDoc) string {
	name := ragSourceText(doc.Metadata["source_name"])
	kbID := ragSourceText(doc.Metadata["kb_id"])
	var b strings.Builder
	if name == "" {
		b.WriteString("知识库片段")
	} else {
		b.WriteString(name)
	}
	if kbID != "" {
		b.WriteString("（知识库 ")
		b.WriteString(kbID)
		b.WriteString("）")
	}
	filename := ragSourceText(doc.Metadata["source_filename"])
	if filename != "" {
		b.WriteString("，原文件：")
		b.WriteString(filename)
	}
	if index, total, ok := chunkRange(doc.Metadata); ok {
		fmt.Fprintf(&b, "，第 %d/%d 个片段", index+1, total)
	}
	return b.String()
}

// chunkRange 解析 chunk_index/total_chunks（Java 语义：均为数值且
// 0 ≤ index < total 才展示，展示值为 index+1/total）。
func chunkRange(metadata map[string]any) (index, total int, ok bool) {
	if metadata == nil {
		return 0, 0, false
	}
	idx, idxOK := metadataNumber(metadata["chunk_index"])
	tot, totOK := metadataNumber(metadata["total_chunks"])
	if !idxOK || !totOK {
		return 0, 0, false
	}
	index, total = idx, tot
	if index < 0 || index >= total {
		return 0, 0, false
	}
	return index, total, true
}

// metadataNumber 元数据数值归一（JSON 反序列化 float64；测试/内存态 int 系）。
func metadataNumber(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

// ragSourceText 元数据文本清洗（Java text()）：控制字符/空白压为单空格并 trim，
// 再对 Markdown 特殊字符逐个加反斜杠。
func ragSourceText(value any) string {
	if value == nil {
		return ""
	}
	singleLine := strings.TrimSpace(collapseWhitespace.ReplaceAllString(fmt.Sprintf("%v", value), " "))
	var b strings.Builder
	for _, r := range singleLine {
		if strings.ContainsRune(escapeChars, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
