package snow

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

func TestRawDoesNotRetryWrites(t *testing.T) {
	attempts := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.Raw(context.Background(), http.MethodPost, "/api/write", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("want the 503 surfaced as an error")
	}
	if attempts != 1 {
		t.Errorf("a non-GET request must hit the wire exactly once, got %d attempts", attempts)
	}
}

func TestRawRetriesGet(t *testing.T) {
	attempts := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
	}))
	if _, err := c.Raw(context.Background(), http.MethodGet, "/api/read", nil, nil); err != nil {
		t.Fatalf("GET should retry transparently: %v", err)
	}
	if attempts != 2 {
		t.Errorf("want 2 attempts, got %d", attempts)
	}
}

func TestRawRejectsOversizedResponse(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), maxBodyBytes+1)) //nolint:errcheck
	}))
	_, err := c.Raw(context.Background(), http.MethodGet, "/api/huge", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "MiB buffer") {
		t.Errorf("oversized response must error loudly, not truncate, got: %v", err)
	}
}

func TestNormalizeInstance(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "acme", want: "https://acme.service-now.com"},
		{in: "acme.service-now.com", want: "https://acme.service-now.com"},
		{in: "https://acme.service-now.com/", want: "https://acme.service-now.com"},
		{in: "https://acme.service-now.com/nav_to.do?uri=incident_list.do", want: "https://acme.service-now.com"},
		{in: "acme.service-now.com/now/nav/ui/classic/params/target/incident.do", want: "https://acme.service-now.com"},
		{in: "http://localhost:8080", want: "http://localhost:8080"},
		{in: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{in: "  ", wantErr: true},
		// H-02: credentials embedded in the URL are refused, never stored.
		{in: "https://user:pass@acme.service-now.com", wantErr: true},
		{in: "https://user@acme.service-now.com", wantErr: true},
		// H-01: plaintext http is refused off loopback; odd schemes too.
		{in: "http://acme.service-now.com", wantErr: true},
		{in: "ftp://acme.service-now.com", wantErr: true},
	}
	for _, c := range cases {
		u, err := NormalizeInstance(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeInstance(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeInstance(%q): %v", c.in, err)
			continue
		}
		if u.String() != c.want {
			t.Errorf("NormalizeInstance(%q) = %q, want %q", c.in, u.String(), c.want)
		}
	}
}

func TestNonGetDoesNotFollowRedirects(t *testing.T) {
	// H-01: a 307 on a write must not replay the body to /finish; the 3xx
	// is surfaced to the caller as an error and hits the wire exactly once.
	var finishHits int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/finish", http.StatusTemporaryRedirect)
		case "/finish":
			finishHits++
			w.WriteHeader(http.StatusOK)
		}
	}))
	_, err := c.Raw(context.Background(), http.MethodPost, "/start", nil, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("a redirected write must surface the 3xx as an error, not follow it")
	}
	if finishHits != 0 {
		t.Errorf("write body was replayed to the redirect target %d time(s)", finishHits)
	}
}

func TestGetRefusesCrossOriginRedirect(t *testing.T) {
	// H-01: a GET redirect off the configured origin is refused, keeping
	// the Basic credential on the configured host.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/leak", http.StatusFound)
	}))
	if _, err := c.Raw(context.Background(), http.MethodGet, "/api/read", nil, nil); err == nil {
		t.Error("cross-origin GET redirect must be refused")
	}
}

func TestNewBasicRejectsNonPositiveTimeout(t *testing.T) {
	if _, err := NewBasic("https://acme.service-now.com", "u", "p", 0); err == nil {
		t.Error("zero timeout must be rejected (it disables the client deadline)")
	}
}

func TestParseRetryAfterForms(t *testing.T) {
	if d, ok := parseRetryAfter("2"); !ok || d != 2*time.Second {
		t.Errorf("delta-seconds: got %v, %v", d, ok)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Errorf("HTTP-date future: got %v, %v", d, ok)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(past); !ok || d != 0 {
		t.Errorf("HTTP-date past should be zero wait: got %v, %v", d, ok)
	}
	if _, ok := parseRetryAfter("soon"); ok {
		t.Error("garbage Retry-After must not parse")
	}
}

func TestRetryDelayCapped(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "99999")
	if d := retryDelay(resp, 1); d != maxRetryDelay {
		t.Errorf("a huge Retry-After must cap at %s, got %s", maxRetryDelay, d)
	}
}

