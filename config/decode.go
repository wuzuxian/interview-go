package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

// tagPriority 依次尝试的结构体标签，取第一个非 "-" 的非空值作为配置键。
var tagPriority = []string{"config", "mapstructure", "json", "yaml", "toml"}

// Unmarshal 将整棵配置树绑定到 v 指向的结构体（或 map）。
//
// 字段映射优先读取 config 标签，其次 mapstructure/json/yaml/toml 标签，最后用字段名。
// 字段级默认值与环境变量可直接通过标签声明：
//
//	type Server struct {
//	    Host    string        `config:"host" default:"0.0.0.0"`
//	    Port    int           `config:"port" default:"8080"`
//	    Timeout time.Duration `config:"timeout" default:"30s"`
//	    Token   string        `env:"APP_AUTH_TOKEN"` // env 标签为完整环境变量名，始终生效
//	}
//
// 每个字段的取值优先级：环境变量 > 配置树 > default 标签（未命中时保持零值）。
// 开启 AutomaticEnv 后，未声明 env 标签的字段也会按 "前缀+完整配置路径" 自动推导环境变量名。
func (c *Config) Unmarshal(v any) error {
	return c.UnmarshalKey("", v)
}

// UnmarshalKey 将某个键对应的子树绑定到 v 指向的结构体，key 为空等价于 Unmarshal。
func (c *Config) UnmarshalKey(key string, v any) error {
	if v == nil {
		return fmt.Errorf("config: 目标值不能为 nil")
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("config: 目标值必须是非 nil 指针")
	}

	var source map[string]any
	if key == "" {
		source = c.All()
	} else if sub, ok := c.Sub(key); ok {
		source = sub.All()
	} else {
		source = map[string]any{}
	}

	ctx := &decodeCtx{cfg: c, prefix: key}
	return decodeMap(source, rv.Elem(), ctx)
}

// decodeCtx 携带绑定过程中的上下文。
type decodeCtx struct {
	cfg    *Config
	prefix string // 当前绑定的键前缀，用于推导环境变量名
}

// decodeValue 将 source 值解码进目标 reflect.Value。
func decodeValue(source any, rv reflect.Value, ctx *decodeCtx) error {
	if rv.Kind() == reflect.Pointer {
		if source == nil {
			rv.Set(reflect.Zero(rv.Type()))
			return nil
		}
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		return decodeValue(source, rv.Elem(), ctx)
	}
	if rv.Type() == timeType {
		t, err := toTime(source)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(t))
		return nil
	}

	switch rv.Kind() {
	case reflect.Struct:
		return decodeStruct(source, rv, ctx)
	case reflect.Map:
		return decodeMap(source, rv, ctx)
	case reflect.Slice, reflect.Array:
		return decodeSlice(source, rv, ctx)
	default:
		return assignScalar(source, rv)
	}
}

// decodeStruct 将 map 形态的 source 逐字段解码进结构体。
func decodeStruct(source any, rv reflect.Value, ctx *decodeCtx) error {
	src, ok := source.(map[string]any)
	if !ok {
		if source == nil {
			return nil
		}
		return fmt.Errorf("期望映射结构，实际为 %T", source)
	}

	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		key, def, ok := fieldTags(field)
		if !ok {
			continue // 标签为 "-"，跳过该字段
		}

		// 依次尝试：显式 env 标签 > 自动推导的环境变量 > 配置树 > default 标签。
		val, found := lookupFieldEnv(field, joinKey(ctx.prefix, key), ctx)
		if !found {
			val, found = src[key]
		}
		if !found && def != "" {
			val, found = def, true
		}

		fieldCtx := &decodeCtx{cfg: ctx.cfg, prefix: joinKey(ctx.prefix, key)}
		if !found {
			// 配置与默认值都缺失时，非指针嵌套结构体仍按空映射递归，
			// 使其内部字段的环境变量与 default 标签有机会生效；其余字段保持零值。
			if fv := rv.Field(i); fv.Kind() == reflect.Struct && fv.Type() != timeType {
				if err := decodeValue(map[string]any{}, fv, fieldCtx); err != nil {
					return fmt.Errorf("字段 %s: %w", field.Name, err)
				}
			}
			continue
		}
		if err := decodeValue(val, rv.Field(i), fieldCtx); err != nil {
			return fmt.Errorf("字段 %s: %w", field.Name, err)
		}
	}
	return nil
}

