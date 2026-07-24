package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// freePort grabs an ephemeral port for a test login's callback listener.
// The tiny close-then-reuse race is acceptable in tests; production uses
// the fixed registry-matched port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestPKCEPair(t *testing.T) {
	a, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if len(a.verifier) < 43 {
		t.Errorf("verifier %d chars, want >= 43 (RFC 7636 minimum)", len(a.verifier))
	}
	sum := sha256.Sum256([]byte(a.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); a.challenge != want {
		t.Errorf("challenge is not S256(verifier): %q vs %q", a.challenge, want)
	}
	b, _ := newPKCE()
	if a.verifier == b.verifier {
		t.Error("two logins must never share a verifier")
	}
}

func TestInstanceEndpoints(t *testing.T) {
	e := InstanceEndpoints("https://acme.service-now.com/")
	if e.AuthURL != "https://acme.service-now.com/oauth_auth.do" || e.TokenURL != "https://acme.service-now.com/oauth_token.do" {
		t.Errorf("unexpected endpoints: %+v", e)
	}
}

func TestTokenValid(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		tok  *Token
		want bool
	}{
		{"nil", nil, false},
		{"empty access token", &Token{Expiry: now.Add(time.Hour)}, false},
		{"live", &Token{AccessToken: "a", Expiry: now.Add(2 * time.Minute)}, true},
		{"inside skew", &Token{AccessToken: "a", Expiry: now.Add(30 * time.Second)}, false},
		{"expired", &Token{AccessToken: "a", Expiry: now.Add(-time.Minute)}, false},
		{"unknown expiry", &Token{AccessToken: "a"}, true},
	}
	for _, c := range cases {
		if got := c.tok.Valid(now); got != c.want {
			t.Errorf("%s: Valid = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTokenEncodeDecode(t *testing.T) {
	in := &Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour).UTC()}
	s, err := in.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeToken(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AccessToken != in.AccessToken || out.RefreshToken != in.RefreshToken || !out.Expiry.Equal(in.Expiry) {
		t.Errorf("roundtrip mismatch: %+v vs %+v", out, in)
	}
	if _, err := decodeToken("not json"); err == nil {
		t.Error("corrupt blob must error")
	}
}

// loginHarness drives a full interactive login headless: it captures the
// auth URL handed to the injected browser-opener and plays the browser's
// part against the loopback listener.
type loginHarness struct {
	cfg      Config
	authURLs chan string
	tokenReq chan url.Values
}

func newLoginHarness(t *testing.T, tokenHandler http.HandlerFunc) *loginHarness {
	t.Helper()
	h := &loginHarness{authURLs: make(chan string, 1), tokenReq: make(chan url.Values, 1)}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		h.tokenReq <- r.PostForm
		tokenHandler(w, r)
	}))
	t.Cleanup(tokenSrv.Close)
	h.cfg = Config{
		Endpoints:    Endpoints{AuthURL: "https://inst.example/oauth_auth.do", TokenURL: tokenSrv.URL},
		ClientID:     "cid",
		RedirectPort: freePort(t),
		Timeout:      5 * time.Second,
		OpenBrowser:  func(u string) error { h.authURLs <- u; return nil },
	}
	return h
}

// browse waits for Login to emit the auth URL and returns its parsed
// query parameters plus the redirect URI.
func (h *loginHarness) browse(t *testing.T) (url.Values, string) {
	t.Helper()
	select {
	case raw := <-h.authURLs:
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse auth URL: %v", err)
		}
		return u.Query(), u.Query().Get("redirect_uri")
	case <-time.After(5 * time.Second):
		t.Fatal("Login never opened the browser")
		return nil, ""
	}
}

func callback(t *testing.T, redirectURI string, params url.Values) *http.Response {
	t.Helper()
	resp, err := http.Get(redirectURI + "?" + params.Encode())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	resp.Body.Close()
	return resp
}

