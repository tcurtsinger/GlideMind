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
		{in: "  ", wantErr: true},
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
