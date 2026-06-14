package yamlconfig

import (
	"os"

	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Version      string    `yaml:"version"`
	SystemPrompt string    `yaml:"system_prompt"`
	Tools        []ToolDef `yaml:"tools"`
}

type ToolDef struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	URL         string            `yaml:"url"`
	Method      string            `yaml:"method,omitempty"`
	Parameters  []Parameter       `yaml:"parameters,omitempty"`
	HTTPHeaders map[string]string `yaml:"http_headers,omitempty"`
}

type Parameter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Default     any    `yaml:"default,omitempty"`
	InPath      bool   `yaml:"in_path,omitempty"`
}

func LoadConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadConfigFromBytes(data)
}

func LoadConfigFromBytes(data []byte) (*AgentConfig, error) {
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