func TestLoginFullFlow(t *testing.T) {
	h := newLoginHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","expires_in":1800,"token_type":"Bearer","scope":"useraccount"}`) //nolint:errcheck
	})

	type result struct {
		tok *Token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := Login(context.Background(), h.cfg)
		done <- result{tok, err}
	}()

	q, redirectURI := h.browse(t)
	if q.Get("response_type") != "code" || q.Get("client_id") != "cid" ||
		q.Get("code_challenge_method") != "S256" || q.Get("scope") != "useraccount" {
		t.Errorf("auth URL missing required params: %v", q)
	}
	if q.Get("state") == "" || q.Get("code_challenge") == "" {
		t.Errorf("auth URL must carry state and code_challenge: %v", q)
	}
	wantURI := fmt.Sprintf("http://localhost:%d/callback", h.cfg.RedirectPort)
	if redirectURI != wantURI {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, wantURI)
	}

	callback(t, redirectURI, url.Values{"code": {"the-code"}, "state": {q.Get("state")}})

	res := <-done
	if res.err != nil {
		t.Fatalf("Login: %v", res.err)
	}
	if res.tok.AccessToken != "at" || res.tok.RefreshToken != "rt" {
		t.Errorf("token = %+v", res.tok)
	}
	remaining := time.Until(res.tok.Expiry)
	if remaining < 25*time.Minute || remaining > 31*time.Minute {
		t.Errorf("expiry not derived from expires_in=1800: %v away", remaining)
	}

	form := <-h.tokenReq
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" ||
		form.Get("client_id") != "cid" || form.Get("redirect_uri") != wantURI {
		t.Errorf("exchange form: %v", form)
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != q.Get("code_challenge") {
		t.Errorf("code_verifier does not match the challenge sent to the auth endpoint")
	}
	if form.Get("client_secret") != "" {
		t.Error("public client must not send a client_secret")
	}
}

func TestLoginStateMismatchIsTerminal(t *testing.T) {
	h := newLoginHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint must never be called on a forged callback")
	})
	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), h.cfg)
		done <- err
	}()
	_, redirectURI := h.browse(t)
	resp := callback(t, redirectURI, url.Values{"code": {"x"}, "state": {"forged"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("forged callback should get HTTP 400, got %d", resp.StatusCode)
	}
	err := <-done
	var authErr *AuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("want state-mismatch AuthError, got %v", err)
	}
	if authErr.ExitCode() != exit.Auth {
		t.Errorf("auth failures map to exit %d, got %d", exit.Auth, authErr.ExitCode())
	}
}

func TestLoginDenied(t *testing.T) {
	h := newLoginHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint must never be called when authorization was refused")
	})
	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), h.cfg)
		done <- err
	}()
	q, redirectURI := h.browse(t)
	callback(t, redirectURI, url.Values{"error": {"access_denied"}, "state": {q.Get("state")}})
	err := <-done
	var authErr *AuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("want access_denied AuthError, got %v", err)
	}
}

func TestLoginTimeout(t *testing.T) {
	h := newLoginHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.cfg.Timeout = 150 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), h.cfg)
		done <- err
	}()
	h.browse(t) // consume the auth URL; never call back
	err := <-done
	var authErr *AuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout AuthError, got %v", err)
	}
}

func TestLoginPortInUse(t *testing.T) {
	port := freePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	cfg := Config{Endpoints: Endpoints{AuthURL: "https://x/a", TokenURL: "https://x/t"}, ClientID: "cid", RedirectPort: port}
	_, err = Login(context.Background(), cfg)
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "redirect_port") {
		t.Fatalf("want ConfigError naming redirect_port, got %v", err)
	}
	if cfgErr.ExitCode() != exit.Usage {
		t.Errorf("misconfiguration maps to exit %d, got %d", exit.Usage, cfgErr.ExitCode())
	}
}

func TestLoginPortInUseIPv6(t *testing.T) {
	// Codex P2 (PR #24): the browser may resolve localhost to ::1, so a
	// port owned by another process on the IPv6 loopback is a hard
	// conflict — never a silent misdelivery of the authorization code.
	port := freePort(t)
	ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Skip("no usable IPv6 loopback on this host")
	}
	defer ln6.Close()
	cfg := Config{Endpoints: Endpoints{AuthURL: "https://x/a", TokenURL: "https://x/t"}, ClientID: "cid", RedirectPort: port}
	_, err = Login(context.Background(), cfg)
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "redirect_port") {
		t.Fatalf("want ConfigError for an IPv6-occupied port, got %v", err)
	}
}

