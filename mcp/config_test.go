package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveTransportType(t *testing.T) {
	tests := []struct {
		name string
		cfg  MCPServerConfig
		want string
	}{
		{"explicit stdio", MCPServerConfig{Type: "stdio", Command: "node"}, "stdio"},
		{"explicit sse", MCPServerConfig{Type: "sse", URL: "http://localhost:3000"}, "sse"},
		{"explicit http", MCPServerConfig{Type: "http", URL: "http://localhost:3000"}, "http"},
		{"streamable-http alias", MCPServerConfig{Type: "streamable-http", URL: "http://x"}, "http"},
		{"streamable_http alias", MCPServerConfig{Type: "streamable_http", URL: "http://x"}, "http"},
		{"streamablehttp alias", MCPServerConfig{Type: "streamablehttp", URL: "http://x"}, "http"},
		{"uppercase normalized", MCPServerConfig{Type: "STDIO", Command: "node"}, "stdio"},
		{"url implies sse", MCPServerConfig{URL: "http://localhost:3000"}, "sse"},
		{"command implies stdio", MCPServerConfig{Command: "node"}, "stdio"},
		{"empty config", MCPServerConfig{}, ""},
		{"explicit type wins over url", MCPServerConfig{Type: "stdio", URL: "http://x", Command: "node"}, "stdio"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveTransportType(tt.cfg)
			if got != tt.want {
				t.Errorf("EffectiveTransportType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	content := `# Comment line
KEY1=value1
KEY2="quoted value"
KEY3='single quoted'

EMPTY=
SPACED = spaced
`
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars, err := loadEnvFile(envFile)
	if err != nil {
		t.Fatal(err)
	}

	checks := map[string]string{
		"KEY1":   "value1",
		"KEY2":   "quoted value",
		"KEY3":   "single quoted",
		"EMPTY":  "",
		"SPACED": "spaced",
	}
	for k, want := range checks {
		got, ok := vars[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("key %q = %q, want %q", k, got, want)
		}
	}

	if len(vars) != len(checks) {
		t.Errorf("got %d vars, want %d", len(vars), len(checks))
	}
}

func TestLoadEnvFile_NotFound(t *testing.T) {
	_, err := loadEnvFile("/nonexistent/.env")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadEnvFile_InvalidFormat(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("NOEQUALS\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadEnvFile(envFile)
	if err == nil {
		t.Error("expected error for invalid format")
	}
}

func TestExpandHomeCommandPath(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"no tilde", "/usr/bin/node", "/usr/bin/node"},
		{"empty", "", ""},
		{"relative path", "./server", "./server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandHomeCommandPath(tt.command)
			if got != tt.want {
				t.Errorf("expandHomeCommandPath(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	t.Run("tilde only", func(t *testing.T) {
		got := expandHomeCommandPath("~")
		if got != home {
			t.Errorf("expandHomeCommandPath(~) = %q, want %q", got, home)
		}
	})

	t.Run("tilde slash", func(t *testing.T) {
		got := expandHomeCommandPath("~/bin/server")
		want := filepath.Join(home, "bin/server")
		if got != want {
			t.Errorf("expandHomeCommandPath(~/bin/server) = %q, want %q", got, want)
		}
	})
}
