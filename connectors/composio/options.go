package composio

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type (
	// Option configures a Composio client at construction time.
	Option func(*Composio)

	// ToolsOption configures a single GetTools request.
	ToolsOption func(*url.Values)

	// AuthOption configures a single ListConnections request.
	AuthOption func(*url.Values)
)

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(c *Composio) { c.logger = logger }
}

// WithBaseURL overrides the API root. Useful for self-hosted or staging
// deploys. The trailing slash is trimmed if present.
func WithBaseURL(baseURL string) Option {
	return func(c *Composio) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient substitutes the underlying *http.Client. Tests use this to
// point at an httptest.Server; production callers can swap in a client with
// custom TLS config, proxy, or transport tuning.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Composio) {
		if hc != nil {
			c.client = hc
		}
	}
}

// WithTimeout overrides the default 30s request timeout. The default is
// chosen to be long enough for the slowest tool call (image generation
// and proxy requests can take 20s+ on a cold path) without leaking hung
// connections on a wedged upstream.
func WithTimeout(d time.Duration) Option {
	return func(c *Composio) {
		if c.client != nil {
			c.client.Timeout = d
		}
	}
}

// WithApp sets the toolkit_slug filter for GetTools (e.g. "GMAIL",
// "SLACK", "GITHUB"). The slug is normalised to upper-snake before the
// request so callers can pass either "github" or "GITHUB".
func WithApp(slug string) ToolsOption {
	return func(u *url.Values) { u.Set("toolkit_slug", NormaliseSlug(slug)) }
}

// WithSearch sets a free-text search term for GetTools.
func WithSearch(q string) ToolsOption {
	return func(u *url.Values) { u.Set("search", q) }
}

// WithTags filters GetTools by the tool's tag set. Multiple tags are
// comma-joined; Composio matches any.
func WithTags(tags ...string) ToolsOption {
	return func(u *url.Values) { u.Set("tags", strings.Join(tags, ",")) }
}

// WithLimit caps the number of tools returned by GetTools. The default
// (no limit) is set by the server.
func WithLimit(n int) ToolsOption {
	return func(u *url.Values) { u.Set("limit", itoa(n)) }
}

// WithUserUUID filters ListConnections to a specific end user. Composio
// uses the term "user_uuid" for the LobeHub-side actor_id; when omitted,
// Composio returns connections for the project key's default user.
func WithUserUUID(userUUID string) AuthOption {
	return func(u *url.Values) { u.Set("user_uuid", userUUID) }
}

// WithShowActiveOnly filters ListConnections to ACTIVE accounts. Use this
// when surfacing the user's available connections in the UI; skip it when
// auditing failed/expired connections for the agent runtime.
func WithShowActiveOnly(show bool) AuthOption {
	return func(u *url.Values) { u.Set("showActiveOnly", boolStr(show)) }
}

// itoa / boolStr are tiny helpers that avoid pulling strconv into the
// options file just for two trivial conversions. Tests assert the encoded
// values; changing these strings is a breaking change for tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
