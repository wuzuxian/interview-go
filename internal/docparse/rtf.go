// RTF 基础剥离器：从 RTF 字节流中提取可读纯文本（知识库向量化解析用）。
// 对应 Java 侧由 Tika AutoDetectParser 承担的 RTF 解析能力子集；复杂嵌入对象
// （图片/OLE）按 spec 明确排除（Out of Scope，偏离留痕）。纯手写状态机，无外部依赖。
package docparse

import (
	"strings"
	"unicode/utf8"
)

// rtfDestination 需要整体跳过的目标组（字体表、颜色表、图片等，正文无关）。
var rtfDestinations = map[string]bool{
	"fonttbl":            true,
	"colortbl":           true,
	"stylesheet":         true,
	"info":               true,
	"pict":               true,
	"object":             true,
	"header":             true,
	"headerl":            true,
	"headerr":            true,
	"headerf":            true,
	"footer":             true,
	"footerl":            true,
	"footerr":            true,
	"footerf":            true,
	"generator":          true,
	"listtable":          true,
	"listoverridetable":  true,
	"rsidtbl":            true,
	"xmlnstbl":           true,
	"themedata":          true,
	"colorschememapping": true,
	"datastore":          true,
	"latentstyles":       true,
	"expiredattributes":  true,
}

// rtfSymbolMap 常见符号控制字 → 替换字符。
var rtfSymbolMap = map[string]string{
	"emdash":    "—",
	"endash":    "–",
	"emspace":   " ",
	"enspace":   " ",
	"qmspace":   " ",
	"bullet":    "•",
	"lquote":    "‘",
	"rquote":    "’",
	"ldblquote": "“",
	"rdblquote": "”",
	"~":         "\u00A0", // 不换行空格（控制符号 \~）
	"-":         "\u00AD", // 可选连字符（控制符号 \-）
	"_":         "‑",      // 不可断连字符（控制符号 \_）
}

// ParseRTF 剥离 RTF 控制序列，返回可读纯文本（不清洗，清洗由调用方负责）。
func ParseRTF(data []byte) string {
	p := &rtfParser{src: data, uc: 1}
	p.run()
	return p.out.String()
}

type rtfParser struct {
	src []byte
	pos int
	out strings.Builder
	uc  int // \uN 后需跳过的回退字符数（RTF 规范默认 1）
}

func (p *rtfParser) run() {
	// 快速排除非 RTF。
	if len(p.src) < 5 || !strings.HasPrefix(strings.TrimLeft(string(p.src[:16]), " \t\r\n"), "{\\rtf") {
		// 非标准 RTF：按纯文本兜底输出（调用方仍有清洗层兜底）。
		p.out.Write(p.src)
		return
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '{':
			p.pos++
			p.parseGroup()
		case '}':
			p.pos++
			return
		case '\\':
			p.parseControl()
		case '\r', '\n':
			p.pos++ // RTF 源码换行只是排版
		default:
			p.pos = p.emitText(p.pos)
		}
	}
}

// parseGroup 从已消费的 '{' 开始解析一个组；遇到匹配的 '}' 返回。
func (p *rtfParser) parseGroup() {
	// 目标组判定：{\fonttbl...（目标组）与 {\*\...（忽略组，\* 后跟目标名）。
	j := p.pos
	if j+1 < len(p.src) && p.src[j] == '\\' && p.src[j+1] == '*' {
		j += 2
	}
	if j < len(p.src) && p.src[j] == '\\' {
		j++
		nameStart := j
		for j < len(p.src) && isRTFAlpha(p.src[j]) {
			j++
		}
		name := string(p.src[nameStart:j])
		if rtfDestinations[name] {
			p.skipDestination()
			return
		}
	}
	// 普通组：嵌套组递归解析（目标组在递归入口识别），到匹配的 '}' 返回。
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '{':
			p.pos++
			p.parseGroup()
		case '}':
			p.pos++
			return
		case '\\':
			p.parseControl()
		case '\r', '\n':
			p.pos++
		default:
			p.pos = p.emitText(p.pos)
		}
	}
}

// skipDestination 跳过整个目标组（已平衡的花括号）。
func (p *rtfParser) skipDestination() {
	depth := 1
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '{':
			depth++
			p.pos++
		case '}':
			depth--
			p.pos++
			if depth == 0 {
				return
			}
		case '\\':
			// 跳过控制序列，避免其参数里的花括号干扰计数。
			j := p.pos + 1
			if j < len(p.src) {
				c := p.src[j]
				if c == '\\' || c == '{' || c == '}' {
					p.pos = j + 1
					continue
				}
				if isRTFAlpha(c) {
					for j < len(p.src) && isRTFAlpha(p.src[j]) {
						j++
					}
					// 数字参数（含负号）
					if j < len(p.src) && p.src[j] == '-' {
						j++
					}
					for j < len(p.src) && isRTFDigit(p.src[j]) {
						j++
					}
					if j < len(p.src) && p.src[j] == ' ' {
						j++
					}
					p.pos = j
					continue
				}
				// 控制符号（如 \'、\*）：消费两字符。
				p.pos = j + 1
				if j < len(p.src) {
					p.pos++
				}
				continue
			}
			p.pos = j
		default:
			p.pos++
		}
	}
}

