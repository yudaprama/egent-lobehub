package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

var rePlaceholder = regexp.MustCompile(`\{[^}]+\}`)

type APITool struct {
	info     *schema.ToolInfo
	urlTpl   string
	headers  map[string]string
	defaults map[string]any
	method   string
	client   *http.Client
}

func NewAPITool(
	info *schema.ToolInfo,
	urlTpl string,
	headers map[string]string,
	defaults map[string]any,
	method string,
) *APITool {
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	// Browser-rendering style POSTs can be slow; give them a generous timeout.
	timeout := 15 * time.Second
	if method != http.MethodGet {
		timeout = 90 * time.Second
	}
	return &APITool{
		info:     info,
		urlTpl:   urlTpl,
		headers:  headers,
		defaults: defaults,
		method:   method,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (t *APITool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *APITool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	args := map[string]any{}
	if argumentsInJSON != "" && argumentsInJSON != "{}" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("parse arguments: %w", err)
		}
	}

	for k, v := range t.defaults {
		if _, exists := args[k]; !exists {
			args[k] = v
		}
	}

	// JSON-string fallback: if an LLM accidentally serializes an array/object
	// param as a JSON string (e.g. "rejectRequestPattern": "[\"/.css/\"]"
	// instead of "rejectRequestPattern": ["/.css/"]), parse it back. This makes
	// the tool more resilient to LLM output variations.
	for k, v := range args {
		if s, ok := v.(string); ok && len(s) > 0 && (s[0] == '[' || s[0] == '{') {
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err == nil {
				args[k] = parsed
			}
			// If unmarshal fails, keep the string as-is (might be intentional).
		}
	}

	path := t.urlTpl
	for paramName, val := range args {
		placeholder := "{" + paramName + "}"
		encoded := url.PathEscape(fmt.Sprintf("%v", val))
		path = strings.ReplaceAll(path, placeholder, encoded)
	}

	// Remove remaining unsubstituted placeholders (optional params not provided)
	path = rePlaceholder.ReplaceAllString(path, "")

	// Clean empty query params
	path = cleanEmptyQueryParams(path)

	var bodyReader io.Reader
	if t.method == http.MethodGet {
		// Existing behavior — no body.
	} else {
		// Marshal all args as the JSON body. URL-substituted path params are
		// included as harmless extra fields; the LLM still controls which
		// params become path vs body via the URL template and `in_path`.
		payload, err := json.Marshal(args)
		if err != nil {
			return "", fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(context.Background(), t.method, path, bodyReader)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.method != http.MethodGet && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, string(body)), nil
	}

	// For Cloudflare-style envelopes, surface just the `result` field so the
	// LLM gets the raw markdown instead of a wrapped JSON blob.
	if t.method != http.MethodGet {
		var envelope struct {
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Errors  []any           `json:"errors"`
		}
		if err := json.Unmarshal(body, &envelope); err == nil && envelope.Success && len(envelope.Result) > 0 {
			if len(envelope.Errors) > 0 {
				return string(envelope.Result), nil
			}
			// Try to return a string result unquoted; fall back to raw JSON.
			var s string
			if err := json.Unmarshal(envelope.Result, &s); err == nil {
				return s, nil
			}
			return string(envelope.Result), nil
		}
	}

	return string(body), nil
}

func cleanEmptyQueryParams(path string) string {
	qIdx := strings.Index(path, "?")
	if qIdx == -1 {
		return path
	}

	base := path[:qIdx]
	query := path[qIdx+1:]

	parts := strings.Split(query, "&")
	var kept []string
	for _, p := range parts {
		if strings.Contains(p, "=") {
			kv := strings.SplitN(p, "=", 2)
			if kv[1] != "" {
				kept = append(kept, p)
			}
		}
	}

	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}
