package composio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Tool is a single Composio action. The ArgsSchema is kept as
// json.RawMessage because Composio returns a full JSON Schema and we only
// forward it to the agent; parsing it into Go types would be lossy and
// would require regenerating the package every time Composio ships a new
// toolkit.
//
// Mirrors the projection used by lobehub/src/server/services/composio's
// getComposioManifests: each Tool becomes one entry in the LobeToolManifest
// api array (description / name / parameters).
type Tool struct {
	// Slug is the canonical Composio action name (e.g.
	// "GITHUB_LIST_REPOS"). Used as the path segment in
	// POST /tools/execute/{slug}.
	Slug string `json:"slug"`
	// Name is the human-readable name; may be empty in which case callers
	// fall back to Slug.
	Name string `json:"name,omitempty"`
	// Description is the LLM-facing description of what the tool does.
	Description string `json:"description,omitempty"`
	// Toolkit is the parent toolkit this tool belongs to.
	Toolkit Toolkit `json:"toolkit,omitempty"`
	// ArgsSchema is the raw JSON Schema describing the tool's input
	// parameters. Composio returns this under either "input_parameters"
	// or "input_schema" depending on the endpoint; both are normalised
	// here by the client. Pass straight to the agent as the tool's
	// parameters — do not re-parse.
	ArgsSchema json.RawMessage `json:"args_schema,omitempty"`
	// Version is the toolkit version this tool belongs to (e.g.
	// "20260615_00"). Composio requires a version on ExecuteTool unless
	// the client passes dangerouslySkipVersionCheck; we set "latest"
	// by default and let the API resolve it.
	Version string `json:"version,omitempty"`
	// AvailableVersions lists the versions Composio has published for
	// this tool. Empty when the API does not return it.
	AvailableVersions []string `json:"available_versions,omitempty"`
	// Tags is the toolkit's tag set (e.g. ["Actions", "Production"]).
	// Useful for filtering but not currently surfaced by LobeHub.
	Tags []string `json:"tags,omitempty"`
	// Deprecated is true when Composio has marked the tool as deprecated.
	// LobeHub's manifest filter does not currently drop these, but the
	// agent runtime could choose to.
	Deprecated bool `json:"deprecated,omitempty"`
}

// rawTool captures the wire shape from /tools. Field names follow the
// snake_case used by the v3.1 API; we project to the export-facing Tool
// above. Two fields accept either snake_case (input_parameters, the
// current default) or camelCase (inputSchema, used by the legacy /actions
// endpoint) — both are decoded so the client works against either shape
// during the v1→v3.1 migration window.
type rawTool struct {
	Slug            string          `json:"slug"`
	Name            string          `json:"name,omitempty"`
	Description     string          `json:"description,omitempty"`
	Toolkit         *Toolkit        `json:"toolkit,omitempty"`
	InputParameters   json.RawMessage `json:"input_parameters,omitempty"`
	InputSchema       json.RawMessage `json:"input_schema,omitempty"`
	Version           string          `json:"version,omitempty"`
	AvailableVersions []string        `json:"available_versions,omitempty"`
	Tags              []string        `json:"tags,omitempty"`
	Deprecated        *deprecatedMeta `json:"deprecated,omitempty"`
}

// deprecatedMeta is the wire shape of the `deprecated` field on /tools.
// Despite the name, Composio returns an object here with version metadata
// rather than a plain bool. The actual deprecation flag is IsDeprecated.
type deprecatedMeta struct {
	IsDeprecated      bool     `json:"is_deprecated,omitempty"`
	DisplayName       string   `json:"displayName,omitempty"`
	Version           string   `json:"version,omitempty"`
	AvailableVersions []string `json:"available_versions,omitempty"`
	Toolkit           *Toolkit `json:"toolkit,omitempty"`
}

