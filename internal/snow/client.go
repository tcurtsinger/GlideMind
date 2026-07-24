// Package snow is the transport-agnostic ServiceNow client shared by every
// glm front-end (CLI today, HTTP/MCP facade later): instance normalization,
// auth, bounded retries, and error mapping. No terminal concerns live here.
package snow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxAttempts   = 3
	baseRetryWait = 500 * time.Millisecond
	maxBodyBytes  = 8 << 20
	maxRedirects  = 5
	maxRetryDelay = 30 * time.Second
)

// Record is one row from the Table API. Value types depend on the
// sysparm_display_value mode requested by the caller.
type Record = map[string]any

// Client talks to one instance. Credentials are injected per request by an
// authenticator — basic auth or a static bearer token today; the refreshing
// OAuth implementations arrive with DESIGN-OAUTH.md's later phases.
type Client struct {
	base     *url.URL
	hc       *http.Client
	username string
	auth     authenticator
	logf     func(format string, args ...any)
}

// authenticator injects credentials into outgoing requests (DESIGN-OAUTH.md
// O7). apply sets the Authorization material on one request. retryAuth is
// consulted at most once per call after an HTTP 401: (true, nil) means the
// credential was renewed and the request retries once; (false, nil) means
// it cannot renew, so the 401 surfaces; a non-nil error means renewal was
// attempted and FAILED — that error replaces the raw 401, because it names
// the remedy (an expired session's corrective login, a rejected client
// secret) where the 401 alone would not. A 401 is rejected before the
// instance processes the request, so the one retry never violates the
// write path's send-once contract.
type authenticator interface {
	apply(req *http.Request) error
	retryAuth(ctx context.Context) (bool, error)
}

type basicAuth struct{ username, password string }

func (a basicAuth) apply(req *http.Request) error {
	req.SetBasicAuth(a.username, a.password)
	return nil
}

func (a basicAuth) retryAuth(context.Context) (bool, error) { return false, nil }

type bearerAuth struct{ token string }

func (a bearerAuth) apply(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

func (a bearerAuth) retryAuth(context.Context) (bool, error) { return false, nil }

// TokenSource supplies and renews a bearer token — the seam the OAuth
// grants plug into (DESIGN-OAUTH.md O6/O7). Token returns a currently
// usable access token, minting or refreshing as needed; an error means the
// credential is unobtainable (e.g. an expired session needing an
// interactive login). Renew is consulted once per call after an HTTP 401
// and returns nil only when a genuinely new credential was obtained; its
// error replaces the raw 401 for the caller, so it must name the remedy.
// Implementations must be safe for concurrent use.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Renew(ctx context.Context) error
}

type sourceAuth struct{ src TokenSource }

