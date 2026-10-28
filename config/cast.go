package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// 本文件提供 any -> 目标基础类型的宽松转换：
// 环境变量和 INI 等来源的值一律是字符串，读取时需要按目标类型自动转换。

var timeType = reflect.TypeOf(time.Time{})
var durationType = reflect.TypeOf(time.Duration(0))

var (
	reflectTypeInt     = reflect.TypeOf(0)
	reflectTypeBool    = reflect.TypeOf(false)
	reflectTypeFloat64 = reflect.TypeOf(float64(0))
)

// toString 将任意基础类型转换为字符串。
func toString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int8:
		return strconv.FormatInt(int64(t), 10), true
	case int16:
		return strconv.FormatInt(int64(t), 10), true
	case int32:
		return strconv.FormatInt(int64(t), 10), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case uint:
		return strconv.FormatUint(uint64(t), 10), true
	case uint8:
		return strconv.FormatUint(uint64(t), 10), true
	case uint16:
		return strconv.FormatUint(uint64(t), 10), true
	case uint32:
		return strconv.FormatUint(uint64(t), 10), true
	case uint64:
		return strconv.FormatUint(t, 10), true
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case time.Duration:
		return t.String(), true
	case time.Time:
		return t.Format(time.RFC3339Nano), true
	case []byte:
		return string(t), true
	case json.Number:
		return t.String(), true
	default:
		return "", false
	}
}

// toInt64 将任意基础类型转换为 int64。
func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case int:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case int64:
		return t, nil
	case uint:
		return int64(t), nil
	case uint8:
		return int64(t), nil
	case uint16:
		return int64(t), nil
	case uint32:
		return int64(t), nil
	case uint64:
		return int64(t), nil
	case float32:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case time.Duration:
		return int64(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 0, 64); err == nil {
			return n, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), nil
		}
		if b, err := strconv.ParseBool(s); err == nil {
			if b {
				return 1, nil
			}
			return 0, nil
		}
		return 0, fmt.Errorf("无法将 %q 转换为整数", t)
	case json.Number:
		return toInt64(string(t))
	default:
		return 0, fmt.Errorf("无法将 %T 转换为整数", v)
	}
}

// toUint64 将任意基础类型转换为 uint64。
func toUint64(v any) (uint64, error) {
	n, err := toInt64(v)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("负数 %d 无法转换为无符号整数", n)
	}
	return uint64(n), nil
}

// toFloat64 将任意基础类型转换为 float64。
func toFloat64(v any) (float64, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case float32:
		return float64(t), nil
	case float64:
		return t, nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	default:
		n, err := toInt64(v)
		if err == nil {
			return float64(n), nil
		}
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f, nil
			}
		}
		return 0, fmt.Errorf("无法将 %T 转换为浮点数", v)
	}
}

// toBool 将任意基础类型转换为 bool，字符串支持 1/t/T/true/TRUE、0/f/F/false/FALSE。
func toBool(v any) (bool, error) {
	switch t := v.(type) {
	case nil:
		return false, nil
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("无法将 %q 转换为布尔值", t)
		}
		return b, nil
	default:
		n, err := toInt64(v)
		if err != nil {
			return false, fmt.Errorf("无法将 %T 转换为布尔值", v)
		}
		return n != 0, nil
	}
}

// toDuration 将任意基础类型转换为 time.Duration：
// 字符串支持 "300ms"、"1h30m" 等格式，纯数字按纳秒处理（与标准库一致）。
func toDuration(v any) (time.Duration, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case time.Duration:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if d, err := time.ParseDuration(s); err == nil {
			return d, nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Duration(n), nil
		}
		return 0, fmt.Errorf("无法将 %q 转换为时间段", t)
	default:
		n, err := toInt64(v)
		if err != nil {
			return 0, fmt.Errorf("无法将 %T 转换为时间段", v)
		}
		return time.Duration(n), nil
	}
}

// toTime 将任意基础类型转换为 time.Time，支持 RFC3339、日期、Unix 时间戳等常见格式。
func toTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		layouts := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05",
			"2006-01-02 15:04",
			"2006-01-02",
		}
		for _, layout := range layouts {
			if ts, err := time.ParseInLocation(layout, s, time.Local); err == nil {
				return ts, nil
			}
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0), nil
		}
		return time.Time{}, fmt.Errorf("无法将 %q 转换为时间", t)
	default:
		n, err := toInt64(v)
		if err != nil {
			return time.Time{}, fmt.Errorf("无法将 %T 转换为时间", v)
		}
		return time.Unix(n, 0), nil
	}
}

// toStringSlice 转换为 []string：单个字符串按逗号拆分（适配环境变量 "a,b,c" 的写法）。
func toStringSlice(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), t...), nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return []string{}, nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := toString(item)
			if !ok {
				return nil, fmt.Errorf("无法将 %T 转换为字符串", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		if s, ok := toString(v); ok {
			return toStringSlice(s)
		}
		return nil, fmt.Errorf("无法将 %T 转换为字符串切片", v)
	}
}

// toGeneralSlice 将值转换为 elemType 类型的切片，供 GetIntSlice 等以及结构体绑定使用。
func toGeneralSlice(v any, elemType reflect.Type) (reflect.Value, error) {
	items, ok := v.([]any)
	if !ok {
		if str, isStr := v.(string); isStr && elemType.Kind() == reflect.String {
			ss, err := toStringSlice(str)
			if err != nil {
				return reflect.Value{}, err
			}
			items = make([]any, len(ss))
			for i, s := range ss {
				items[i] = s
			}
		} else {
			return reflect.Value{}, fmt.Errorf("期望数组，实际为 %T", v)
		}
	}

	out := reflect.MakeSlice(reflect.SliceOf(elemType), 0, len(items))
	for _, item := range items {
		elem := reflect.New(elemType).Elem()
		if err := assignScalar(item, elem); err != nil {
			return reflect.Value{}, err
		}
		out = reflect.Append(out, elem)
	}
	return out, nil
}

// assignScalar 将 any 值写入一个非切片、非结构体的 reflect.Value。
func assignScalar(v any, rv reflect.Value) error {
	if !rv.CanSet() {
		return fmt.Errorf("目标值不可设置")
	}
	switch rv.Kind() {
	case reflect.String:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("无法将 %T 转换为字符串", v)
		}
		rv.SetString(s)
		return nil
	case reflect.Bool:
		b, err := toBool(v)
		if err != nil {
			return err
		}
		rv.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if rv.Type() == durationType {
			d, err := toDuration(v)
			if err != nil {
				return err
			}
			rv.SetInt(int64(d))
			return nil
		}
		n, err := toInt64(v)
		if err != nil {
			return err
		}
		if rv.OverflowInt(n) {
			return fmt.Errorf("值 %d 超出 %s 范围", n, rv.Type())
		}
		rv.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := toUint64(v)
		if err != nil {
			return err
		}
		if rv.OverflowUint(n) {
			return fmt.Errorf("值 %d 超出 %s 范围", n, rv.Type())
		}
		rv.SetUint(n)
		return nil
	case reflect.Float32, reflect.Float64:
		f, err := toFloat64(v)
		if err != nil {
			return err
		}
		rv.SetFloat(f)
		return nil
	case reflect.Interface:
		if v == nil {
			rv.Set(reflect.Zero(rv.Type()))
			return nil
		}
		rv.Set(reflect.ValueOf(v))
		return nil
	default:
		return fmt.Errorf("不支持的目标类型 %s", rv.Type())
	}
}