// parseControl 从已消费的 '\\' 开始解析控制字或控制符号。
func (p *rtfParser) parseControl() {
	p.pos++ // 跳过 '\'
	if p.pos >= len(p.src) {
		return
	}
	c := p.src[p.pos]
	switch {
	case c == '\\', c == '{', c == '}':
		p.out.WriteByte(c)
		p.pos++
	case c == '\'': // \'xx 十六进制字节（按 cp1252 解码）
		if p.pos+2 < len(p.src) {
			p.out.WriteRune(cp1252Rune(hexByte(p.src[p.pos+1], p.src[p.pos+2])))
			p.pos += 3
		} else {
			p.pos = len(p.src)
		}
	case c == '~' || c == '-' || c == '_':
		p.out.WriteString(rtfSymbolMap[string(c)])
		p.pos++
	case c == '*': // {\*...} 已在组层处理；这里出现时跳过标记本身
		p.pos++
	case isRTFAlpha(c):
		nameStart := p.pos
		for p.pos < len(p.src) && isRTFAlpha(p.src[p.pos]) {
			p.pos++
		}
		name := string(p.src[nameStart:p.pos])
		// 数字参数（含负号）
		param, hasParam := 0, false
		neg := false
		if p.pos < len(p.src) && p.src[p.pos] == '-' {
			neg = true
			p.pos++
		}
		for p.pos < len(p.src) && isRTFDigit(p.src[p.pos]) {
			param = param*10 + int(p.src[p.pos]-'0')
			hasParam = true
			p.pos++
		}
		if neg {
			param = -param
		}
		// 控制字以空格结束时空格被消费；否则保留（'\parX' 的 'X' 是正文）。
		if p.pos < len(p.src) && p.src[p.pos] == ' ' {
			p.pos++
		}
		p.applyControl(name, param, hasParam)
	case c == '\r' || c == '\n': // \ 后直接换行：等价 \par（规范行为）
		p.out.WriteByte('\n')
		p.pos++
		if p.pos < len(p.src) && p.src[p.pos] == '\n' {
			p.pos++
		}
	default: // 未知的单字符控制符号：忽略
		p.pos++
	}
}

// applyControl 处理影响文本输出的控制字。
func (p *rtfParser) applyControl(name string, param int, hasParam bool) {
	switch name {
	case "par", "line", "sect", "page", "column":
		p.out.WriteByte('\n')
	case "tab":
		p.out.WriteByte('\t')
	case "uc":
		if hasParam {
			p.uc = param
		}
	case "u": // \uN Unicode 字符（负数补码），后随 ucN 个回退字符
		r := rune(param)
		if r < 0 {
			r += 65536
		}
		if r > 0 && (r < 0xD800 || r > 0xDFFF) {
			p.out.WriteRune(r)
		}
		for i := 0; i < p.uc; i++ {
			// 跳过回退字符（可能是 \'xx 字节或单字节字符）。
			if p.pos < len(p.src) && p.src[p.pos] == '\\' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '\'' {
				p.pos += 3
			} else if p.pos < len(p.src) {
				p.pos++
			}
		}
	case "emdash", "endash", "bullet", "lquote", "rquote", "ldblquote", "rdblquote", "emspace", "enspace", "qmspace":
		p.out.WriteString(rtfSymbolMap[name])
	default:
		// 其余控制字（\ansi、\ansicpg、\deffN、\b、\fsN 等）不影响文本。
	}
}

// emitText 从 pos 处 emit 一个可见字符（ASCII 直出；多字节序列尝试按 UTF-8
// 解码，非法时按 cp1252 单字节兜底），返回下一个位置。
func (p *rtfParser) emitText(i int) int {
	b := p.src[i]
	if b < 0x80 {
		p.out.WriteByte(b)
		return i + 1
	}
	if n := utf8SeqLen(b); n > 1 && i+n <= len(p.src) {
		if r, width := utf8.DecodeRune(p.src[i : i+n]); width == n && r != utf8.RuneError {
			p.out.WriteRune(r)
			return i + n
		}
	}
	p.out.WriteRune(cp1252Rune(b))
	return i + 1
}

// utf8SeqLen 首字节对应的 UTF-8 序列长度；非序列首字节返回 0。
func utf8SeqLen(b byte) int {
	switch {
	case b >= 0xC2 && b <= 0xDF:
		return 2
	case b >= 0xE0 && b <= 0xEF:
		return 3
	case b >= 0xF0 && b <= 0xF4:
		return 4
	}
	return 0
}

// cp1252Rune Windows-1252 高位字节 → Unicode（0x80-0x9F 为特殊符号表，
// 0xA0-0xFF 与 Latin-1 一致；未定义位置按同码点兜底）。
func cp1252Rune(b byte) rune {
	switch {
	case b >= 0x80 && b <= 0x9F:
		switch b {
		case 0x80:
			return '€'
		case 0x82:
			return '‚'
		case 0x83:
			return 'ƒ'
		case 0x84:
			return '„'
		case 0x85:
			return '…'
		case 0x86:
			return '†'
		case 0x87:
			return '‡'
		case 0x88:
			return 'ˆ'
		case 0x89:
			return '‰'
		case 0x8A:
			return 'Š'
		case 0x8B:
			return '‹'
		case 0x8C:
			return 'Œ'
		case 0x8E:
			return 'Ž'
		case 0x91:
			return '‘'
		case 0x92:
			return '’'
		case 0x93:
			return '“'
		case 0x94:
			return '”'
		case 0x95:
			return '•'
		case 0x96:
			return '–'
		case 0x97:
			return '—'
		case 0x98:
			return '˜'
		case 0x99:
			return '™'
		case 0x9A:
			return 'š'
		case 0x9B:
			return '›'
		case 0x9C:
			return 'œ'
		case 0x9E:
			return 'ž'
		case 0x9F:
			return 'Ÿ'
		}
		return rune(b)
	default:
		return rune(b)
	}
}

func isRTFAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isRTFDigit(c byte) bool { return c >= '0' && c <= '9' }

func hexByte(hi, lo byte) byte {
	return hexNibble(hi)<<4 | hexNibble(lo)
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
