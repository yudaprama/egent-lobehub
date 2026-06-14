package config

import (
	"reflect"
	"testing"
)

func TestMergeAgentConfig_NilAgent_ReturnsNil(t *testing.T) {
	got := MergeAgentConfig(DefaultAgentConfig, nil, nil, nil, "")
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestMergeAgentConfig_PrecedenceChain(t *testing.T) {
	defaults := map[string]any{"model": "default-model", "temperature": 0.5}
	server := map[string]any{"model": "server-model"}
	user := map[string]any{"model": "user-model"}
	agent := map[string]any{"temperature": 0.9}

	got := MergeAgentConfig(defaults, server, user, agent, "")

	if got["model"] != "user-model" {
		t.Errorf("expected model=user-model, got %v", got["model"])
	}
	if got["temperature"] != 0.9 {
		t.Errorf("expected temperature=0.9, got %v", got["temperature"])
	}
}

func TestMergeAgentConfig_AgentOverridesAll(t *testing.T) {
	defaults := map[string]any{"model": "default-model", "provider": "zhipu"}
	server := map[string]any{"model": "server-model"}
	user := map[string]any{"model": "user-model"}
	agent := map[string]any{"model": "agent-model", "custom": "value"}

	got := MergeAgentConfig(defaults, server, user, agent, "")

	if got["model"] != "agent-model" {
		t.Errorf("expected model=agent-model, got %v", got["model"])
	}
	if got["provider"] != "zhipu" {
		t.Errorf("expected provider=zhipu (from defaults), got %v", got["provider"])
	}
	if got["custom"] != "value" {
		t.Errorf("expected custom=value (from agent), got %v", got["custom"])
	}
}

func TestMergeAgentConfig_WorkspaceSkipsUserLayer(t *testing.T) {
	defaults := map[string]any{"model": "default-model"}
	server := map[string]any{"model": "server-model"}
	user := map[string]any{"model": "user-model"}
	agent := map[string]any{}

	got := MergeAgentConfig(defaults, server, user, agent, "workspace-123")

	if got["model"] != "agent-model-not-set" && got["model"] != "default-model" {
		if got["model"] == "user-model" {
			t.Errorf("workspace scope should skip user layer, but got model=user-model")
		}
	}
	_ = got
}

func TestMergeAgentConfig_NestedMapMerge(t *testing.T) {
	defaults := map[string]any{
		"chatConfig": map[string]any{"temperature": 0.5, "topP": 0.9},
	}
	agent := map[string]any{
		"chatConfig": map[string]any{"temperature": 0.7},
	}

	got := MergeAgentConfig(defaults, nil, nil, agent, "")

	chatConfig, ok := got["chatConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected chatConfig to be a map, got %T", got["chatConfig"])
	}
	if chatConfig["temperature"] != 0.7 {
		t.Errorf("expected temperature=0.7, got %v", chatConfig["temperature"])
	}
	if chatConfig["topP"] != 0.9 {
		t.Errorf("expected topP=0.9 (inherited), got %v", chatConfig["topP"])
	}
}

func TestCleanNil_RemovesEmptyValues(t *testing.T) {
	m := map[string]any{
		"keep":      "value",
		"empty":     "",
		"nilVal":    nil,
		"emptyList": []string{},
	}
	got := cleanNil(m)
	if _, ok := got["empty"]; ok {
		t.Error("empty string should be removed")
	}
	if _, ok := got["nilVal"]; ok {
		t.Error("nil value should be removed")
	}
	if _, ok := got["emptyList"]; ok {
		t.Error("empty list should be removed")
	}
	if got["keep"] != "value" {
		t.Error("non-empty value should be kept")
	}
}

func TestFromMap_ConvertsRawToTyped(t *testing.T) {
	raw := map[string]any{
		"id":           "agent-123",
		"model":        "gpt-4",
		"systemPrompt": "be helpful",
		"chatConfig": map[string]any{
			"temperature": 0.5,
		},
	}
	cfg, err := FromMap(raw)
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	if cfg.ID != "agent-123" {
		t.Errorf("expected id=agent-123, got %q", cfg.ID)
	}
	if cfg.Model != "gpt-4" {
		t.Errorf("expected model=gpt-4, got %q", cfg.Model)
	}
	if cfg.SystemPrompt != "be helpful" {
		t.Errorf("expected systemPrompt='be helpful', got %q", cfg.SystemPrompt)
	}
	if cfg.ChatConfig == nil {
		t.Fatal("expected ChatConfig to be set")
	}
	if cfg.ChatConfig.Temperature == nil || *cfg.ChatConfig.Temperature != 0.5 {
		t.Errorf("expected temperature=0.5, got %v", cfg.ChatConfig.Temperature)
	}
}

func TestDeepMerge_MutatesDst(t *testing.T) {
	dst := map[string]any{"a": 1}
	src := map[string]any{"b": 2}
	got := deepMerge(dst, src)
	if got["b"] != 2 {
		t.Error("deepMerge should add src values to dst")
	}
	if _, ok := dst["b"]; !ok {
		t.Error("deepMerge mutates dst in place")
	}
}

func TestDeepMerge_NestedReplacement(t *testing.T) {
	dst := map[string]any{
		"nested": map[string]any{"a": 1, "b": 2},
	}
	src := map[string]any{
		"nested": map[string]any{"a": 99},
	}
	got := deepMerge(dst, src)
	nested := got["nested"].(map[string]any)
	if nested["a"] != 99 {
		t.Errorf("expected a=99, got %v", nested["a"])
	}
	if nested["b"] != 2 {
		t.Errorf("expected b=2 (inherited), got %v", nested["b"])
	}
}

func TestLoadServerDefaults_EmptyEnv_ReturnsNil(t *testing.T) {
	t.Setenv("SERVER_DEFAULT_AGENT_CONFIG", "")
	got := LoadServerDefaults()
	if got != nil {
		t.Errorf("expected nil for empty env, got %v", got)
	}
}

func TestLoadServerDefaults_InvalidJSON_ReturnsNil(t *testing.T) {
	t.Setenv("SERVER_DEFAULT_AGENT_CONFIG", "not json")
	got := LoadServerDefaults()
	if got != nil {
		t.Errorf("expected nil for invalid JSON, got %v", got)
	}
}

func TestLoadServerDefaults_ValidJSON(t *testing.T) {
	t.Setenv("SERVER_DEFAULT_AGENT_CONFIG", `{"model":"server-model"}`)
	got := LoadServerDefaults()
	if !reflect.DeepEqual(got["model"], "server-model") {
		t.Errorf("expected model=server-model, got %v", got["model"])
	}
}
