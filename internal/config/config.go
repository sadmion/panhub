// Package config 负责读取 config.yaml 并做默认值/环境变量覆盖。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServerConfig 是服务自身的配置。
type ServerConfig struct {
	Addr          string `yaml:"addr"`
	AdminPassword string `yaml:"admin_password"`
	DataDir       string `yaml:"data_dir"`
	// PublicBaseURL 用于生成 STRM 里的代理地址，例如 http://192.168.1.10:5211
	PublicBaseURL string `yaml:"public_base_url"`
}

// MountConfig 描述一个挂载点。
type MountConfig struct {
	Name     string         `yaml:"name"`
	Type     string         `yaml:"type"`
	ReadOnly bool           `yaml:"read_only"`
	Config   map[string]any `yaml:"config"`
}

// TaskConfig 描述一个定时任务。
type TaskConfig struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Mount    string `yaml:"mount"`
	Dir      string `yaml:"dir"`
	Target   string `yaml:"target"`
	Mode     string `yaml:"mode"`
	Interval string `yaml:"interval"`
	Enabled  bool   `yaml:"enabled"`
}

// Config 是完整配置。
type Config struct {
	Server ServerConfig  `yaml:"server"`
	Mounts []MountConfig `yaml:"mounts"`
	Tasks  []TaskConfig  `yaml:"tasks"`
}

// Default 返回带默认值的配置。
func Default() *Config {
	c := &Config{}
	c.Server.Addr = ":5211"
	c.Server.AdminPassword = "admin"
	c.Server.DataDir = "./data"
	return c
}

// Load 读取配置文件；文件不存在时返回默认配置（不算错误）。
func Load(path string) (*Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	applyEnv(c)
	c.normalize()
	return c, nil
}

// applyEnv 允许用环境变量覆盖（Docker 场景更方便）。
func applyEnv(c *Config) {
	if v := os.Getenv("PANHUB_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("PANHUB_PASSWORD"); v != "" {
		c.Server.AdminPassword = v
	}
	if v := os.Getenv("PANHUB_DATA_DIR"); v != "" {
		c.Server.DataDir = v
	}
	if v := os.Getenv("PANHUB_BASE_URL"); v != "" {
		c.Server.PublicBaseURL = v
	}
}

func (c *Config) normalize() {
	if strings.TrimSpace(c.Server.Addr) == "" {
		c.Server.Addr = ":5211"
	}
	if strings.TrimSpace(c.Server.DataDir) == "" {
		c.Server.DataDir = "./data"
	}
	if strings.TrimSpace(c.Server.AdminPassword) == "" {
		c.Server.AdminPassword = "admin"
	}
	c.Server.PublicBaseURL = strings.TrimSuffix(strings.TrimSpace(c.Server.PublicBaseURL), "/")
	for i := range c.Mounts {
		c.Mounts[i].Name = strings.TrimSpace(c.Mounts[i].Name)
		c.Mounts[i].Type = strings.TrimSpace(c.Mounts[i].Type)
		if c.Mounts[i].Config == nil {
			c.Mounts[i].Config = map[string]any{}
		}
	}
}
