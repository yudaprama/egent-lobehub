package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

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
			log.Fatalf("load config: %v", err)
		}
		log.Printf("loaded %d tools from %s", len(cfg.Tools), *configPath)
	} else {
		cfg, err = yamlconfig.LoadConfigFromBytes(embeddedConfigYAML)
		if err != nil {
			log.Fatalf("load embedded config: %v", err)
		}
		log.Printf("loaded %d tools from embedded config", len(cfg.Tools))
	}

	ctx := context.Background()
	rt, err = runtime.New(ctx, &runtime.Config{
		AgentName:           "LobeHubAgent",
		SystemPrompt:        cfg.SystemPrompt,
		ToolResultMaxLength: 25000,
	})
	if err != nil {
		log.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	// Build tools from config and register with runtime
	tools, err := tool.BuildToolsFromConfig(cfg)
	if err != nil {
		log.Fatalf("build tools: %v", err)
	}
	if err := rt.RegisterTools(tools); err != nil {
		log.Fatalf("register tools: %v", err)
	}

	if err := rt.Start(ctx); err != nil {
		log.Fatalf("start runtime: %v", err)
	}

	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/v1/tools", toolsHandler)

	addr := "0.0.0.0:" + *port
	log.Printf("egent-lobehub %s starting on %s", version, addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
