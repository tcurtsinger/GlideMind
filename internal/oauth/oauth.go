package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

const (
	// DefaultRedirectPort is the fixed loopback callback port (O4): "glm"
	// on a phone keypad. ServiceNow matches redirect URIs exactly, so the
	// port is baked into each instance's Application Registry entry;
	// redirect_port on the profile overrides for conflicts.
	DefaultRedirectPort = 8456

	callbackPath   = "/callback"
	defaultScope   = "useraccount"
	defaultTimeout = 5 * time.Minute

	// maxTokenBody bounds token-endpoint responses; real ones are tiny.
	maxTokenBody = 1 << 20
)

// Endpoints are one instance's OAuth endpoints, injectable for tests.
type Endpoints struct {
	AuthURL  string
	TokenURL string
}

// InstanceEndpoints derives the standard ServiceNow endpoints from a
// normalized instance base URL.
func InstanceEndpoints(baseURL string) Endpoints {
	base := strings.TrimRight(baseURL, "/")
	return Endpoints{
		AuthURL:  base + "/oauth_auth.do",
		TokenURL: base + "/oauth_token.do",
	}
}

// Config parameterizes one flow. Zero values take the documented defaults;
// only Endpoints and ClientID are always required.
type Config struct {
	Endpoints Endpoints
	ClientID  string
	// ClientSecret is empty for the recommended public PKCE client (O9)
	// and required for client-credentials (O13).
	ClientSecret string
	Scope        string        // default "useraccount"
	RedirectPort int           // default DefaultRedirectPort
	Timeout      time.Duration // interactive login wait, default 5m
	// OpenBrowser launches the user's browser; nil uses the platform
	// default. Best-effort — Login always emits the URL via Notify too.
	OpenBrowser func(url string) error
	// Notify surfaces login progress (the authorization URL); nil = silent.
	Notify func(msg string)
	// HTTP is the token-endpoint client; nil uses a 30s-timeout default.
	HTTP *http.Client
}

func (c Config) scope() string {
	if c.Scope != "" {
		return c.Scope
	}
	return defaultScope
}

func (c Config) redirectPort() int {
	if c.RedirectPort > 0 {
		return c.RedirectPort
	}
	return DefaultRedirectPort
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c Config) httpClient() *http.Client {
	// An injected client (tests) owns its own redirect policy.
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: refuseRedirect}
}

// refuseRedirect stops the token client from following any redirect: a
// token endpoint has no legitimate reason to redirect, and following a
// 307/308 would replay the form — authorization code, refresh token,
// possibly a client secret — to wherever it points (the same rule snow's
// transport applies to writes). The 3xx surfaces as a rejected request.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (c Config) notify(msg string) {
	if c.Notify != nil {
		c.Notify(msg)
	}
}

// AuthError is a failed OAuth flow — denied consent, a bad callback, a
// rejected token request, or a login timeout. Exit 2 per DESIGN-OAUTH O12.
type AuthError struct {
	Msg    string
	Detail string
}

func (e *AuthError) Error() string {
	if e.Detail != "" {
		return e.Msg + ": " + e.Detail
	}
	return e.Msg
}

func (e *AuthError) ExitCode() int { return exit.Auth }

// ConfigError is OAuth misconfiguration the user must fix before a flow can
// run — a missing client_id, an occupied callback port. Exit 1.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }
func (e *ConfigError) ExitCode() int { return exit.Usage }

// NetworkError wraps transport failures reaching the OAuth endpoints,
// mirroring snow's mapping (exit 4).
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "network: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }
func (e *NetworkError) ExitCode() int { return exit.Network }

// Refresh exchanges a refresh token for a new access token (O6). ServiceNow
// does not rotate refresh tokens; when the response omits one, the old
// token is carried forward.
func Refresh(ctx context.Context, cfg Config, refreshToken string) (*Token, error) {
	if refreshToken == "" {
		return nil, &AuthError{Msg: "no refresh token stored"}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	t, err := postToken(ctx, cfg, form)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// ClientCredentials mints an access token from the client secret (O13).
// No refresh token exists in this grant — renewal is simply another mint,
// so the flow is self-healing while the secret is valid.
func ClientCredentials(ctx context.Context, cfg Config) (*Token, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, &ConfigError{Msg: "client-credentials needs a client_id and a stored client secret"}
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	if s := cfg.scope(); s != "" {
		form.Set("scope", s)
	}
	return postToken(ctx, cfg, form)
}

// postToken sends one form to the token endpoint and parses the response.
// glm trusts the expires_in it is handed, not assumptions about instance
// defaults (O6).
func postToken(ctx context.Context, cfg Config, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoints.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return nil, &NetworkError{Err: err}
	}

	var body struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		Error        string      `json:"error"`
		ErrorDesc    string      `json:"error_description"`
	}
	// Tolerate an unparsable body: the status code still classifies it.
	_ = json.Unmarshal(data, &body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := strings.TrimSpace(strings.Join([]string{body.Error, body.ErrorDesc}, " "))
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			detail += fmt.Sprintf(" (HTTP %d)", resp.StatusCode)
		}
		return nil, &AuthError{Msg: "token request rejected", Detail: detail}
	}
	if body.AccessToken == "" {
		return nil, &AuthError{Msg: "token endpoint returned no access token"}
	}
	t := &Token{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken}
	if secs, err := body.ExpiresIn.Int64(); err == nil && secs > 0 {
		t.Expiry = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return t, nil
}
