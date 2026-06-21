package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveDBDSN returns the Postgres DSN from KNOWLEDGE_PG_DSN.
// Returns empty string when not set.
func resolveDBDSN() string {
	return os.Getenv("KNOWLEDGE_PG_DSN")
}

// initDBPool creates a shared Postgres connection pool from the resolved
// DSN. Returns nil when no DSN is available. The caller owns the pool
// lifecycle and must call Close() on shutdown.
func initDBPool(ctx context.Context) *pgxpool.Pool {
	dsn := resolveDBDSN()
	if dsn == "" {
		slog.Info("db pool: KNOWLEDGE_PG_DSN not set; no shared Postgres pool")
		return nil
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		slog.Error("db pool: parse config failed", "error", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		slog.Error("db pool: create failed", "error", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		slog.Error("db pool: ping failed", "error", err)
		pool.Close()
		os.Exit(1)
	}
	slog.Info("db pool: shared Postgres pool ready", "dsn_host", redactedDSNHost(dsn))
	return pool
}

// poolConfigDefaults returns the pool tuning values applied by initDBPool.
// Exposed so tests can assert the config without creating a real pool.
func poolConfigDefaults() (maxConns int32, minConns int32, maxLifetime, maxIdle time.Duration, healthCheck time.Duration) {
	return 10, 2, 30 * time.Minute, 5 * time.Minute, 30 * time.Second
}

func formatPoolConfig() string {
	maxConns, minConns, maxLifetime, maxIdle, healthCheck := poolConfigDefaults()
	return fmt.Sprintf("max_conns=%d min_conns=%d max_lifetime=%s max_idle=%s health_check=%s",
		maxConns, minConns, maxLifetime, maxIdle, healthCheck)
}
