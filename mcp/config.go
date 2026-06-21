// Copyright (c) 2026 PicoClaw contributors
// SPDX-License-Identifier: MIT
// Ported from github.com/sipeed/picoclaw/pkg/mcp/ and pkg/config/mcp_transport.go

package mcp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MCPServerConfig defines configuration for a single MCP server.
type MCPServerConfig struct {
	Enabled bool              `json:"enabled"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	EnvFile string            `json:"env_file,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig defines configuration for all MCP servers.
type MCPConfig struct {
	Servers map[string]MCPServerConfig `json:"servers,omitempty"`
}

// EffectiveTransportType returns the normalized transport type.
// Ported from picoclaw/pkg/config/mcp_transport.go (MIT).
func EffectiveTransportType(cfg MCPServerConfig) string {
	t := strings.ToLower(strings.TrimSpace(cfg.Type))
	switch t {
	case "streamable-http", "streamable_http", "streamablehttp":
		return "http"
	case "":
		if cfg.URL != "" {
			return "sse"
		}
		if cfg.Command != "" {
			return "stdio"
		}
		return ""
	default:
		return t
	}
}

// expandHomeCommandPath expands a leading ~ to the user's home directory.
func expandHomeCommandPath(command string) string {
	if command == "" || command[0] != '~' {
		return command
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return command
	}
	if command == "~" {
		return home
	}
	if strings.HasPrefix(command, "~/") || strings.HasPrefix(command, "~\\") {
		return filepath.Join(home, command[2:])
	}
	return command
}

// loadEnvFile loads environment variables from a .env format file.
func loadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open env file: %w", err)
	}
	defer file.Close()

	envVars := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format at line %d: %s", lineNum, line)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if key == "" {
			return nil, fmt.Errorf("invalid format at line %d: empty key", lineNum)
		}

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		envVars[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file: %w", err)
	}

	return envVars, nil
}
