package main

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDBDSN_Unset(t *testing.T) {
	t.Setenv("KNOWLEDGE_PG_DSN", "")
	if dsn := resolveDBDSN(); dsn != "" {
		t.Errorf("expected empty DSN, got %q", dsn)
	}
}

func TestResolveDBDSN_Set(t *testing.T) {
	t.Setenv("KNOWLEDGE_PG_DSN", "postgres://user:pass@host:5432/db")
	if dsn := resolveDBDSN(); dsn != "postgres://user:pass@host:5432/db" {
		t.Errorf("expected DSN from env, got %q", dsn)
	}
}

func TestPoolConfigDefaults(t *testing.T) {
	maxConns, minConns, maxLifetime, maxIdle, healthCheck := poolConfigDefaults()

	if maxConns != 10 {
		t.Errorf("expected MaxConns 10, got %d", maxConns)
	}
	if minConns != 2 {
		t.Errorf("expected MinConns 2, got %d", minConns)
	}
	if maxLifetime != 30*time.Minute {
		t.Errorf("expected MaxConnLifetime 30m, got %s", maxLifetime)
	}
	if maxIdle != 5*time.Minute {
		t.Errorf("expected MaxConnIdleTime 5m, got %s", maxIdle)
	}
	if healthCheck != 30*time.Second {
		t.Errorf("expected HealthCheckPeriod 30s, got %s", healthCheck)
	}
}

func TestInitDBPool_NoDSN(t *testing.T) {
	t.Setenv("KNOWLEDGE_PG_DSN", "")
	pool := initDBPool(t.Context())
	if pool != nil {
		t.Error("expected nil pool when DSN is unset")
		pool.Close()
	}
}

func TestFormatPoolConfig(t *testing.T) {
	s := formatPoolConfig()
	if s == "" {
		t.Error("expected non-empty pool config string")
	}
	for _, want := range []string{"max_conns=10", "min_conns=2", "max_lifetime=30m", "max_idle=5m", "health_check=30s"} {
		if !strings.Contains(s, want) {
			t.Errorf("formatPoolConfig() missing %q in %q", want, s)
		}
	}
}