func (a sourceAuth) apply(req *http.Request) error {
	tok, err := a.src.Token(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func (a sourceAuth) retryAuth(ctx context.Context) (bool, error) {
	if err := a.src.Renew(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// NormalizeInstance turns "acme", "acme.service-now.com", or a full URL into
// a base URL.
func NormalizeInstance(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("instance is empty")
	}
	if !strings.Contains(s, "://") {
		if !strings.Contains(s, ".") {
			s += ".service-now.com"
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid instance %q", s)
	}
	// A credential embedded in the URL (user:pass@host) would be persisted
	// to config.toml and echoed in output — never accept it silently.
	if u.User != nil {
		return nil, fmt.Errorf("instance URL must not embed credentials (user:password@…) — set the username on the profile and supply the password via the keyring or GLM_PASSWORD")
	}
	// Basic auth over plaintext exposes the credential on the wire. Require
	// https; allow http only for loopback dev instances.
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("refusing plaintext http for %q — use https (http is allowed only for loopback dev instances)", u.Host)
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q in instance %q — use https", u.Scheme, s)
	}
	// Users paste browser URLs (e.g. .../nav_to.do?uri=...). The REST API
	// always lives at the host root, so drop any path outright — keeping it
	// would silently aim every request at /nav_to.do/api/now/....
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

// isLoopback reports whether host is localhost or a loopback IP literal.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// NewBasic builds a client for one instance using basic auth.
func NewBasic(instance, username, password string, timeout time.Duration) (*Client, error) {
	u, err := NormalizeInstance(instance)
	if err != nil {
		return nil, err
	}
	if username == "" {
		return nil, fmt.Errorf("username is empty (set it on the profile or via GLM_USERNAME)")
	}
	// A zero/negative timeout would disable the client deadline entirely.
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive, got %s", timeout)
	}
	return &Client{
		base:     u,
		hc:       &http.Client{Timeout: timeout, CheckRedirect: secureRedirect},
		username: username,
		auth:     basicAuth{username: username, password: password},
	}, nil
}

// NewBearer builds a client that authenticates with a static bearer token
// (GLM_TOKEN — DESIGN-OAUTH.md O8). The token is never renewed: when it
// expires, the 401 surfaces (exit 2) and the environment re-mints. username
// names the identity for the per-user schema cache; callers pass their best
// knowledge (GLM_USERNAME) or a stable pseudo-identity, never "".
func NewBearer(instance, token, username string, timeout time.Duration) (*Client, error) {
	u, err := NormalizeInstance(instance)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("bearer token is empty")
	}
	if username == "" {
		return nil, fmt.Errorf("username is empty — bearer clients must name an identity for the per-user cache")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive, got %s", timeout)
	}
	return &Client{
		base:     u,
		hc:       &http.Client{Timeout: timeout, CheckRedirect: secureRedirect},
		username: username,
		auth:     bearerAuth{token: token},
	}, nil
}

// NewTokenAuth builds a client whose bearer token comes from a renewable
// TokenSource — the OAuth PKCE and client-credentials constructors
// (DESIGN-OAUTH.md O7). username names the identity for the per-user
// schema cache; never "".
func NewTokenAuth(instance string, src TokenSource, username string, timeout time.Duration) (*Client, error) {
	u, err := NormalizeInstance(instance)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("token source is nil")
	}
	if username == "" {
		return nil, fmt.Errorf("username is empty — token clients must name an identity for the per-user cache")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive, got %s", timeout)
	}
	return &Client{
		base:     u,
		hc:       &http.Client{Timeout: timeout, CheckRedirect: secureRedirect},
		username: username,
		auth:     sourceAuth{src: src},
	}, nil
}

// secureRedirect is the client's redirect policy. Non-GET requests never
// follow a redirect: a 307/308 would replay the write body to a new
// location (breaking the "exactly once on the wire" contract the --yes
// preview promises) and a 301/302/303 would silently drop it — so the 3xx
// is surfaced to the caller instead. GET follows only same-origin hops,
// which keeps the Basic credential on the exact configured host and blocks
// any https→http downgrade.
func secureRedirect(req *http.Request, via []*http.Request) error {
	orig := via[0]
	if orig.Method != http.MethodGet {
		return http.ErrUseLastResponse
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if !sameOrigin(orig.URL, req.URL) {
		return fmt.Errorf("refusing redirect off the configured origin (%s → %s)", orig.URL.Host, req.URL.Host)
	}
	return nil
}

// sameOrigin compares scheme, host, and effective port.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// SetLogf installs a --verbose logger; nil disables logging.
func (c *Client) SetLogf(f func(format string, args ...any)) { c.logf = f }

func (c *Client) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// BaseURL is the normalized instance URL.
func (c *Client) BaseURL() string { return c.base.String() }

// Username is the authenticated identity — cache keys must include it, since
// instance metadata (dictionary rows) is ACL-filtered per user.
func (c *Client) Username() string { return c.username }

// TokenIdentity reports whether the client's identity comes from a token
// rather than a configured username+password. Identity checks (whoami,
// profile test) must then ask the instance who the token is — a stored
// username may not match the credential actually in use.
func (c *Client) TokenIdentity() bool {
	_, basic := c.auth.(basicAuth)
	return !basic
}

// GetJSON performs a GET with bounded retries on 429/503 (honoring
// Retry-After) and decodes the JSON response into out (which may be nil).
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.getJSON(ctx, path, query, out)
	return err
}

// getJSON is GetJSON plus the response headers (for X-Total-Count).
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) (http.Header, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()

	renewed := false
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		if err := c.auth.apply(req); err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		c.log("GET %s (attempt %d/%d)", u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, &NetworkError{Err: err}
		}

		retry, rerr := c.renewOn401(ctx, resp, &renewed)
		if rerr != nil {
			return nil, rerr
		}
		if retry {
			c.log("HTTP 401, retrying with renewed credentials")
			continue
		}
		if wait, ok := retryAfter(resp, attempt); ok {
			c.log("HTTP %d, retrying in %s", resp.StatusCode, wait)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, &NetworkError{Err: ctx.Err()}
			}
		}
		return resp.Header, decodeResponse(resp, out)
	}
}

