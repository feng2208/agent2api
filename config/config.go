package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen  string        `yaml:"listen"`
	APIKeys []string      `yaml:"api_keys"`
	Agents  []AgentConfig `yaml:"agents"`
}

type ModelConfig struct {
	Name            string              `yaml:"name"`
	MaxIdleSessions int                 `yaml:"max_idle_sessions"`
	ExtraArgs       []string            `yaml:"extra_args"`
	Options         []map[string]string `yaml:"options"`
}

type AgentConfig struct {
	Name    string        `yaml:"name"`
	Command string        `yaml:"command"`
	Cwd     string        `yaml:"cwd"`
	Args    []string      `yaml:"args"`
	Models  []ModelConfig `yaml:"models"`
}

func (a *AgentConfig) HasExtraArgs() bool {
	for _, m := range a.Models {
		if len(m.ExtraArgs) > 0 {
			return true
		}
	}
	return false
}

func (a *AgentConfig) MaxIdleSessions() int {
	max := 0
	for _, m := range a.Models {
		if m.MaxIdleSessions > max {
			max = m.MaxIdleSessions
		}
	}
	return max
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:8080"
	}

	for i, agent := range cfg.Agents {
		if agent.Cwd == "" {
			return nil, fmt.Errorf("agents[%d] %q cwd is required", i, agent.Name)
		}
		if !filepath.IsAbs(agent.Cwd) {
			return nil, fmt.Errorf("agents[%d] %q cwd must be an absolute path: %s", i, agent.Name, agent.Cwd)
		}
		info, err := os.Stat(agent.Cwd)
		if err != nil {
			return nil, fmt.Errorf("agents[%d] %q cwd is not accessible: %w", i, agent.Name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("agents[%d] %q cwd is not a directory: %s", i, agent.Name, agent.Cwd)
		}
	}

	return cfg, nil
}

func (c *Config) AgentByName(name string) (AgentConfig, bool) {
	for _, agent := range c.Agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return AgentConfig{}, false
}
