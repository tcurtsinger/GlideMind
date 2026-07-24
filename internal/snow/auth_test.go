package snow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// rotatingAuth is a renewable credential: each retryAuth advances to the
// next token and reports success — standing in for the refreshing OAuth
// implementations that arrive with DESIGN-OAUTH.md's later phases.
type rotatingAuth struct {
	tokens []string
	idx    int
	renews int
}

func (a *rotatingAuth) apply(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.tokens[a.idx])
	return nil
}

func (a *rotatingAuth) retryAuth(context.Context) (bool, error) {
	a.renews++
	if a.idx+1 < len(a.tokens) {
		a.idx++
	}
	return true, nil
}

// failingAuth attempts renewal and fails — the revoked-session shape.
type failingAuth struct{ err error }

func (a failingAuth) apply(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer revoked")
	return nil
}

func (a failingAuth) retryAuth(context.Context) (bool, error) { return false, a.err }

// newAuthClient builds a client around an injected authenticator — the seam
// PR 2's OAuth implementations plug into.
func newAuthClient(t *testing.T, handler http.Handler, auth authenticator) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &Client{
		base:     u,
		hc:       &http.Client{Timeout: 5 * time.Second, CheckRedirect: secureRedirect},
		username: "u",
		auth:     auth,
	}
}

func TestBearerSendsToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c, err := NewBearer(srv.URL, "tok-1", "svc", 5*time.Second)
	if err != nil {
		t.Fatalf("NewBearer: %v", err)
	}
	if err := c.GetJSON(context.Background(), "/api/now/table/x", nil, nil); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tok-1")
	}
	if c.Username() != "svc" {
		t.Errorf("Username() = %q, want %q (per-user cache keys depend on it)", c.Username(), "svc")
	}
}

func TestNewBearerValidation(t *testing.T) {
	if _, err := NewBearer("https://acme.service-now.com", "", "svc", time.Second); err == nil {
		t.Error("empty token must be rejected")
	}
	if _, err := NewBearer("https://acme.service-now.com", "tok", "", time.Second); err == nil {
		t.Error("empty username must be rejected — cache keys need an identity")
	}
	if _, err := NewBearer("https://acme.service-now.com", "tok", "svc", 0); err == nil {
		t.Error("zero timeout must be rejected")
	}
}

func TestStatic401SurfacesImmediately(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c, err := NewBearer(srv.URL, "expired", "svc", 5*time.Second)
	if err != nil {
		t.Fatalf("NewBearer: %v", err)
	}
	err = c.GetJSON(context.Background(), "/api/now/table/x", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want APIError 401, got %v", err)
	}
	if apiErr.ExitCode() != exit.Auth {
		t.Errorf("401 must map to exit %d, got %d", exit.Auth, apiErr.ExitCode())
	}
	if hits != 1 {
		t.Errorf("a static credential must not retry a 401, got %d attempts", hits)
	}
}

func TestRenewOn401RetriesOnceWithNewCredential(t *testing.T) {
	hits := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
	})
	auth := &rotatingAuth{tokens: []string{"old", "new"}}
	c := newAuthClient(t, handler, auth)
	if err := c.GetJSON(context.Background(), "/api/now/table/x", nil, nil); err != nil {
		t.Fatalf("a renewable credential must recover from a 401: %v", err)
	}
	if hits != 2 {
		t.Errorf("want exactly 2 attempts (401 + renewed retry), got %d", hits)
	}
	if auth.renews != 1 {
		t.Errorf("want exactly 1 renewal, got %d", auth.renews)
	}
}

func TestRenewOn401IsSpentAfterOneRetry(t *testing.T) {
	// Even an authenticator that always claims success must not loop: the
	// single renewal is spent after one retry and the 401 surfaces.
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	auth := &rotatingAuth{tokens: []string{"a", "b"}}
	c := &Client{base: u, hc: &http.Client{Timeout: 5 * time.Second, CheckRedirect: secureRedirect}, username: "u", auth: auth}
	err := c.GetJSON(context.Background(), "/api/now/table/x", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want the persistent 401 surfaced, got %v", err)
	}
	if hits != 2 {
		t.Errorf("want exactly 2 attempts (original + one renewed retry), got %d", hits)
	}
}

func TestRenewalErrorReplacesRaw401(t *testing.T) {
	// Codex P2 (PR #25): when renewal is attempted and FAILS, that error —
	// which names the remedy — must replace the raw 401, not be collapsed
	// into "not renewable".
	hits := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	})
	remedy := errors.New(`OAuth session expired — run: glm profile login p`)
	c := newAuthClient(t, handler, failingAuth{err: remedy})
	err := c.GetJSON(context.Background(), "/api/now/table/x", nil, nil)
	if !errors.Is(err, remedy) {
		t.Fatalf("want the renewal error surfaced, got %v", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Error("the raw 401 must be replaced by the actionable renewal error")
	}
	if hits != 1 {
		t.Errorf("no retry after a failed renewal, got %d attempts", hits)
	}
}

func TestRenewOn401AppliesToWrites(t *testing.T) {
	// A 401 is rejected before the instance processes the request, so the
	// one renewed retry keeps Raw's effectively-once write contract: the
	// write that counts still goes on the wire exactly once.
	hits := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"result":{"sys_id":"x"}}`)) //nolint:errcheck
	})
	auth := &rotatingAuth{tokens: []string{"old", "new"}}
	c := newAuthClient(t, handler, auth)
	if _, err := c.Raw(context.Background(), http.MethodPost, "/api/now/table/x", nil, []byte(`{}`)); err != nil {
		t.Fatalf("a renewable credential must recover a write from a 401: %v", err)
	}
	if hits != 2 {
		t.Errorf("want exactly 2 attempts (401 + renewed retry), got %d", hits)
	}
}
