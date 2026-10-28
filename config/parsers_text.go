package config

import (
	"bufio"
	"fmt"
	"strings"
)

// parseINI 解析 INI 格式：
//
//	# 注释或 ; 注释
//	key0 = top            <- 不在任何 section 下的键，位于顶层
//	[server]              <- section，其下键归入 "server" 子树
//	host = 0.0.0.0
//	port = 8080
//
// 值一律保留为字符串，读取时按目标类型自动转换；值两侧成对引号会被剥掉。
func parseINI(data []byte) (map[string]any, error) {
	out := map[string]any{}
	section := "" // "" 表示顶层

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				return nil, fmt.Errorf("第 %d 行 section 语法错误: %s", lineNo, line)
			}
			section = strings.TrimSpace(line[1:end])
			continue
		}

		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("第 %d 行缺少 '=': %s", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("第 %d 行键名为空", lineNo)
		}
		value = unquoteINI(value)

		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}
		setPath(out, splitKey(fullKey), value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquoteINI 剥掉值两侧成对的单引号或双引号，并去掉行内注释（引号外的 " #" 或 " ;"）。
func unquoteINI(v string) string {
	// 行内注释：引号外出现 " #" 或 " ;" 时截断。
	if !strings.HasPrefix(v, "\"") && !strings.HasPrefix(v, "'") {
		if i := strings.IndexAny(v, "#;"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
	}
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseDotEnv 解析 .env 格式：
//
//	# 注释
//	DB_HOST=127.0.0.1
//	export DB_PORT=3306          <- 兼容 export 前缀
//	MSG="hello world"            <- 双引号支持 \n \t \" \\ 转义
//	TOKEN='raw $value'           <- 单引号原样保留
//	FLAGS=a,b,c                  <- 无引号时去掉行内 # 注释
//
// 键作为顶层键返回，值保留为字符串。
func parseDotEnv(data []byte) (map[string]any, error) {
	out := map[string]any{}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)

		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("第 %d 行缺少 '=': %s", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("第 %d 行键名为空", lineNo)
		}
		out[key] = unquoteEnvValue(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquoteEnvValue 处理 .env 值的引号与行内注释。
func unquoteEnvValue(v string) string {
	switch {
	case strings.HasPrefix(v, "\""):
		if end := findClosingQuote(v, '"'); end >= 0 {
			return unescapeEnv(v[1:end])
		}
		return unescapeEnv(strings.TrimSuffix(v[1:], "\""))
	case strings.HasPrefix(v, "'"):
		if end := findClosingQuote(v, '\''); end >= 0 {
			return v[1:end]
		}
		return strings.TrimSuffix(v[1:], "'")
	default:
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if i := strings.Index(v, "\t#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v
	}
}

// findClosingQuote 返回从下标 0 的引号开始的闭合引号下标，未找到返回 -1。
func findClosingQuote(s string, quote byte) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' && quote == '"' {
			i++ // 跳过转义字符
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

// unescapeEnv 展开双引号内的常见转义序列。
func unescapeEnv(s string) string {
	repl := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`)
	return repl.Replace(s)
}
