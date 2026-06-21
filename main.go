package main

import (
	"context"
	_ "embed"
	"encoding/json"
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

	"egent-lobehub/connectors/composio"
	composioeino "egent-lobehub/connectors/composio/eino"
	"egent-lobehub/keyvault"
	"egent-lobehub/knowledge"
	"egent-lobehub/lock"
	"egent-lobehub/mcp"
	mcpeino "egent-lobehub/mcp/eino"
	"egent-lobehub/memory"
	"egent-lobehub/middleware"
	"egent-lobehub/runtime"
	"egent-lobehub/runtime/task"
	"egent-lobehub/tool"
	"egent-lobehub/tracing"
	"egent-lobehub/yamlconfig"

	einoTool "github.com/cloudwego/eino/components/tool"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/joho/godotenv"
	temporalclient "go.temporal.io/sdk/client"
)

//go:embed agent_config.yaml
var embeddedConfigYAML []byte

var (
	version string
	rt      *runtime.Runtime
	dbPool  *pgxpool.Pool
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

	// Initialize OpenTelemetry tracing (after ctx is declared)
	tracingEnabled := os.Getenv("OTLP_ENABLED") == "1" || os.Getenv("OTLP_ENABLED") == "true"
	tracingShutdown, tracingErr := tracing.Init(ctx, tracing.Config{
		Enabled:     tracingEnabled,
		Endpoint:    tracing.ParseEndpoint(),
		ServiceName: "egent-lobehub",
		SampleRate:  tracing.ParseSampleRate(),
	})
	if tracingErr != nil {
		slog.Error("tracing init failed", "error", tracingErr)
		os.Exit(1)
	}
	defer tracingShutdown(context.Background())

	// Shared Postgres pool from KNOWLEDGE_PG_DSN. When unset the service
	// runs without DB-backed handlers (same as today).
	dbPool = initDBPool(ctx)
	if dbPool != nil {
		defer dbPool.Close()
	}

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
		Lock:                initEditLock(),
		KeyVault:            initKeyVault(),
	})
	if err != nil {
		slog.Error("create runtime failed", "error", err)
		os.Exit(1)
	}
	defer rt.Close()

	// Build tools from config and register with runtime.
	// These are the HTTP API tools from agent_config.yaml. Memory and
	// knowledge tools are added separately below because they need a
	// backing service.
	tools, err := tool.BuildToolsFromConfig(cfg)
	if err != nil {
		slog.Error("build tools failed", "error", err)
		os.Exit(1)
	}
	if err := rt.RegisterTools(tools); err != nil {
		slog.Error("register tools failed", "error", err)
		os.Exit(1)
	}

	// Always wire memory tools (4 tools). They use an in-memory store — swap
	// for a Postgres-backed store in production.
	memMgr := memory.NewManager(memory.NewInMemoryStore())
	if err := rt.RegisterTools(memoryTools(memMgr)); err != nil {
		slog.Error("register memory tools failed", "error", err)
		os.Exit(1)
	}
	slog.Info("memory tools registered", "count", 4)

	// Optional: knowledge_search tool. Wired when the shared Postgres pool
	// is available (DATABASE_URL or KNOWLEDGE_PG_DSN). The embedder reads
	// OPENAI_API_KEY (or any OpenAI-compatible URL via
	// OPENAI_EMBEDDINGS_URL). When the pool or embedder is missing, the
	// tool is skipped and the agent runs without RAG.
	if dbPool != nil {
		slog.Info("knowledge: wiring knowledge_search tool")
		embedder := buildKnowledgeEmbedder()
		if embedder == nil {
			slog.Warn("knowledge: embedder not configured; tool will report 'not configured' for every query")
		}
		kSvc, err := knowledge.NewService(ctx, dbPool, embedder)
		if err != nil {
			slog.Error("knowledge: create service failed", "error", err)
			os.Exit(1)
		}
		if kSvc != nil {
			defer kSvc.Close()
			knowledgeTool := knowledge.NewKnowledgeSearchTool(kSvc)
			if err := rt.RegisterTools([]einoTool.BaseTool{knowledgeTool}); err != nil {
				slog.Error("knowledge: register tool failed", "error", err)
				os.Exit(1)
			}
			slog.Info("knowledge: knowledge_search tool registered")
		}
	} else {
		slog.Info("knowledge: no shared Postgres pool; knowledge_search tool disabled")
	}

	// Optional: Composio 3rd-party SaaS tools (Slack/Gmail/GitHub/etc.).
	// Wired when both COMPOSIO_API_KEY and PREST_URL are set. The agent
	// gets one tool per action per connected app for every user who has
	// an ACTIVE Composio connection (the adapter returns NotConnectedError
	// for users without one, which the runtime maps to ErrorKindStop).
	//
	// Tools are registered at startup because their slugs are stable
	// across requests (the manifest is per-toolkit, not per-user). The
	// per-user connection lookup happens at InvokableRun time via the
	// RESTAccountStore.
	// Optional: Composio 3rd-party SaaS tools (Slack/Gmail/GitHub/etc.).
	// Wired when COMPOSIO_API_KEY is set. The agent gets one tool per
	// action per connected app for every user who has an ACTIVE
	// Composio connection; the adapter returns NotConnectedError for
	// users without one, which the runtime maps to ErrorKindStop.
	//
	// The per-user connection lookup happens at InvokableRun time via
	// RESTAccountStore (pREST-backed) — tools are registered at
	// startup because their slugs are stable across requests.
	if composioKey := os.Getenv("COMPOSIO_API_KEY"); composioKey != "" {
		prestURL := os.Getenv("PREST_URL")
		if prestURL == "" {
			prestURL = "http://localhost:3000"
		}
		store := composioeino.NewRESTAccountStore(prestURL)
		client, err := composio.NewComposer(composioKey)
		if err != nil {
			slog.Error("composio: create client failed", "error", err)
			os.Exit(1)
		}
		if client != nil && store != nil {
			// Expose to composio_handlers.go so the UI can drive
			// connection lifecycle through Go instead of Next.js.
			composioCli = client
			composioAccountStore = store

			builder := composioeino.NewBuilder(client, store, slog.Default())
			// Build for all catalog apps up-front. The builder skips
			// apps whose manifest fetch fails (e.g. rate-limited), so
			// a single broken toolkit doesn't block startup.
			composioTools, err := builder.Build(ctx)
			if err != nil {
				slog.Error("composio: build tools failed", "error", err)
				os.Exit(1)
			}
			// Register each Composio tool as an Eino BaseTool.
			bases := make([]einoTool.BaseTool, 0, len(composioTools))
			for _, ct := range composioTools {
				bases = append(bases, ct)
			}
			if err := rt.RegisterTools(bases); err != nil {
				slog.Error("composio: register tools failed", "error", err)
				os.Exit(1)
			}
			slog.Info("composio: tools registered", "summary", composioeino.FormatToolsForLog(composioTools))
		} else {
			slog.Info("composio: client or store not configured; Composio tools disabled")
		}
	} else {
		slog.Info("composio: COMPOSIO_API_KEY not set; Composio tools disabled")
	}

	// Optional: MCP servers. Wired when MCP config is provided via
	// MCP_SERVERS env var (JSON) or mcp.json file in CWD.
	mcpCfg := loadMCPConfig()
	if len(mcpCfg.Servers) > 0 {
		slog.Info("mcp: initializing servers", "count", len(mcpCfg.Servers))
		mcpMgr := mcp.NewManager()
		if err := mcpMgr.LoadFromConfig(ctx, mcpCfg, ""); err != nil {
			slog.Warn("mcp: some servers failed to connect", "error", err)
		}
		defer mcpMgr.Close()

		mcpTools := mcpeino.BuildTools(mcpMgr)
		if len(mcpTools) > 0 {
			for _, t := range mcpTools {
				info, _ := t.Info(ctx)
				if info != nil {
					if err := rt.Resolver().Register(t, info.Name, runtime.ToolSourceMCP); err != nil {
						slog.Warn("mcp: register tool failed", "tool", info.Name, "error", err)
					}
				}
			}
			slog.Info("mcp: tools registered", "count", len(mcpTools))
		}
	}

	if err := rt.Start(ctx); err != nil {
		slog.Error("start runtime failed", "error", err)
		os.Exit(1)
	}

	// Optional: Temporal task worker for durable task execution.
	// When TEMPORAL_HOST_PORT is set, connect to the Temporal cluster
	// and start polling the lobehub-tasks queue. When unset, the
	// task HTTP endpoints are still mounted but return 503 for
	// task operations (the chat-completions API is unaffected).
	taskWorker := startTaskWorker(ctx, rt)

	// HTTP server with timeouts
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/health/ready", readyHandler)
	mux.HandleFunc("/v1/tools", toolsHandler)
	mux.HandleFunc("/health/secure", secureHealthHandler)
	if taskWorker != nil {
		taskWorker.RegisterHTTP(mux)
	} else {
		mux.HandleFunc("/v1/tasks/", tasksNotConfiguredHandler)
	}
	mux.HandleFunc("/v1/composio/connections", composioCreateConnectionHandler)
	mux.HandleFunc("/v1/composio/connections/poll", composioPollHandler)
	mux.HandleFunc("/v1/composio/connections/delete", composioDeleteConnectionHandler)
	mux.HandleFunc("/v1/composio/plugins", composioGetPluginsHandler)
	mux.HandleFunc("/v1/composio/plugins/update", composioUpdatePluginHandler)
	mux.HandleFunc("/v1/composio/plugins/remove", composioRemovePluginHandler)
	mux.HandleFunc("/v1/composio/oauth/callback", composioOAuthCallbackHandler)
	mux.HandleFunc("/v1/files/create", createFileHandler)
	mux.HandleFunc("/v1/files/remove", removeFileHandler)
	mux.HandleFunc("/v1/documents/history/save", saveDocumentHistoryHandler)
	mux.HandleFunc("/v1/documents/history/item", getDocumentHistoryItemHandler)
	mux.HandleFunc("/v1/documents/history/compare", compareDocumentHistoryItemsHandler)
	mux.HandleFunc("/v1/documents/update", updateDocumentHandler)
	mux.HandleFunc("/v1/documents/remove", removeDocumentHandler)
	mux.HandleFunc("/v1/chat/send", sendChatHandler)
	mux.HandleFunc("/v1/chat/generate", outputJSONHandler)
	mux.HandleFunc("/v1/chat/archive-tool-result", archiveToolResultHandler)

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
	if taskWorker != nil {
		if err := taskWorker.Stop(); err != nil {
			slog.Warn("task worker stop error", "error", err)
		}
	}
	slog.Info("server stopped")
}

