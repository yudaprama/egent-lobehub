package mcp

import (
	"context"
	"testing"
)

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	servers := m.GetServers()
	if len(servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(servers))
	}
	tools := m.GetAllTools()
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestLoadFromConfig_NoServers(t *testing.T) {
	m := NewManager()
	err := m.LoadFromConfig(context.Background(), MCPConfig{}, "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(m.GetServers()) != 0 {
		t.Error("expected 0 servers after loading empty config")
	}
}

func TestLoadFromConfig_DisabledServer(t *testing.T) {
	m := NewManager()
	cfg := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"disabled-server": {
				Enabled: false,
				Command: "echo",
			},
		},
	}
	err := m.LoadFromConfig(context.Background(), cfg, "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(m.GetServers()) != 0 {
		t.Error("disabled server should not be connected")
	}
}

func TestCallTool_ServerNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.CallTool(context.Background(), "nonexistent", "tool", nil)
	if err == nil {
		t.Error("expected error for nonexistent server")
	}
	if got := err.Error(); got != "server nonexistent not found" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestCallTool_ManagerClosed(t *testing.T) {
	m := NewManager()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := m.CallTool(context.Background(), "any", "tool", nil)
	if err == nil {
		t.Error("expected error when manager is closed")
	}
	if got := err.Error(); got != "manager is closed" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestClose_Idempotent(t *testing.T) {
	m := NewManager()
	if err := m.Close(); err != nil {
		t.Errorf("first close failed: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second close failed: %v", err)
	}
}

func TestGetServer_NotFound(t *testing.T) {
	m := NewManager()
	_, ok := m.GetServer("missing")
	if ok {
		t.Error("expected false for missing server")
	}
}

func TestLoadFromConfig_AllServersFail(t *testing.T) {
	m := NewManager()
	cfg := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"bad-server": {
				Enabled: true,
			},
		},
	}
	err := m.LoadFromConfig(context.Background(), cfg, "")
	if err == nil {
		t.Error("expected error when all servers fail")
	}
	if len(m.GetServers()) != 0 {
		t.Error("expected 0 servers after all failures")
	}
}