// lookupFieldEnv 按字段查找环境变量。
// env 标签声明的变量名始终生效；否则在开启 AutomaticEnv 时按完整配置路径自动推导：
// 前缀 + 大写化路径（"." 替换为 "_"），如 server.port -> APP_SERVER_PORT。
func lookupFieldEnv(field reflect.StructField, path string, ctx *decodeCtx) (any, bool) {
	if name := fieldEnvName(field); name != "" {
		if v, ok := os.LookupEnv(name); ok {
			return v, true
		}
		return "", false
	}
	if v, ok := ctx.cfg.lookupEnvValue(path); ok {
		return v, true
	}
	return "", false
}

// decodeMap 将 map 形态的 source 解码进目标 map。
func decodeMap(source any, rv reflect.Value, ctx *decodeCtx) error {
	switch {
	case rv.Kind() == reflect.Struct:
		return decodeStruct(source, rv, ctx)
	case rv.Kind() == reflect.Map:
	default:
		return fmt.Errorf("期望映射结构，实际目标类型为 %s", rv.Type())
	}

	src, ok := source.(map[string]any)
	if !ok {
		if source == nil {
			return nil
		}
		return fmt.Errorf("期望映射结构，实际为 %T", source)
	}
	if rv.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("map 的键类型必须为 string，实际为 %s", rv.Type().Key())
	}
	if rv.IsNil() {
		rv.Set(reflect.MakeMap(rv.Type()))
	}
	for k, v := range src {
		ev := reflect.New(rv.Type().Elem()).Elem()
		if err := decodeValue(v, ev, ctx); err != nil {
			return fmt.Errorf("键 %q: %w", k, err)
		}
		rv.SetMapIndex(reflect.ValueOf(k).Convert(rv.Type().Key()), ev)
	}
	return nil
}

// decodeSlice 将数组形态的 source 解码进目标切片或数组。
func decodeSlice(source any, rv reflect.Value, ctx *decodeCtx) error {
	items, ok := source.([]any)
	if !ok {
		if s, isStr := source.(string); isStr && rv.Type().Elem().Kind() == reflect.String {
			ss, err := toStringSlice(s)
			if err != nil {
				return err
			}
			items = make([]any, len(ss))
			for i, v := range ss {
				items[i] = v
			}
		} else {
			if source == nil {
				return nil
			}
			return fmt.Errorf("期望数组，实际为 %T", source)
		}
	}

	if rv.Kind() == reflect.Array {
		if rv.Len() < len(items) {
			return fmt.Errorf("数组长度 %d 不足以容纳 %d 个元素", rv.Len(), len(items))
		}
		for i, item := range items {
			if err := decodeValue(item, rv.Index(i), ctx); err != nil {
				return fmt.Errorf("下标 %d: %w", i, err)
			}
		}
		return nil
	}

	out := reflect.MakeSlice(rv.Type(), 0, len(items))
	for i, item := range items {
		ev := reflect.New(rv.Type().Elem()).Elem()
		if err := decodeValue(item, ev, ctx); err != nil {
			return fmt.Errorf("下标 %d: %w", i, err)
		}
		out = reflect.Append(out, ev)
	}
	rv.Set(out)
	return nil
}

// fieldTags 解析字段标签，返回配置键名与 default 值；ok 为 false 表示该字段被 "-" 忽略。
func fieldTags(field reflect.StructField) (key string, def string, ok bool) {
	for _, tag := range tagPriority {
		v, has := field.Tag.Lookup(tag)
		if !has {
			continue
		}
		name := strings.Split(v, ",")[0]
		if name == "-" {
			return "", "", false
		}
		if name != "" {
			key = name
			break
		}
	}
	if key == "" {
		key = field.Name
	}
	if d, has := field.Tag.Lookup("default"); has {
		def = strings.Split(d, ",")[0]
	}
	return key, def, true
}

// fieldEnvName 返回字段显式声明的 env 标签名（完整环境变量名），未声明返回空串。
func fieldEnvName(field reflect.StructField) string {
	if v, ok := field.Tag.Lookup("env"); ok {
		return strings.Split(v, ",")[0]
	}
	return ""
}

// joinKey 拼接两级键路径。
func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