// renewOn401 decides what a 401 response means: (true, nil) — the
// authenticator renewed its credential, the body is drained, retry once;
// (false, nil) — not renewable (or the single renewal is spent), decode
// the 401 as usual; (false, err) — renewal was ATTEMPTED and failed, and
// that error replaces the raw 401 because it names the remedy the 401
// cannot (the corrective login command, a rejected client secret).
func (c *Client) renewOn401(ctx context.Context, resp *http.Response, renewed *bool) (bool, error) {
	if resp.StatusCode != http.StatusUnauthorized || *renewed {
		return false, nil
	}
	ok, err := c.auth.retryAuth(ctx)
	if err != nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		return false, err
	}
	if !ok {
		return false, nil
	}
	*renewed = true
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	return true, nil
}

// retryAfter reports whether the response is retryable (429/503) within the
// attempt budget, draining the body and returning the wait when it is.
func retryAfter(resp *http.Response, attempt int) (time.Duration, bool) {
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable
	if !retryable || attempt >= maxAttempts {
		return 0, false
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	return retryDelay(resp, attempt), true
}

// Raw performs an arbitrary REST request (glm api) with the same auth and
// error mapping as reads, returning the raw response body — possibly empty
// (HTTP 204). Automatic 429/503 retries apply to GET only: a 503 can arrive
// after the instance already applied a write, so a non-GET request goes on
// the wire exactly once — matching the single request the --yes preview
// showed. The one exception is a 401 with a renewable credential: a 401 is
// rejected before the instance processes the request, so the single renewed
// retry keeps the effectively-once contract.
func (c *Client) Raw(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	u, err := c.rawURL(path, query)
	if err != nil {
		return nil, err
	}

	renewed := false
	for attempt := 1; ; attempt++ {
		var rdr io.Reader
		if len(body) > 0 {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		if err := c.auth.apply(req); err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		c.log("%s %s (attempt %d/%d)", method, u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, &NetworkError{Err: err}
		}
		retry, rerr := c.renewOn401(ctx, resp, &renewed)
		if rerr != nil {
			return nil, rerr
		}
		if retry {
			c.log("HTTP 401, retrying with renewed credentials")
			continue
		}
		if method == http.MethodGet {
			if wait, ok := retryAfter(resp, attempt); ok {
				c.log("HTTP %d, retrying in %s", resp.StatusCode, wait)
				select {
				case <-time.After(wait):
					continue
				case <-ctx.Done():
					return nil, &NetworkError{Err: ctx.Err()}
				}
			}
		}
		// Read one byte past the cap so truncation is detected, never silent.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, &NetworkError{Err: err}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			if len(data) > maxBodyBytes {
				data = data[:maxBodyBytes]
			}
			return nil, apiError(resp.StatusCode, data)
		}
		if len(data) > maxBodyBytes {
			return nil, fmt.Errorf("response exceeds glm's %d MiB buffer - narrow the request (sysparm_limit, pagination, or a more specific endpoint)", maxBodyBytes>>20)
		}
		// glm api is a raw passthrough, so legitimate HTML (a scripted resource,
		// an HTML attachment) must flow through. Only abort on the UI login/session
		// page — the one HTML response that is never the data asked for.
		if ct := resp.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "text/html") && isLoginPage(data) {
			return nil, &ProtocolError{Err: fmt.Errorf("instance returned its login page, not data - the session/credentials may have expired (HTTP %d)", resp.StatusCode)}
		}
		return data, nil
	}
}

