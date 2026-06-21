// Copyright (c) 2026 PicoClaw contributors
// SPDX-License-Identifier: MIT
// Ported from github.com/sipeed/picoclaw/pkg/mcp/manager.go

package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerTransport is an http.RoundTripper that adds custom headers to requests.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// ServerConnection represents a connection to an MCP server.
type ServerConnection struct {
	Name        string
	Config      MCPServerConfig
	Client      *mcp.Client
	Session     *mcp.ClientSession
	Tools       []*mcp.Tool
	reconnectMu sync.Mutex
}

// Manager manages multiple MCP server connections.
type Manager struct {
	servers map[string]*ServerConnection
	mu      sync.RWMutex
	closed  atomic.Bool
	wg      sync.WaitGroup
}

// NewManager creates a new MCP manager.
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*ServerConnection),
	}
}

// LoadFromConfig loads MCP servers from configuration.
// It connects to all enabled servers concurrently. If all enabled servers
// fail, it returns an aggregated error. Partial failures are logged but
// not fatal.
func (m *Manager) LoadFromConfig(ctx context.Context, cfg MCPConfig, workspacePath string) error {
	if len(cfg.Servers) == 0 {
		slog.Debug("mcp: no servers configured")
		return nil
	}

	slog.Info("mcp: initializing servers", "count", len(cfg.Servers))

	var wg sync.WaitGroup
	errs := make(chan error, len(cfg.Servers))
	enabledCount := 0

	for name, serverCfg := range cfg.Servers {
		if !serverCfg.Enabled {
			slog.Debug("mcp: skipping disabled server", "server", name)
			continue
		}

		enabledCount++
		wg.Add(1)
		go func(name string, serverCfg MCPServerConfig, workspace string) {
			defer wg.Done()

			if serverCfg.EnvFile != "" && !filepath.IsAbs(serverCfg.EnvFile) {
				if workspace == "" {
					err := fmt.Errorf("workspace path is empty while resolving relative envFile %q for server %s",
						serverCfg.EnvFile, name)
					slog.Error("mcp: invalid server config", "server", name, "error", err)
					errs <- err
					return
				}
				serverCfg.EnvFile = filepath.Join(workspace, serverCfg.EnvFile)
			}

			if err := m.ConnectServer(ctx, name, serverCfg); err != nil {
				slog.Error("mcp: failed to connect", "server", name, "error", err)
				errs <- fmt.Errorf("server %s: %w", name, err)
			}
		}(name, serverCfg, workspacePath)
	}

	wg.Wait()
	close(errs)

	var allErrors []error
	for err := range errs {
		allErrors = append(allErrors, err)
	}

	connectedCount := len(m.GetServers())

	if enabledCount > 0 && connectedCount == 0 {
		slog.Error("mcp: all servers failed to connect", "failed", len(allErrors), "total", enabledCount)
		return errors.Join(allErrors...)
	}

	if len(allErrors) > 0 {
		slog.Warn("mcp: some servers failed to connect",
			"failed", len(allErrors), "connected", connectedCount, "total", enabledCount)
	}

	slog.Info("mcp: initialization complete", "connected", connectedCount, "total", enabledCount)
	return nil
}

// ConnectServer connects to a single MCP server.
func (m *Manager) ConnectServer(ctx context.Context, name string, cfg MCPServerConfig) error {
	slog.Info("mcp: connecting", "server", name)
	conn, err := connectServer(ctx, name, cfg)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed.Load() {
		_ = conn.Session.Close()
		return fmt.Errorf("manager is closed")
	}

	m.servers[name] = conn
	slog.Info("mcp: connected", "server", name, "tools", len(conn.Tools))
	return nil
}

