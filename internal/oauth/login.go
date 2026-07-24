package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// pkce is one login's proof-key pair (O11): a fresh 43-char verifier and
// its S256 challenge. No plain fallback exists.
type pkce struct {
	verifier  string
	challenge string
}

func newPKCE() (pkce, error) {
	v, err := randomToken(32) // 32 bytes → 43 base64url chars, the RFC 7636 minimum
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(v))
	return pkce{verifier: v, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// The callback pages carry no token material (O11): the code arrives as a
// query parameter, is exchanged immediately, and is never echoed back.
const (
	successPage = `<!doctype html><meta charset="utf-8"><title>glm</title><body style="font-family:sans-serif"><p>Signed in. You can close this tab and return to the terminal.</p>`
	failurePage = `<!doctype html><meta charset="utf-8"><title>glm</title><body style="font-family:sans-serif"><p>Sign-in failed. Return to the terminal for details.</p>`
	donePage    = `<!doctype html><meta charset="utf-8"><title>glm</title><body style="font-family:sans-serif"><p>This sign-in attempt has already completed.</p>`
)

// callbackResult is what the loopback listener hands back: a code or a
// terminal error.
type callbackResult struct {
	code string
	err  error
}

// Login runs the interactive Authorization Code + PKCE flow (O1, O3, O4):
// start a loopback listener, send the user's browser to the instance's
// authorization endpoint, capture the callback, and exchange the code. The
// listener binds loopback only, and the random state gates everything
// (O11): only a callback carrying this attempt's state — success or error,
// since the authorization server echoes state on both — can complete or
// consume the attempt. Anything else (any local page can hit the fixed
// callback URL while a login is pending) gets a 400 and the listener keeps
// waiting for the genuine response.
func Login(ctx context.Context, cfg Config) (*Token, error) {
	if cfg.ClientID == "" {
		return nil, &ConfigError{Msg: "OAuth login needs a client_id on the profile"}
	}
	pk, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomToken(16)
	if err != nil {
		return nil, err
	}

	port := cfg.redirectPort()
	listeners, err := listenLoopback(port)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("cannot listen on localhost:%d for the OAuth callback (%v) — free the port or set redirect_port on the profile (the instance's registry entry must list the same port)", port, err)}
	}

	redirectURI := fmt.Sprintf("http://localhost:%d%s", port, callbackPath)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("scope", cfg.scope())
	q.Set("code_challenge", pk.challenge)
	q.Set("code_challenge_method", "S256")
	authURL := cfg.Endpoints.AuthURL + "?" + q.Encode()

	results := make(chan callbackResult, 1)
	var mu sync.Mutex
	settled := false
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		params := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		mu.Lock()
		defer mu.Unlock()
		if settled {
			fmt.Fprint(w, donePage) //nolint:errcheck
			return
		}
		// The state check comes FIRST, before honoring error or code: only
		// the authorization server's response carries this attempt's random
		// state (echoed on both success and error), so a stateless request
		// from some other local process can neither complete the attempt
		// nor consume it — it gets a 400 and the wait continues.
		if subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(state)) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, failurePage) //nolint:errcheck
			cfg.notify("glm: ignored a callback with a missing or mismatched state — still waiting for the sign-in")
			return
		}
		res := callbackResult{}
		switch {
		case params.Get("error") != "":
			res.err = &AuthError{Msg: "authorization refused", Detail: joinNonEmpty(params.Get("error"), params.Get("error_description"))}
		case params.Get("code") == "":
			res.err = &AuthError{Msg: "callback carried no authorization code"}
		default:
			res.code = params.Get("code")
		}
		if res.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, failurePage) //nolint:errcheck
		} else {
			fmt.Fprint(w, successPage) //nolint:errcheck
		}
		settled = true
		results <- res
	})}
	for _, ln := range listeners {
		go srv.Serve(ln) //nolint:errcheck
	}
	// Graceful shutdown, not Close: the handler queues its result before
	// net/http finishes writing the response, so an abrupt Close can sever
	// the connection mid-page and the browser shows a reset instead of the
	// success/failure page. Shutdown lets the in-flight response complete.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	cfg.notify("Open this URL to authorize glm (a browser window should open):\n  " + authURL)
	opener := cfg.OpenBrowser
	if opener == nil {
		opener = openBrowser
	}
	_ = opener(authURL) // best effort — the URL was printed

	timeout := cfg.timeout()
	select {
	case res := <-results:
		if res.err != nil {
			return nil, res.err
		}
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", res.code)
		form.Set("redirect_uri", redirectURI)
		form.Set("client_id", cfg.ClientID)
		form.Set("code_verifier", pk.verifier)
		if cfg.ClientSecret != "" {
			// Confidential fallback (O9) — only when the instance's registry
			// entry demands a secret despite the public-client recommendation.
			form.Set("client_secret", cfg.ClientSecret)
		}
		return postToken(ctx, cfg, form)
	case <-time.After(timeout):
		return nil, &AuthError{Msg: fmt.Sprintf("login timed out after %s waiting for the browser callback", timeout)}
	case <-ctx.Done():
		return nil, &AuthError{Msg: "login canceled"}
	}
}

// listenLoopback binds the callback port on both loopback families (O4).
// Go resolves "localhost" to a single address for Listen, but the user's
// browser resolves it per OS policy and may deliver the callback over the
// other family — where a different process could already be listening,
// silently receiving the authorization code or hanging the login. Binding
// both closes that gap. IPv6 is skipped only when the host has no usable
// IPv6 loopback at all (probed via [::1]:0), so an in-use [::1]:port is
// still a hard port conflict, never a silent misdelivery.
func listenLoopback(port int) ([]net.Listener, error) {
	ln4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		if probe, perr := net.Listen("tcp6", "[::1]:0"); perr == nil {
			// IPv6 loopback works in general, so this port specifically is
			// taken on the family the browser might use.
			probe.Close()
			ln4.Close()
			return nil, err
		}
		return []net.Listener{ln4}, nil
	}
	return []net.Listener{ln4, ln6}, nil
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + ": " + b
	}
}

// openBrowser is the platform-default browser launcher, used when
// Config.OpenBrowser is nil. Best-effort: Login always prints the URL too.
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