// rawURL builds the absolute URL a Raw request will hit. The path is already
// percent-encoded by the caller (e.g. a%2Fb); parsing it as a relative
// reference preserves those escapes (assigning to URL.Path would re-escape %
// into %25). Any query already in the path merges with the caller's params.
func (c *Client) rawURL(path string, query url.Values) (*url.URL, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("invalid request path %q: %w", path, err)
	}
	if ref.IsAbs() || ref.Host != "" || ref.User != nil {
		return nil, fmt.Errorf("request path must be a relative path, not %q", path)
	}
	// An unescaped # starts a URL fragment, which is never sent — silently
	// truncating the request. Make the caller encode a literal # as %23.
	if strings.Contains(path, "#") {
		return nil, fmt.Errorf("request path must not contain a literal '#' (it starts a fragment that is never sent) — encode it as %%23: %q", path)
	}
	u := *c.base
	u.Path = ref.Path
	u.RawPath = ref.EscapedPath()
	merged := ref.Query()
	for k, vs := range query {
		for _, v := range vs {
			merged.Add(k, v)
		}
	}
	u.RawQuery = merged.Encode()
	return &u, nil
}

// PreviewURL returns the exact absolute URL a Raw call with these arguments
// would send, so a --yes write preview shows what actually goes on the wire.
func (c *Client) PreviewURL(path string, query url.Values) (string, error) {
	u, err := c.rawURL(path, query)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Attachment fetches one attachment's metadata by sys_id.
func (c *Client) Attachment(ctx context.Context, sysID string) (Record, error) {
	var out struct {
		Result Record `json:"result"`
	}
	if err := c.GetJSON(ctx, "/api/now/attachment/"+url.PathEscape(sysID), nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// DownloadAttachment streams the attachment's bytes to w, returning the
// count written. No body-size cap here: attachments are the one
// legitimately large payload.
func (c *Client) DownloadAttachment(ctx context.Context, sysID string, w io.Writer) (int64, error) {
	u := *c.base
	u.Path += "/api/now/attachment/" + url.PathEscape(sysID) + "/file"

	renewed := false
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return 0, fmt.Errorf("build request: %w", err)
		}
		if err := c.auth.apply(req); err != nil {
			return 0, err
		}

		c.log("GET %s (attempt %d/%d)", u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return 0, &NetworkError{Err: err}
		}
		retry, rerr := c.renewOn401(ctx, resp, &renewed)
		if rerr != nil {
			return 0, rerr
		}
		if retry {
			c.log("HTTP 401, retrying with renewed credentials")
			continue
		}
		if wait, ok := retryAfter(resp, attempt); ok {
			c.log("HTTP %d, retrying in %s", resp.StatusCode, wait)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return 0, &NetworkError{Err: ctx.Err()}
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
			return 0, apiError(resp.StatusCode, body)
		}
		n, err := io.Copy(w, resp.Body)
		resp.Body.Close()
		if err != nil {
			return n, &NetworkError{Err: err}
		}
		return n, nil
	}
}

// Table fetches rows from the Table API for the given query parameters.
func (c *Client) Table(ctx context.Context, table string, query url.Values) ([]Record, error) {
	records, _, err := c.TablePage(ctx, table, query)
	return records, err
}

// TablePage fetches rows plus the total match count from the X-Total-Count
// header; total is -1 when the instance does not provide it.
func (c *Client) TablePage(ctx context.Context, table string, query url.Values) ([]Record, int, error) {
	var out struct {
		Result []Record `json:"result"`
	}
	header, err := c.getJSON(ctx, "/api/now/table/"+url.PathEscape(table), query, &out)
	if err != nil {
		return nil, 0, err
	}
	total := -1
	if v := header.Get("X-Total-Count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			total = n
		}
	}
	return out.Result, total, nil
}