func TestRawPreservesEscapedPath(t *testing.T) {
	var gotURI string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	if _, err := c.Raw(context.Background(), http.MethodGet, "/api/x/a%2Fb", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURI, "a%2Fb") {
		t.Errorf("escaped slash was re-encoded or decoded: %q", gotURI)
	}
}

func TestRawRejectsAbsolutePath(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := c.Raw(context.Background(), http.MethodGet, "http://evil.example/x", nil, nil); err == nil {
		t.Error("an absolute URL as the request path must be rejected")
	}
}

func TestRawAbortsOnHTML(t *testing.T) {
	// A REST call that returns the UI session page must abort with one line,
	// never dump the whole HTML document to the caller.
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Write([]byte("<!DOCTYPE html><html><body>You are not logged in</body></html>")) //nolint:errcheck
	}))
	data, err := c.Raw(context.Background(), http.MethodGet, "/api/now/table/incident", nil, nil)
	if err == nil {
		t.Fatal("an HTML response must be an error, not returned data")
	}
	if data != nil {
		t.Error("the HTML body must not be handed back to the caller")
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Errorf("HTML response should be a ProtocolError, got %T", err)
	}
}

func TestMalformedJSONIsProtocolError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`)) //nolint:errcheck
	}))
	_, err := c.Table(context.Background(), "incident", nil)
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("malformed 200 body should be a ProtocolError, got %T", err)
	}
	if pe.ExitCode() != exit.API {
		t.Errorf("ProtocolError must map to exit %d, got %d", exit.API, pe.ExitCode())
	}
}

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewBasic(srv.URL, "user", "pass", 5*time.Second)
	if err != nil {
		t.Fatalf("NewBasic: %v", err)
	}
	return c, srv
}

func TestTableSuccessAndAuthHeader(t *testing.T) {
	var gotUser, gotPass string
	var gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":[{"number":"INC0000001"}]}`)) //nolint:errcheck
	}))

	q := url.Values{}
	q.Set("sysparm_limit", "1")
	recs, err := c.Table(context.Background(), "incident", q)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	if len(recs) != 1 || recs[0]["number"] != "INC0000001" {
		t.Fatalf("unexpected records: %+v", recs)
	}
	if gotUser != "user" || gotPass != "pass" {
		t.Fatalf("basic auth not sent: %q/%q", gotUser, gotPass)
	}
	if gotPath != "/api/now/table/incident" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		wantExit int
	}{
		{status: 401, wantExit: exit.Auth},
		{status: 403, wantExit: exit.Auth},
		{status: 404, wantExit: exit.NotFound},
		{status: 500, wantExit: exit.API},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"error":{"message":"boom","detail":"why"},"status":"failure"}`)) //nolint:errcheck
		}))
		_, err := c.Table(context.Background(), "incident", url.Values{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: expected APIError, got %v", tc.status, err)
		}
		if apiErr.ExitCode() != tc.wantExit {
			t.Errorf("status %d: exit %d, want %d", tc.status, apiErr.ExitCode(), tc.wantExit)
		}
		if apiErr.Message != "boom" {
			t.Errorf("status %d: envelope not decoded: %+v", tc.status, apiErr)
		}
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	attempts := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
	}))

	if _, err := c.Table(context.Background(), "incident", url.Values{}); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	attempts := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	_, err := c.Table(context.Background(), "incident", url.Values{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Fatalf("expected final 503 APIError, got %v", err)
	}
	if attempts != maxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, maxAttempts)
	}
}

func TestNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // dead server → connection refused
	c, err := NewBasic(srv.URL, "user", "pass", 2*time.Second)
	if err != nil {
		t.Fatalf("NewBasic: %v", err)
	}

	_, err = c.Table(context.Background(), "incident", url.Values{})
	var netErr *NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("expected NetworkError, got %v", err)
	}
	if netErr.ExitCode() != exit.Network {
		t.Fatalf("exit = %d, want %d", netErr.ExitCode(), exit.Network)
	}
}
