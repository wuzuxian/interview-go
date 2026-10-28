package config

import (
	"encoding/json"
	"time"
)

// Get 按点分路径取值，按优先级查找：Set 覆盖值 > 环境变量 > 配置文件 > 默认值。
// 第二个返回值表示键是否存在。
func (c *Config) Get(key string) (any, bool) {
	path := splitKey(key)
	if len(path) == 0 {
		return nil, false
	}

	if v, ok := c.lookupEnvValue(key); ok {
		return v, true
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := lookupPath(c.overrides, path); ok {
		return v, true
	}
	if v, ok := lookupPath(c.values, path); ok {
		return v, true
	}
	return lookupPath(c.defaults, path)
}

// GetString 读取字符串值。值为数字或布尔等类型时自动转为字符串。
func (c *Config) GetString(key string) string {
	v, ok := c.Get(key)
	if !ok {
		return ""
	}
	s, _ := toString(v)
	return s
}

// GetBool 读取布尔值，字符串支持 "1/t/T/true/TRUE"、"0/f/F/false/FALSE"。
func (c *Config) GetBool(key string) bool {
	v, ok := c.Get(key)
	if !ok {
		return false
	}
	b, _ := toBool(v)
	return b
}

// GetInt 读取 int 值。
func (c *Config) GetInt(key string) int {
	return int(c.GetInt64(key))
}

// GetInt32 读取 int32 值。
func (c *Config) GetInt32(key string) int32 {
	return int32(c.GetInt64(key))
}

// GetInt64 读取 int64 值。
func (c *Config) GetInt64(key string) int64 {
	v, ok := c.Get(key)
	if !ok {
		return 0
	}
	n, _ := toInt64(v)
	return n
}

// GetUint 读取 uint 值。
func (c *Config) GetUint(key string) uint {
	return uint(c.GetUint64(key))
}

// GetUint64 读取 uint64 值。
func (c *Config) GetUint64(key string) uint64 {
	v, ok := c.Get(key)
	if !ok {
		return 0
	}
	n, _ := toUint64(v)
	return n
}

// GetFloat64 读取 float64 值。
func (c *Config) GetFloat64(key string) float64 {
	v, ok := c.Get(key)
	if !ok {
		return 0
	}
	f, _ := toFloat64(v)
	return f
}

// GetFloat32 读取 float32 值。
func (c *Config) GetFloat32(key string) float32 {
	return float32(c.GetFloat64(key))
}

// GetDuration 读取 time.Duration 值，字符串支持 "300ms"、"1h30m" 等格式，
// 纯数字按纳秒处理。
func (c *Config) GetDuration(key string) time.Duration {
	v, ok := c.Get(key)
	if !ok {
		return 0
	}
	d, _ := toDuration(v)
	return d
}

// GetTime 读取 time.Time 值，支持 RFC3339、"2006-01-02 15:04:05"、
// "2006-01-02" 及 Unix 秒级时间戳。
func (c *Config) GetTime(key string) time.Time {
	v, ok := c.Get(key)
	if !ok {
		return time.Time{}
	}
	t, _ := toTime(v)
	return t
}

// GetStringSlice 读取 []string，单个字符串按英文逗号拆分。
func (c *Config) GetStringSlice(key string) []string {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	s, _ := toStringSlice(v)
	return s
}

// GetIntSlice 读取 []int。
func (c *Config) GetIntSlice(key string) []int {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	rv, err := toGeneralSlice(v, reflectTypeInt)
	if err != nil {
		return nil
	}
	return rv.Interface().([]int)
}

// GetBoolSlice 读取 []bool。
func (c *Config) GetBoolSlice(key string) []bool {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	rv, err := toGeneralSlice(v, reflectTypeBool)
	if err != nil {
		return nil
	}
	return rv.Interface().([]bool)
}

// GetFloat64Slice 读取 []float64。
func (c *Config) GetFloat64Slice(key string) []float64 {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	rv, err := toGeneralSlice(v, reflectTypeFloat64)
	if err != nil {
		return nil
	}
	return rv.Interface().([]float64)
}

// GetDurationSlice 读取 []time.Duration。
func (c *Config) GetDurationSlice(key string) []time.Duration {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	rv, err := toGeneralSlice(v, durationType)
	if err != nil {
		return nil
	}
	return rv.Interface().([]time.Duration)
}

// GetStringMap 读取 map[string]any。
func (c *Config) GetStringMap(key string) map[string]any {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return normalizeStringMap(m)
	}
	return nil
}

// GetStringMapString 读取 map[string]string。
func (c *Config) GetStringMapString(key string) map[string]string {
	m := c.GetStringMap(key)
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, _ := toString(v)
		out[k] = s
	}
	return out
}

// JSON 返回某个键对应值的 JSON 字符串，便于调试输出。
func (c *Config) JSON(key string) string {
	v, ok := c.Get(key)
	if !ok {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