func connectServer(ctx context.Context, name string, cfg MCPServerConfig) (*ServerConnection, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "egent-lobehub",
		Version: "1.0.0",
	}, nil)

	var transport mcp.Transport
	transportType := EffectiveTransportType(cfg)
	if transportType == "" {
		return nil, fmt.Errorf("either URL or command must be provided for server %s", name)
	}

	switch transportType {
	case "sse", "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("URL is required for SSE/HTTP transport")
		}
		disableStandaloneSSE := transportType == "http"
		slog.Debug("mcp: using SSE/HTTP transport",
			"server", name, "url", cfg.URL, "disableStandaloneSSE", disableStandaloneSSE)

		sseTransport := &mcp.StreamableClientTransport{
			Endpoint:             cfg.URL,
			DisableStandaloneSSE: disableStandaloneSSE,
		}

		if len(cfg.Headers) > 0 {
			sseTransport.HTTPClient = &http.Client{
				Transport: &headerTransport{
					base:    http.DefaultTransport,
					headers: cfg.Headers,
				},
			}
			slog.Debug("mcp: added custom headers", "server", name, "count", len(cfg.Headers))
		}
		transport = sseTransport

	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("command is required for stdio transport")
		}
		slog.Debug("mcp: using stdio transport", "server", name, "command", cfg.Command)

		cmd := exec.CommandContext(ctx, expandHomeCommandPath(cfg.Command), cfg.Args...)

		envMap := make(map[string]string)
		for _, e := range cmd.Environ() {
			if idx := strings.Index(e, "="); idx > 0 {
				envMap[e[:idx]] = e[idx+1:]
			}
		}

		if cfg.EnvFile != "" {
			envVars, err := loadEnvFile(cfg.EnvFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load env file %s: %w", cfg.EnvFile, err)
			}
			for k, v := range envVars {
				envMap[k] = v
			}
			slog.Debug("mcp: loaded env file", "server", name, "vars", len(envVars))
		}

		for k, v := range cfg.Env {
			envMap[k] = v
		}

		env := make([]string, 0, len(envMap))
		for k, v := range envMap {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
		transport = &stdioTransport{Command: cmd}

	default:
		return nil, fmt.Errorf("unsupported transport type: %s (supported: stdio, sse, http)", transportType)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	initResult := session.InitializeResult()
	slog.Info("mcp: connected to server",
		"server", name,
		"serverName", initResult.ServerInfo.Name,
		"serverVersion", initResult.ServerInfo.Version,
		"protocol", initResult.ProtocolVersion)

	tools, err := listServerTools(ctx, name, session, initResult)
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	return &ServerConnection{
		Name:    name,
		Config:  cfg,
		Client:  client,
		Session: session,
		Tools:   tools,
	}, nil
}

func listServerTools(ctx context.Context, name string, session *mcp.ClientSession, initResult *mcp.InitializeResult) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	if initResult.Capabilities.Tools == nil {
		return tools, nil
	}

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			slog.Warn("mcp: error listing tool", "server", name, "error", err)
			continue
		}
		tools = append(tools, tool)
	}

	slog.Info("mcp: listed tools", "server", name, "count", len(tools))
	return tools, nil
}

// GetServers returns all connected servers.
func (m *Manager) GetServers() map[string]*ServerConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]*ServerConnection, len(m.servers))
	for k, v := range m.servers {
		result[k] = v
	}
	return result
}

// GetServer returns a specific server connection.
func (m *Manager) GetServer(name string) (*ServerConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conn, ok := m.servers[name]
	return conn, ok
}

// GetAllTools returns all tools from all connected servers.
func (m *Manager) GetAllTools() []*mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []*mcp.Tool
	for _, conn := range m.servers {
		tools = append(tools, conn.Tools...)
	}
	return tools
}

// CallTool calls a tool on a specific server.
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	if m.closed.Load() {
		m.mu.RUnlock()
		return nil, fmt.Errorf("manager is closed")
	}
	conn, ok := m.servers[serverName]
	if ok {
		m.wg.Add(1)
	}
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	defer m.wg.Done()

	params := &mcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	}

	result, err := conn.Session.CallTool(ctx, params)
	if err != nil {
		if shouldReconnectCallError(err) {
			slog.Warn("mcp: session lost during tool call, reconnecting",
				"server", serverName, "tool", toolName, "error", err)

			reconnectedConn, reconnectErr := m.reconnectServer(ctx, serverName, conn)
			if reconnectErr != nil {
				return nil, fmt.Errorf("failed to recover lost MCP session: %w", reconnectErr)
			}

			result, err = reconnectedConn.Session.CallTool(ctx, params)
			if err == nil {
				return result, nil
			}
		}
		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	return result, nil
}

func shouldReconnectCallError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), mcp.ErrSessionMissing.Error())
}

func (m *Manager) reconnectServer(ctx context.Context, serverName string, staleConn *ServerConnection) (*ServerConnection, error) {
	if staleConn == nil {
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	staleConn.reconnectMu.Lock()
	defer staleConn.reconnectMu.Unlock()

	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	currentConn, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	if currentConn != staleConn {
		return currentConn, nil
	}

	freshConn, err := connectServer(ctx, serverName, staleConn.Config)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("manager is closed")
	}

	currentConn, ok = m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	if currentConn == staleConn {
		m.servers[serverName] = freshConn
		staleToClose := staleConn
		m.mu.Unlock()
		_ = staleToClose.Session.Close()
		return freshConn, nil
	}

	m.mu.Unlock()
	_ = freshConn.Session.Close()
	return currentConn, nil
}

// Close closes all server connections.
func (m *Manager) Close() error {
	if m.closed.Swap(true) {
		return nil
	}

	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	slog.Info("mcp: closing all connections", "count", len(m.servers))

	var errs []error
	for name, conn := range m.servers {
		if err := conn.Session.Close(); err != nil {
			slog.Error("mcp: failed to close server", "server", name, "error", err)
			errs = append(errs, fmt.Errorf("server %s: %w", name, err))
		}
	}

	m.servers = make(map[string]*ServerConnection)

	if len(errs) > 0 {
		return fmt.Errorf("failed to close %d server(s): %w", len(errs), errors.Join(errs...))
	}
	return nil
}
