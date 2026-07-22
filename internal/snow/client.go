// Package snow is the transport-agnostic ServiceNow client shared by every
// glm front-end (CLI today, HTTP/MCP facade later): instance normalization,
// auth, bounded retries, and error mapping. No terminal concerns live here.
package snow

import (
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

		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable
		if retryable && attempt < maxAttempts {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			wait := retryDelay(resp, attempt)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return &NetworkError{Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
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

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
