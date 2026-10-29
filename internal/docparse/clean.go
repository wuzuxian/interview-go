// Java 版 TextCleaningService.cleanText 的 Go 移植（逐正则对齐）：
// 知识库解析出的文本在切分与向量化前经过同样的语义去噪与格式规范化，
// 保证两个实例对同一文档产出一致的切片输入。
package docparse

import (
	"regexp"
	"strings"
)

var (
	// cleanImageFilenameLine 图片文件名行：image123.png（整行匹配，防止误删正文中的文件名字符串）。
	cleanImageFilenameLine = regexp.MustCompile(`(?m)^image\d+\.(png|jpe?g|gif|bmp|webp)\s*$`)

	// cleanImageURL HTTP/HTTPS 图片链接（支持 URL 查询参数，大小写不敏感）。
	cleanImageURL = regexp.MustCompile(`(?i)https?://\S+?\.(png|jpe?g|gif|bmp|webp)(\?\S*)?`)

	// cleanFileURL 文件协议 URL（Tika PDF 临时文件路径等）。
	cleanFileURL = regexp.MustCompile(`(?i)file:(//)?\S+`)

	// cleanSeparatorLine 分隔线：---、___、***、===（整行匹配，至少 3 个连续符号）。
	cleanSeparatorLine = regexp.MustCompile(`(?m)^\s*[-_*=]{3,}\s*$`)

	// cleanControlChars 控制字符（保留换行符 \n 与制表符 \t）。
	cleanControlChars = regexp.MustCompile("[\x00-\x08\x0B\x0C\x0E-\x1F]")

	// cleanTrailingSpaces 行尾空格和制表符（保留空行以保持段落结构）。
	cleanTrailingSpaces = regexp.MustCompile(`(?m)[ \t]+$`)

	// cleanConsecutiveNewlines 压缩连续空行（最多保留 2 个换行符）。
	cleanConsecutiveNewlines = regexp.MustCompile(`\n{3,}`)
)

// CleanTextJava 清洗解析出的文本，语义与 Java TextCleaningService.cleanText 一致：
//
//	语义级过滤：控制字符、图片文件名行、图片链接、文件协议路径、符号分隔线；
//	格式级清理：统一换行符、去行尾空白、压缩连续空行（最多 2 个换行）。
func CleanTextJava(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	t := cleanControlChars.ReplaceAllString(text, "")
	t = cleanImageFilenameLine.ReplaceAllString(t, "")
	t = cleanImageURL.ReplaceAllString(t, "")
	t = cleanFileURL.ReplaceAllString(t, "")
	t = cleanSeparatorLine.ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, "\r\n", "\n")
	t = strings.ReplaceAll(t, "\r", "\n")
	t = cleanTrailingSpaces.ReplaceAllString(t, "")
	t = cleanConsecutiveNewlines.ReplaceAllString(t, "\n\n")
	// Java String.strip()（Unicode 空白首尾修剪）。
	return strings.TrimSpace(t)
}