// GetRecord fetches a single record by sys_id.
func (c *Client) GetRecord(ctx context.Context, table, sysID string, query url.Values) (Record, error) {
	var out struct {
		Result Record `json:"result"`
	}
	path := "/api/now/table/" + url.PathEscape(table) + "/" + url.PathEscape(sysID)
	if err := c.GetJSON(ctx, path, query, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// Aggregate runs a stats query. The API returns an array when grouped and a
// single object when not; both normalize to a slice.
func (c *Client) Aggregate(ctx context.Context, table string, query url.Values) ([]Record, error) {
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	if err := c.GetJSON(ctx, "/api/now/stats/"+url.PathEscape(table), query, &out); err != nil {
		return nil, err
	}
	var rows []Record
	if err := json.Unmarshal(out.Result, &rows); err == nil {
		return rows, nil
	}
	var single Record
	if err := json.Unmarshal(out.Result, &single); err == nil {
		return []Record{single}, nil
	}
	return nil, &ProtocolError{Err: fmt.Errorf("unexpected stats API response shape")}
}

// Count returns the number of matching rows via the Aggregate API — the
// cheapest possible answer to "how many".
func (c *Client) Count(ctx context.Context, table, encodedQuery string) (int, error) {
	q := url.Values{}
	q.Set("sysparm_count", "true")
	if encodedQuery != "" {
		q.Set("sysparm_query", encodedQuery)
	}
	var out struct {
		Result struct {
			Stats struct {
				Count string `json:"count"`
			} `json:"stats"`
		} `json:"result"`
	}
	if err := c.GetJSON(ctx, "/api/now/stats/"+url.PathEscape(table), q, &out); err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out.Result.Stats.Count)
	if err != nil {
		return 0, &ProtocolError{Err: fmt.Errorf("unexpected count value %q from stats API", out.Result.Stats.Count)}
	}
	return n, nil
}

// isLoginPage reports whether an HTML body is the ServiceNow login page
// rather than legitimate HTML data. Keying on structure — a form posting to
// login.do AND a password input — avoids false positives on prose or links
// that merely mention login.do or a session message. Only the head of the
// body is scanned; the login form lives near the top.
func isLoginPage(body []byte) bool {
	head := body
	if len(head) > 16384 {
		head = head[:16384]
	}
	lower := strings.ToLower(string(head))
	postsToLogin := strings.Contains(lower, "login.do")
	hasPasswordField := strings.Contains(lower, `type="password"`) || strings.Contains(lower, "type=password")
	return postsToLogin && hasPasswordField
}

// apiError builds the typed error for a non-2xx response, extracting the
// ServiceNow error envelope when present.
func apiError(statusCode int, body []byte) *APIError {
	apiErr := &APIError{StatusCode: statusCode}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		apiErr.Message = envelope.Error.Message
		apiErr.Detail = envelope.Error.Detail
	}
	return apiErr
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		// A hostile or misconfigured server must not be able to park the
		// CLI for hours behind a nominal timeout.
		if d > maxRetryDelay {
			return maxRetryDelay
		}
		return d
	}
	backoff := baseRetryWait << (attempt - 1)
	if backoff > maxRetryDelay {
		backoff = maxRetryDelay
	}
	return backoff + rand.N(backoff/2+1)
}

// parseRetryAfter reads both allowed Retry-After forms: delta-seconds and an
// HTTP-date. A past date yields a zero wait.
func parseRetryAfter(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func decodeResponse(resp *http.Response, out any) error {
	defer resp.Body.Close()
	// Read one byte past the cap so truncation is detected, never silent.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return &NetworkError{Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(body) > maxBodyBytes {
			body = body[:maxBodyBytes]
		}
		return apiError(resp.StatusCode, body)
	}

	if out == nil {
		return nil
	}
	if len(body) > maxBodyBytes {
		return &ProtocolError{Err: fmt.Errorf("response exceeds glm's %d MiB buffer - lower --limit or request fewer fields", maxBodyBytes>>20)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &ProtocolError{Err: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}
