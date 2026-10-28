package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadFile 加载一个配置文件并合并进当前配置树，后加载的文件覆盖先加载的同名键。
// 文件格式按扩展名自动识别：.json/.yaml/.yml/.toml/.ini/.env。
func (c *Config) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: 读取配置文件 %s 失败: %w", path, err)
	}

	format, err := DetectFormat(path)
	if err != nil {
		return err
	}
	tree, err := UnmarshalFormat(format, data)
	if err != nil {
		return fmt.Errorf("config: 解析配置文件 %s 失败: %w", path, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = mergeTree(c.values, tree)
	return nil
}

// ReadConfig 从任意 reader 读取指定格式的配置并合并进当前配置树。
func (c *Config) ReadConfig(r io.Reader, format Format) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("config: 读取配置流失败: %w", err)
	}
	tree, err := UnmarshalFormat(format, data)
	if err != nil {
		return fmt.Errorf("config: 解析 %s 格式配置失败: %w", format, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = mergeTree(c.values, tree)
	return nil
}

// DetectFormat 按文件扩展名判断格式；无法识别时返回错误。
func DetectFormat(path string) (Format, error) {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	case "toml":
		return FormatTOML, nil
	case "ini":
		return FormatINI, nil
	case "env":
		return FormatENV, nil
	default:
		return "", fmt.Errorf("config: 无法识别的配置文件格式: %s", path)
	}
}