func (r *rawTool) toPublic() *Tool {
	out := &Tool{
		Slug:              r.Slug,
		Name:              r.Name,
		Description:       r.Description,
		Version:           r.Version,
		AvailableVersions: r.AvailableVersions,
		Tags:              r.Tags,
	}
	// Prefer input_parameters (v3.1) over input_schema (legacy v1).
	if len(r.InputParameters) > 0 {
		out.ArgsSchema = r.InputParameters
	} else if len(r.InputSchema) > 0 {
		out.ArgsSchema = r.InputSchema
	}
	if r.Toolkit != nil {
		out.Toolkit = *r.Toolkit
	}
	if r.Deprecated != nil {
		out.Deprecated = r.Deprecated.IsDeprecated
		// The metadata object sometimes carries version info that's
		// missing from the top level; fall back to it when needed.
		if out.Version == "" && r.Deprecated.Version != "" {
			out.Version = r.Deprecated.Version
		}
		if len(out.AvailableVersions) == 0 && len(r.Deprecated.AvailableVersions) > 0 {
			out.AvailableVersions = r.Deprecated.AvailableVersions
		}
	}
	return out
}

type toolListResponse struct {
	Items      []rawTool `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	TotalCount int       `json:"total_count,omitempty"`
}

// GetTools returns the tool list, optionally filtered by toolkit (WithApp),
// search term (WithSearch), tags (WithTags), or limit (WithLimit).
//
// Mirrors `composio.tools.getRawComposioTools(...)` and
// `getComposioManifests` in lobehub/src/server/services/composio. The
// returned Tool.ArgsSchema is the raw JSON Schema from the API; pass it
// straight to the agent as the tool's parameters.
//
// Note: groq-go/extensions/composio exposes this as GetTools against the
// deprecated /v1/actions endpoint with appNames/useCase query params. We
// use /api/v3.1/tools with toolkit_slug — the field renames happened in
// the v3.0 cutover (see @composio/core CHANGELOG 2025-09).
func (c *Composio) GetTools(ctx context.Context, opts ...ToolsOption) ([]Tool, error) {
	u, err := url.Parse(c.baseURL + "/tools")
	if err != nil {
		return nil, fmt.Errorf("composio: parse url: %w", err)
	}
	q := u.Query()
	for _, opt := range opts {
		opt(&q)
	}
	u.RawQuery = q.Encode()

	req, err := newJSONRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	var raw toolListResponse
	if err := c.doRequest(req, &raw); err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(raw.Items))
	for i := range raw.Items {
		out = append(out, *raw.Items[i].toPublic())
	}
	return out, nil
}

// GetToolsForApp is a convenience wrapper around GetTools(WithApp(slug))
// for the common case of fetching one toolkit's actions. Equivalent to
// composio.tools.getRawComposioTools({toolkits: [slug]}) in the JS SDK.
func (c *Composio) GetToolsForApp(ctx context.Context, slug string) ([]Tool, error) {
	if slug == "" {
		return nil, fmt.Errorf("composio: slug required")
	}
	return c.GetTools(ctx, WithApp(slug))
}

// executeRequest is the body of POST /tools/execute/{tool_slug}.
// Field names follow the snake_case used by the v3.1 API.
type executeRequest struct {
	Arguments          map[string]any `json:"arguments,omitempty"`
	ConnectedAccountID string         `json:"connected_account_id,omitempty"`
	UserID             string         `json:"user_id,omitempty"`
	Version            string         `json:"version,omitempty"`
	AllowTracing       bool           `json:"allow_tracing,omitempty"`
	Text               string         `json:"text,omitempty"`
}

// ExecuteResult is the parsed return value of ExecuteTool.
// The raw API returns a deeply nested object (data.data | data.result | data);
// we collapse it to a single string ready for the agent, mirroring
// ComposioService.executeComposioTool in lobehub/src/server/services/composio.
type ExecuteResult struct {
	// Content is the agent-ready string. May be empty on failure.
	Content string `json:"content"`
	// Success is true when the tool executed without an error.
	Success bool `json:"success"`
	// Executed mirrors the API's executed flag. May differ from Success
	// when the upstream returned partial data.
	Executed bool `json:"executed,omitempty"`
	// Error is non-nil only when Success == false.
	Error *ExecuteError `json:"error,omitempty"`
}

// ExecuteError is the structured error returned on tool execution failure.
// Mirrors the codes used by the TS ComposioService:
// COMPOSIO_NOT_CONFIGURED, COMPOSIO_NOT_INITIALIZED, COMPOSIO_SERVER_NOT_FOUND,
// COMPOSIO_CONFIG_NOT_FOUND, COMPOSIO_ERROR, COMPOSIO_AUTH_ERROR.
type ExecuteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ExecuteError) Error() string {
	return fmt.Sprintf("composio: %s: %s", e.Code, e.Message)
}

// rawExecuteResponse is the wire shape of POST /tools/execute/{tool_slug}.
// The actual data payload is unstructured — it may be a string, an array
// of content blocks ({type:"text", text:"..."}), or a wrapped error. The
// client collapses all of these into ExecuteResult.Content.
type rawExecuteResponse struct {
	Data       json.RawMessage `json:"data,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
	ErrorMsg   string          `json:"error_message,omitempty"`
	Executed   bool            `json:"executed,omitempty"`
}