// initEditLock creates an edit-lock from REDIS_URL. Returns nil (disabled)
// when the env var is unset — this is the expected state in local dev.
func initEditLock() *lock.Mutex {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		slog.Info("edit lock: REDIS_URL not set; lock disabled")
		return nil
	}
	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		slog.Warn("edit lock: bad REDIS_URL, lock disabled", "error", err)
		return nil
	}
	rdb := goredis.NewClient(opts)
	m := lock.New(rdb)
	slog.Info("edit lock: enabled", "redis_host", opts.Addr)
	return m
}

// initKeyVault creates an encryptor from KEY_VAULTS_SECRET. Returns nil
// (disabled) when the env var is unset — this is the expected state in
// local dev / test.
func initKeyVault() *keyvault.Encryptor {
	kv, err := keyvault.New()
	if err != nil {
		slog.Warn("keyvault: init failed, encryption disabled", "error", err)
		return nil
	}
	if kv == nil {
		slog.Info("keyvault: KEY_VAULTS_SECRET not set; encryption disabled")
		return nil
	}
	slog.Info("keyvault: enabled")
	return kv
}

// startTaskWorker optionally connects to a Temporal cluster and starts
// the durable task worker. Returns nil when TEMPORAL_HOST_PORT is not
// set — callers should then mount the not-configured HTTP handlers
// instead of the worker's.
//
// The store is currently an in-memory implementation suitable for
// development. Production deployments must wire a Postgres-backed
// TaskStore (see runtime/task/store.go for the interface).
func startTaskWorker(ctx context.Context, rt *runtime.Runtime) *task.Worker {
	hostPort := os.Getenv("TEMPORAL_HOST_PORT")
	if hostPort == "" {
		slog.Info("task worker: TEMPORAL_HOST_PORT not set; task endpoints disabled")
		return nil
	}
	taskQueue := os.Getenv("TEMPORAL_TASK_QUEUE")
	if taskQueue == "" {
		taskQueue = "lobehub-tasks"
	}

	client, err := temporalclient.Dial(temporalclient.Options{
		HostPort: hostPort,
	})
	if err != nil {
		slog.Error("task worker: dial Temporal failed", "error", err, "host_port", hostPort)
		return nil
	}

	store := task.NewInMemoryStore()
	exec := task.NewRuntimeExecutor(nil) // populated lazily by clients in production
	_ = exec                           // keep the import in the build
	// We bind the runtime to the executor: in production the executor
	// wraps runtime.AiAgentService. The mapping is intentionally not
	// wired here so the chat-completions path stays the single owner
	// of the runtime; task workflows use a dedicated executor and
	// can be extended separately.
	w, err := task.NewWorker(task.WorkerConfig{
		Client:   client,
		Store:    store,
		Executor: nil, // nil → NoopExecutor; production overrides per workflow
		Options:  task.WorkflowOptions{TaskQueue: taskQueue},
	})
	if err != nil {
		slog.Error("task worker: construct failed", "error", err)
		client.Close()
		return nil
	}
	if err := w.Start(ctx); err != nil {
		slog.Error("task worker: start failed", "error", err)
		client.Close()
		return nil
	}
	slog.Info("task worker: started", "host_port", hostPort, "task_queue", taskQueue)
	_ = rt
	return w
}

