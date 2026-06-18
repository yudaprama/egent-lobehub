package composio

import (
	"context"
	"os"
	"testing"
)

// TestSmoke_LiveAPI exercises every public method against the real
// Composio API. Skips when COMPOSIO_API_KEY is not set. Uses a fixed
// "smoke" user uuid so ListConnections/LinkConnection have a stable id.
func TestSmoke_LiveAPI(t *testing.T) {
	key := os.Getenv("COMPOSIO_API_KEY")
	if key == "" {
		t.Skip("COMPOSIO_API_KEY not set")
	}
	c, err := NewComposer(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("ListAuthConfigs", func(t *testing.T) {
		configs, err := c.ListAuthConfigs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("auth configs: %d", len(configs))
		for i, cfg := range configs {
			if i < 5 {
				t.Logf("  %s: %s (%s)", cfg.ID, cfg.Name, cfg.Toolkit.Slug)
			}
		}
	})

	t.Run("GetTools_GMAIL", func(t *testing.T) {
		tools, err := c.GetToolsForApp(ctx, "GMAIL")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("GMAIL tools: %d", len(tools))
		for i, tool := range tools {
			if i < 3 {
				t.Logf("  %s: %s", tool.Slug, tool.Description)
			}
		}
	})

	t.Run("GetTools_GITHUB", func(t *testing.T) {
		tools, err := c.GetToolsForApp(ctx, "GITHUB")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("GITHUB tools: %d", len(tools))
		for i, tool := range tools {
			if i < 3 {
				t.Logf("  %s: %s", tool.Slug, tool.Description)
			}
		}
	})

	t.Run("GetTools_SLACK", func(t *testing.T) {
		tools, err := c.GetToolsForApp(ctx, "SLACK")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("SLACK tools: %d", len(tools))
		for i, tool := range tools {
			if i < 3 {
				t.Logf("  %s: %s", tool.Slug, tool.Description)
			}
		}
	})

	t.Run("GetTools_Search", func(t *testing.T) {
		tools, err := c.GetTools(ctx, WithSearch("send email"), WithLimit(5))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("search 'send email': %d tools", len(tools))
		for _, tool := range tools {
			t.Logf("  %s: %s", tool.Slug, tool.Toolkit.Slug)
		}
	})

	t.Run("ListConnections", func(t *testing.T) {
		conns, err := c.ListConnections(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("connections: %d", len(conns))
		for i, conn := range conns {
			if i < 5 {
				t.Logf("  %s: %s (%s)", conn.ID, conn.Status, conn.Toolkit.Slug)
			}
		}
	})

	t.Run("CreateManagedAuthConfig_SLACK", func(t *testing.T) {
		cfg, err := c.CreateManagedAuthConfig(ctx, "slack", "Test Slack Config")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("created: %s (%s) type=%s", cfg.ID, cfg.Name, cfg.Type)
	})

	t.Run("FindAuthConfigForToolkit_SLACK", func(t *testing.T) {
		cfg, err := c.FindAuthConfigForToolkit(ctx, "slack")
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			t.Error("expected to find slack config from previous test")
		} else {
			t.Logf("found: %s", cfg.ID)
		}
	})

	t.Run("LinkConnection_SLACK", func(t *testing.T) {
		// Find or create a slack auth config.
		cfg, err := c.ResolveOrCreateAuthConfig(ctx, "slack", "Smoke Test")
		if err != nil {
			t.Fatal(err)
		}
		connID, redirect, err := c.LinkConnection(ctx, "smoke-user-1", cfg.ID, "https://example.com/cb")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("linked: %s → %s", connID, redirect)
		// Now poll for the connection state.
		got, err := c.GetConnection(ctx, connID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("connection state: %s (auth_config=%s)", got.Status, got.AuthConfigID)
		// Clean up — delete the connection so re-runs don't accumulate.
		if err := c.DeleteConnection(ctx, connID); err != nil {
			t.Logf("delete (may be expected if pending): %v", err)
		}
	})

	t.Run("ExecuteTool_MissingConnection", func(t *testing.T) {
		result, err := c.ExecuteTool(ctx, "GITHUB_LIST_REPOS",
			map[string]any{"owner": "composio"},
			"fake-connection-id",
			"smoke-user-1")
		if err != nil {
			t.Fatal(err)
		}
		if result.Success {
			t.Error("expected failure for invalid connection")
		}
		t.Logf("got expected failure: %s: %s", result.Error.Code, result.Error.Message)
	})
}
