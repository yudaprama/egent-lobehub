package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"egent-lobehub/middleware"
	"egent-lobehub/runtime"
	"egent-lobehub/tool"
	"egent-lobehub/yamlconfig"

	"github.com/joho/godotenv"
)

//go:embed agent_config.yaml
var embeddedConfigYAML []byte

var (
	version string
	rt      *runtime.Runtime
)

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to agent config file (uses embedded config if empty)")
	port := flag.String("port", "10531", "HTTP server port")
	flag.Parse()

	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	if *versionFlag {
		fmt.Printf("egent-lobehub %s\n", version)
		os.Exit(0)
	}

	// Load .env
	if exe, err := os.Executable(); err == nil {
		envPath := filepath.Join(filepath.Dir(exe), "..", ".env")
		godotenv.Load(envPath)
	}
	godotenv.Load()

	if *configPath != "" {
		configDir := filepath.Dir(*configPath)
		godotenv.Load(filepath.Join(configDir, ".env"))
	}

	var err error
	var cfg *yamlconfig.AgentConfig
	if *configPath != "" {
		cfg, err = yamlconfig.LoadConfig(*configPath)
		if err != nil {
			slog.Error("load config failed", "error", err)
			os.Exit(1)
		}
		slog.Info("loaded config from file", "tools", len(cfg.Tools), "path", *configPath)
	} else {
		cfg, err = yamlconfig.LoadConfigFromBytes(embeddedConfigYAML)
		if err != nil {
			slog.Error("load embedded config failed", "error", err)
			os.Exit(1)
		}
		slog.Info("loaded embedded config", "tools", len(cfg.Tools))
	}

	ctx := context.Background()

	// Build disabled tools set from YAML + DISABLED_TOOLS env var
	disabledTools := make(map[string]bool)
	for _, name := range cfg.DisabledTools {
		disabledTools[name] = true
	}
	if envDisabled := os.Getenv("DISABLED_TOOLS"); envDisabled != "" {
		for _, name := range strings.Split(envDisabled, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				disabledTools[name] = true
			}
		}
	}
	var permCfg *middleware.PermissionConfig
	if len(disabledTools) > 0 {
		permCfg = &middleware.PermissionConfig{DisabledTools: disabledTools}
		slog.Info("permission gate configured", "disabled_tools", len(disabledTools))
	}

	rt, err = runtime.New(ctx, &runtime.Config{
		AgentName:           "LobeHubAgent",
		SystemPrompt:        cfg.SystemPrompt,
		ToolResultMaxLength: 25000,
		PermissionConfig:    permCfg,
	})
	if err != nil {
		slog.Error("create runtime failed", "error", err)
		os.Exit(1)
	}
	defer rt.Close()

	// Build tools from config and register with runtime
	tools, err := tool.BuildToolsFromConfig(cfg)
	if err != nil {
		slog.Error("build tools failed", "error", err)
		os.Exit(1)
	}
	if err := rt.RegisterTools(tools); err != nil {
		slog.Error("register tools failed", "error", err)
		os.Exit(1)
	}

	if err := rt.Start(ctx); err != nil {
		slog.Error("start runtime failed", "error", err)
		os.Exit(1)
	}

	// HTTP server with timeouts
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/health/ready", readyHandler)
	mux.HandleFunc("/v1/tools", toolsHandler)

	addr := "0.0.0.0:" + *port
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "addr", addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutdown signal received, draining connections...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