// tasksNotConfiguredHandler is the fallback handler mounted when
// Temporal is not configured. It returns 503 for every task endpoint
// so callers can detect the disabled state.
func tasksNotConfiguredHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"Temporal not configured; set TEMPORAL_HOST_PORT to enable task execution"}`))
}

// loadMCPConfig reads MCP server configuration from:
// 1. MCP_SERVERS env var (JSON: {"servers": {...}})
// 2. mcp.json file in CWD
func loadMCPConfig() mcp.MCPConfig {
	if envJSON := os.Getenv("MCP_SERVERS"); envJSON != "" {
		var wrapper struct {
			Servers map[string]mcp.MCPServerConfig `json:"servers"`
		}
		if err := json.Unmarshal([]byte(envJSON), &wrapper); err == nil && len(wrapper.Servers) > 0 {
			return mcp.MCPConfig{Servers: wrapper.Servers}
		}
		var cfg mcp.MCPConfig
		if err := json.Unmarshal([]byte(envJSON), &cfg); err == nil {
			return cfg
		}
		slog.Warn("mcp: failed to parse MCP_SERVERS env var")
	}

	if data, err := os.ReadFile("mcp.json"); err == nil {
		var cfg mcp.MCPConfig
		if err := json.Unmarshal(data, &cfg); err == nil {
			slog.Info("mcp: loaded config from mcp.json", "servers", len(cfg.Servers))
			return cfg
		}
		slog.Warn("mcp: failed to parse mcp.json")
	}

	return mcp.MCPConfig{}
}
