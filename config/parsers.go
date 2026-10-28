package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// UnmarshalFormat 将指定格式的字节流解析为统一的嵌套 map[string]any 配置树。
// .env 格式的键（如 DB_HOST）会作为顶层键保留原始写法。
func UnmarshalFormat(format Format, data []byte) (map[string]any, error) {
	switch format {
	case FormatJSON:
		return parseJSON(data)
	case FormatYAML:
		return parseYAML(data)
	case FormatTOML:
		return parseTOML(data)
	case FormatINI:
		return parseINI(data)
	case FormatENV:
		return parseDotEnv(data)
	default:
		return nil, fmt.Errorf("config: 不支持的格式 %q", format)
	}
}

func parseJSON(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return normalizeStringMap(out), nil
}

func parseYAML(data []byte) (map[string]any, error) {
	var out any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return map[string]any{}, nil
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config: YAML 顶层必须是键值映射")
	}
	return normalizeStringMap(m), nil
}

func parseTOML(data []byte) (map[string]any, error) {
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return normalizeStringMap(out), nil
}