func TestLoginCallbackOverIPv6(t *testing.T) {
	if ln, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		t.Skip("no usable IPv6 loopback on this host")
	} else {
		ln.Close()
	}
	h := newLoginHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at6","expires_in":1800}`) //nolint:errcheck
	})
	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), h.cfg)
		done <- err
	}()
	q, _ := h.browse(t)
	// The browser resolved localhost to ::1: the callback must still land.
	v6URI := fmt.Sprintf("http://[::1]:%d/callback", h.cfg.RedirectPort)
	callback(t, v6URI, url.Values{"code": {"c6"}, "state": {q.Get("state")}})
	if err := <-done; err != nil {
		t.Fatalf("callback over IPv6 loopback must complete the login: %v", err)
	}
}

func TestLoginRequiresClientID(t *testing.T) {
	_, err := Login(context.Background(), Config{Endpoints: Endpoints{AuthURL: "https://x/a", TokenURL: "https://x/t"}})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("want ConfigError for missing client_id, got %v", err)
	}
}

func TestRefreshKeepsUnrotatedToken(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-at","expires_in":1800}`) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Endpoints: Endpoints{TokenURL: srv.URL}, ClientID: "cid"}
	tok, err := Refresh(context.Background(), cfg, "the-rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "the-rt" || form.Get("client_id") != "cid" {
		t.Errorf("refresh form: %v", form)
	}
	if tok.AccessToken != "new-at" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "the-rt" {
		t.Errorf("an unrotated refresh token must be carried forward, got %q", tok.RefreshToken)
	}
}

func TestRefreshRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token expired"}`) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Endpoints: Endpoints{TokenURL: srv.URL}, ClientID: "cid"}
	_, err := Refresh(context.Background(), cfg, "dead-rt")
	var authErr *AuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("want AuthError with the server's reason, got %v", err)
	}
	if _, err := Refresh(context.Background(), cfg, ""); err == nil {
		t.Error("empty refresh token must fail without a request")
	}
}

func TestRefreshRequiresClientID(t *testing.T) {
	// Codex P2 (PR #24): a misconfigured profile must get the corrective
	// ConfigError before any network call — the same contract Login and
	// ClientCredentials already honor — not a runtime auth failure from
	// posting an empty client_id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint must not be contacted on misconfiguration")
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Endpoints: Endpoints{TokenURL: srv.URL}}
	_, err := Refresh(context.Background(), cfg, "rt")
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("want ConfigError for missing client_id, got %v", err)
	}
	if cfgErr.ExitCode() != exit.Usage {
		t.Errorf("misconfiguration maps to exit %d, got %d", exit.Usage, cfgErr.ExitCode())
	}
}

func TestTokenRequestRefusesRedirect(t *testing.T) {
	// Codex P2 (PR #24): a redirected token request must never replay the
	// form — code, refresh token, possibly a client secret — to the
	// redirect target. The 3xx surfaces as a rejected token request.
	leaked := 0
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked++
	}))
	t.Cleanup(attacker.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Endpoints: Endpoints{TokenURL: srv.URL}, ClientID: "cid", ClientSecret: "shh"}
	_, err := Refresh(context.Background(), cfg, "rt")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("redirected token request must be rejected, got %v", err)
	}
	if leaked != 0 {
		t.Errorf("token form was replayed to the redirect target %d time(s)", leaked)
	}
}

func TestClientCredentials(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"cc-at","expires_in":1800}`) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Endpoints: Endpoints{TokenURL: srv.URL}, ClientID: "cid", ClientSecret: "shh"}
	tok, err := ClientCredentials(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "cid" || form.Get("client_secret") != "shh" {
		t.Errorf("mint form: %v", form)
	}
	if tok.AccessToken != "cc-at" || tok.RefreshToken != "" {
		t.Errorf("token = %+v (client-credentials has no refresh token)", tok)
	}

	cfg.ClientSecret = ""
	var cfgErr *ConfigError
	if _, err := ClientCredentials(context.Background(), cfg); !errors.As(err, &cfgErr) {
		t.Errorf("missing secret must be a ConfigError, got %v", err)
	}
}
