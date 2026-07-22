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
)

// Record is one row from the Table API. Value types depend on the
// sysparm_display_value mode requested by the caller.
type Record = map[string]any

// Client talks to one instance with basic auth (v1; OAuth client-credentials
// is a planned follow-up).
type Client struct {
	base     *url.URL
	hc       *http.Client
	username string
	password string
	logf     func(format string, args ...any)
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
	// Users paste browser URLs (e.g. .../nav_to.do?uri=...). The REST API
	// always lives at the host root, so drop any path outright — keeping it
	// would silently aim every request at /nav_to.do/api/now/....
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
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
	return &Client{
		base:     u,
		hc:       &http.Client{Timeout: timeout},
		username: username,
		password: password,
	}, nil
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

	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.SetBasicAuth(c.username, c.password)
		req.Header.Set("Accept", "application/json")

		c.log("GET %s (attempt %d/%d)", u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, &NetworkError{Err: err}
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
// showed.
func (c *Client) Raw(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()

	for attempt := 1; ; attempt++ {
		var rdr io.Reader
		if len(body) > 0 {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.SetBasicAuth(c.username, c.password)
		req.Header.Set("Accept", "application/json")
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		c.log("%s %s (attempt %d/%d)", method, u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, &NetworkError{Err: err}
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
		return data, nil
	}
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

	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return 0, fmt.Errorf("build request: %w", err)
		}
		req.SetBasicAuth(c.username, c.password)

		c.log("GET %s (attempt %d/%d)", u.Redacted(), attempt, maxAttempts)
		resp, err := c.hc.Do(req)
		if err != nil {
			return 0, &NetworkError{Err: err}
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
	return nil, fmt.Errorf("unexpected stats API response shape")
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
		return 0, fmt.Errorf("unexpected count value %q from stats API", out.Result.Stats.Count)
	}
	return n, nil
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
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	backoff := baseRetryWait << (attempt - 1)
	return backoff + rand.N(backoff/2+1)
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
		return fmt.Errorf("response exceeds glm's %d MiB buffer - lower --limit or request fewer fields", maxBodyBytes>>20)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
