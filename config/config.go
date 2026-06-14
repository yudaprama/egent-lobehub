package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// AgentConfig is the runtime configuration for a LobeHub agent,
// produced by merging four layers: DEFAULT → server → user → agent.
// Mirrors @lobechat/types LobeAgentConfig.
type AgentConfig struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Title            string         `json:"title,omitempty"`
	Description      string         `json:"description,omitempty"`
	SystemPrompt     string         `json:"systemPrompt,omitempty"`
	Model            string         `json:"model,omitempty"`
	Provider         string         `json:"provider,omitempty"`
	Tools            []string       `json:"tools,omitempty"`
	EnabledPlugins   []string       `json:"enabledPlugins,omitempty"`
	EnabledKnowledge []string       `json:"enabledKnowledgeBases,omitempty"`
	ChatConfig       *ChatConfig    `json:"chatConfig,omitempty"`
	PluginConfig     map[string]any `json:"pluginConfig,omitempty"`
	OpeningMessage   string         `json:"openingMessage,omitempty"`
	OpeningQuestions []string       `json:"openingQuestions,omitempty"`
	// Raw preserves the merged map for fields not in the typed struct.
	Raw map[string]any `json:"-"`
}

// ChatConfig groups LLM sampling parameters.
type ChatConfig struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	MaxTokens        *int     `json:"maxTokens,omitempty"`
	PresencePenalty  *float64 `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequencyPenalty,omitempty"`
}

// DefaultAgentConfig is the hardcoded baseline (LobeHub DEFAULT_AGENT_CONFIG).
// Every agent inherits these unless explicitly overridden by a higher layer.
var DefaultAgentConfig = map[string]any{
	"model":    "custom/glm-5.1",
	"provider": "zhipu",
	"chatConfig": map[string]any{
		"temperature": 0.7,
		"topP":        0.9,
	},
}

// MergeAgentConfig merges four config layers in precedence order:
//   1. defaults  — hardcoded baseline (DEFAULT_AGENT_CONFIG)
//   2. server    — env-driven server defaults
//   3. user      — per-user settings (skipped when workspaceID is set)
//   4. agent     — the agent's own persisted config
//
// Returns nil if agent is nil/empty. Workspace-scoped reads skip the
// user layer to prevent personal defaults from leaking into shared agents.
func MergeAgentConfig(defaults, server, user, agent map[string]any, workspaceID string) map[string]any {
	if agent == nil {
		return nil
	}
	base := deepMerge(defaults, server)
	cleanedAgent := cleanNil(agent)
	if workspaceID != "" {
		return deepMerge(base, cleanedAgent)
	}
	userLayer := cleanNil(user)
	if userLayer != nil {
		base = deepMerge(base, userLayer)
	}
	return deepMerge(base, cleanedAgent)
}

// deepMerge recursively merges src into dst, with src taking precedence.
// Nested map[string]any values are merged; other types are replaced.
func deepMerge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	if src == nil {
		return dst
	}
	for k, v := range src {
		if existing, ok := dst[k]; ok {
			if existingMap, eOk := existing.(map[string]any); eOk {
				if srcMap, sOk := v.(map[string]any); sOk {
					dst[k] = deepMerge(existingMap, srcMap)
					continue
				}
			}
		}
		dst[k] = v
	}
	return dst
}

// cleanNil removes nil/empty values from a config map.
// Prevents empty YAML/JSON values from overwriting real defaults
// (mirrors LobeHub's cleanObject() in services/agent/index.ts).
func cleanNil(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if s, ok := v.([]any); ok && len(s) == 0 {
			continue
		}
		if s, ok := v.([]string); ok && len(s) == 0 {
			continue
		}
		out[k] = v
	}
	return out
}

// FromMap converts a raw merged map to a typed AgentConfig.
func FromMap(m map[string]any) (*AgentConfig, error) {
	if m == nil {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	cfg := &AgentConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal into AgentConfig: %w", err)
	}
	cfg.Raw = m
	return cfg, nil
}

// LoadServerDefaults reads env-driven server defaults.
// SERVER_DEFAULT_AGENT_CONFIG env var is a JSON blob that overrides built-in defaults.
func LoadServerDefaults() map[string]any {
	raw := os.Getenv("SERVER_DEFAULT_AGENT_CONFIG")
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}