// ExecuteTool runs a single tool on behalf of a connected user. The
// toolSlug is the canonical Composio action name (e.g. "GITHUB_LIST_REPOS");
// connectedAccountID is resolved server-side by the caller from PluginModel
// (the client never trusts a client-supplied id — see
// lobehub/src/server/services/composio/ComposioService.executeComposioTool).
//
// The Version field is set to "latest" to match the lobehub behaviour of
// passing `dangerouslySkipVersionCheck: true` to the JS SDK; the TS SDK
// would otherwise throw ComposioToolVersionRequiredError for unpinned
// versions. If you need a pinned version, build the request body directly.
//
// Errors are returned inside ExecuteResult.Error (not as a Go error) so
// the agent runtime can branch on the code without a type assertion. The
// one exception is transport-level failures (network, DNS), which surface
// as a regular error return.
func (c *Composio) ExecuteTool(
	ctx context.Context,
	toolSlug string,
	args map[string]any,
	connectedAccountID string,
	userID string,
) (*ExecuteResult, error) {
	if toolSlug == "" {
		return nil, fmt.Errorf("composio: toolSlug required")
	}
	body := executeRequest{
		Arguments:          args,
		ConnectedAccountID: connectedAccountID,
		UserID:             userID,
		Version:            "latest",
		AllowTracing:       true,
	}
	req, err := newJSONRequest(
		ctx,
		http.MethodPost,
		c.baseURL+"/tools/execute/"+url.PathEscape(toolSlug),
		body,
	)
	if err != nil {
		return nil, err
	}
	var raw rawExecuteResponse
	if err := c.doRequest(req, &raw); err != nil {
		// Convert auth errors to a structured ExecuteError so the agent
		// runtime can map it to ErrorKindStop without a second check.
		var apiErr *APIError
		if errorsAs(err, &apiErr) && apiErr.IsAuthError() {
			return &ExecuteResult{
				Success: false,
				Error:   &ExecuteError{Code: "COMPOSIO_AUTH_ERROR", Message: apiErr.Message},
			}, nil
		}
		return &ExecuteResult{
			Success: false,
			Error:   &ExecuteError{Code: "COMPOSIO_ERROR", Message: err.Error()},
		}, nil
	}
	return &ExecuteResult{
		Success:  raw.Executed,
		Executed: raw.Executed,
		Content:  collapseContent(raw.Data, raw.Result, raw.Error),
	}, nil
}

// collapseContent turns the polymorphic tool-execute response into a single
// string suitable for the agent. Mirrors the LobeHub TS collapse logic at
// lobehub/src/server/services/composio/index.ts:113-126:
//
//	data === string      → as-is
//	data === array       → join text blocks / JSON-stringify objects
//	data === object      → JSON.stringify
//
// The API sometimes wraps the payload under "data" (newer endpoints) and
// sometimes under "result" (legacy). We check both, in that order, and
// skip "null" payloads so a wrapper that omits a field doesn't mask the
// real one.
func collapseContent(payloads ...json.RawMessage) string {
	for _, p := range payloads {
		if len(p) == 0 || string(p) == "null" {
			continue
		}
		// String → as-is.
		var s string
		if err := json.Unmarshal(p, &s); err == nil {
			return s
		}
		// Array → join text blocks.
		var arr []json.RawMessage
		if err := json.Unmarshal(p, &arr); err == nil {
			parts := make([]string, 0, len(arr))
			for _, item := range arr {
				var text string
				if err := json.Unmarshal(item, &text); err == nil {
					parts = append(parts, text)
					continue
				}
				var block struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(item, &block); err == nil && block.Type == "text" {
					parts = append(parts, block.Text)
					continue
				}
				parts = append(parts, string(item))
			}
			return strings.Join(parts, "\n")
		}
		// Object → JSON.
		return string(p)
	}
	return ""
}
