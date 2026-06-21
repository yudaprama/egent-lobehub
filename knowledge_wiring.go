package main

import (
	"log/slog"
	"net/url"
	"os"
	"strings"

	fp "github.com/kawai-network/fileprocessor"
)

// Knowledge wiring lives in its own file so main.go stays focused on the
// server lifecycle.

// buildKnowledgeEmbedder constructs the embedder used by the knowledge
// service. The endpoint defaults to MODEL_BASE_URL + "/embeddings" (or the
// standalone OPENAI_EMBEDDINGS_URL), the model defaults to
// "text-embedding-3-small", and the API key falls back to OPENAI_API_KEY /
// MODEL_API_KEY. Dim is pinned to 1024 to match public.embeddings.
//
// Returns nil when no endpoint can be resolved — the caller registers the
// knowledge_search tool anyway, and the tool will respond with
// "knowledge search is not configured" until a key is provided.
func buildKnowledgeEmbedder() fp.Embedder {
	embedURL := os.Getenv("OPENAI_EMBEDDINGS_URL")
	if embedURL == "" {
		base := os.Getenv("MODEL_BASE_URL")
		if base != "" {
			embedURL = strings.TrimRight(base, "/") + "/embeddings"
		}
	}
	if embedURL == "" {
		slog.Warn("knowledge: no embed URL configured (set OPENAI_EMBEDDINGS_URL or MODEL_BASE_URL)")
		return nil
	}

	model := os.Getenv("OPENAI_EMBEDDINGS_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("MODEL_API_KEY")
	}

	slog.Info("knowledge: embedder configured",
		"url", embedURL,
		"model", model,
		"has_api_key", apiKey != "",
	)
	return fp.NewOpenAIEmbedder(embedURL, apiKey, model, 1024)
}

// redactedDSNHost returns the host portion of a Postgres DSN with the
// password stripped, for safe logging.
func redactedDSNHost(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable)"
	}
	return u.Host
}
