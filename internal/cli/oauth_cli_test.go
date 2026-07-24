package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/exit"
	"github.com/tcurtsinger/GlideMind/internal/oauth"
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

// fakeStore swaps the keyring-backed token/secret seams for in-memory maps —
// cli tests must never touch the real OS keyring (standing harness rule).
type fakeStore struct {
	tokens  map[string]*oauth.Token
	secrets map[string]string
	saves   int
}

func stubStore(t *testing.T) *fakeStore {
	t.Helper()
	fs := &fakeStore{tokens: map[string]*oauth.Token{}, secrets: map[string]string{}}
	origLT, origST, origDT := loadStoredToken, saveStoredToken, deleteStoredToken
	origLS, origSS, origDS := loadClientSecret, saveClientSecret, deleteClientSecret
	origDP := deletePassword
	loadStoredToken = func(p string) (*oauth.Token, bool) { tok, ok := fs.tokens[p]; return tok, ok }
	saveStoredToken = func(p string, tok *oauth.Token) error { fs.saves++; fs.tokens[p] = tok; return nil }
	deleteStoredToken = func(p string) error { delete(fs.tokens, p); return nil }
	loadClientSecret = func(p string) (string, bool) { s, ok := fs.secrets[p]; return s, ok }
	saveClientSecret = func(p, v string) error { fs.secrets[p] = v; return nil }
	deleteClientSecret = func(p string) error { delete(fs.secrets, p); return nil }
	// The password delete too: `profile remove` reaches the real keyring
	// otherwise, which does not exist on headless CI (Linux).
	deletePassword = func(string) error { return nil }
	t.Cleanup(func() {
		loadStoredToken, saveStoredToken, deleteStoredToken = origLT, origST, origDT
		loadClientSecret, saveClientSecret, deleteClientSecret = origLS, origSS, origDS
		deletePassword = origDP
	})
	return fs
}

// oauthRec is a fake instance speaking both /oauth_token.do and the data
// endpoints the tests exercise.
type oauthRec struct {
	srv        *httptest.Server
	tokenForms []url.Values
	apiAuths   []string
	mintN      int
	tokenFail  bool
	api401     func(auth string) bool
}

func newOAuthInstance(t *testing.T) *oauthRec {
	t.Helper()
	rec := &oauthRec{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth_token.do", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		rec.tokenForms = append(rec.tokenForms, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		if rec.tokenFail {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token expired"}`) //nolint:errcheck
			return
		}
		rec.mintN++
		fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-new","expires_in":1800}`, rec.mintN) //nolint:errcheck
	})
	mux.HandleFunc("/api/now/stats/x", func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		rec.apiAuths = append(rec.apiAuths, a)
		if rec.api401 != nil && rec.api401(a) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":{"stats":{"count":"5"}}}`) //nolint:errcheck
	})
	mux.HandleFunc("/api/now/table/sys_user", func(w http.ResponseWriter, r *http.Request) {
		rec.apiAuths = append(rec.apiAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":[{"user_name":"svc.glm","name":"SVC GLM"}]}`) //nolint:errcheck
	})
	rec.srv = httptest.NewServer(mux)
	t.Cleanup(rec.srv.Close)
	return rec
}

func oauthProfile(t *testing.T, rec *oauthRec, name, method string) {
	t.Helper()
	pointConfigAt(t)
	writeConfig(t, &config.File{Profiles: map[string]config.Profile{
		name: {Instance: rec.srv.URL, Auth: method, ClientID: "cid"},
	}})
}

func execRootIn(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	if stdin != "" {
		root.SetIn(strings.NewReader(stdin))
	}
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestEnvProfileClientCredentials(t *testing.T) {
	// GLM_INSTANCE + GLM_CLIENT_ID + GLM_CLIENT_SECRET → the env profile
	// mints via the client-credentials grant (O8 inference) and stays
	// stateless: nothing is ever persisted for it.
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	pointConfigAt(t)
	t.Setenv(config.EnvInstance, rec.srv.URL)
	t.Setenv(secret.EnvClientID, "env-cid")
	t.Setenv(secret.EnvClientSecret, "env-shh")

	stdout, _, err := execRoot(t, "count", "x")
	if err != nil {
		t.Fatalf("count via env client-credentials: %v", err)
	}
	if !strings.Contains(stdout, "5") {
		t.Errorf("want count output, got %q", stdout)
	}
	if len(rec.tokenForms) != 1 {
		t.Fatalf("want exactly one mint, got %d", len(rec.tokenForms))
	}
	form := rec.tokenForms[0]
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "env-cid" || form.Get("client_secret") != "env-shh" {
		t.Errorf("mint form: %v", form)
	}
	if len(rec.apiAuths) == 0 || rec.apiAuths[0] != "Bearer at-1" {
		t.Errorf("API call must carry the minted token, got %v", rec.apiAuths)
	}
	if fs.saves != 0 {
		t.Errorf("the env profile is stateless — %d token save(s) happened", fs.saves)
	}
}

func TestOAuthProfileUsesStoredToken(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at-stored", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}

	stdout, _, err := execRoot(t, "-p", "p", "count", "x")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if !strings.Contains(stdout, "5") {
		t.Errorf("want count output, got %q", stdout)
	}
	if len(rec.tokenForms) != 0 {
		t.Errorf("a live stored token must not hit the token endpoint: %v", rec.tokenForms)
	}
	if rec.apiAuths[0] != "Bearer at-stored" {
		t.Errorf("want the stored token used, got %v", rec.apiAuths)
	}
}

func TestOAuthProfileRefreshesExpiredToken(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at-dead", RefreshToken: "rt-old", Expiry: time.Now().Add(-time.Minute)}

	_, _, err := execRoot(t, "-p", "p", "count", "x")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rec.tokenForms) != 1 || rec.tokenForms[0].Get("grant_type") != "refresh_token" || rec.tokenForms[0].Get("refresh_token") != "rt-old" {
		t.Fatalf("want one refresh with the stored refresh token, got %v", rec.tokenForms)
	}
	if rec.apiAuths[0] != "Bearer at-1" {
		t.Errorf("want the refreshed token used, got %v", rec.apiAuths)
	}
	if got := fs.tokens["p"]; got == nil || got.AccessToken != "at-1" {
		t.Errorf("refreshed token must be persisted for the next run, got %+v", got)
	}
}

func TestOAuthProfileRenewsOn401(t *testing.T) {
	// A token can be valid by the clock yet rejected by the instance
	// (revocation): the 401 triggers one refresh and one retry through the
	// snow seam, end to end.
	rec := newOAuthInstance(t)
	rec.api401 = func(auth string) bool { return auth == "Bearer at-stale" }
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at-stale", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}

	stdout, _, err := execRoot(t, "-p", "p", "count", "x")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if !strings.Contains(stdout, "5") {
		t.Errorf("want count output, got %q", stdout)
	}
	if want := []string{"Bearer at-stale", "Bearer at-1"}; len(rec.apiAuths) != 2 || rec.apiAuths[0] != want[0] || rec.apiAuths[1] != want[1] {
		t.Errorf("want 401 then renewed retry %v, got %v", want, rec.apiAuths)
	}
}

func TestOAuthProfileNoSessionErrors(t *testing.T) {
	rec := newOAuthInstance(t)
	stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)

	_, _, err := execRoot(t, "-p", "p", "count", "x")
	if err == nil || !strings.Contains(err.Error(), "glm profile login p") {
		t.Fatalf("no session must name the corrective login, got %v", err)
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != exit.Auth {
		t.Errorf("no session maps to exit %d, got %v", exit.Auth, err)
	}
	if len(rec.apiAuths) != 0 {
		t.Errorf("no data request may be sent without a session: %v", rec.apiAuths)
	}
}

func TestOAuthProfileRefreshFailureNamesLogin(t *testing.T) {
	rec := newOAuthInstance(t)
	rec.tokenFail = true
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at-dead", RefreshToken: "rt-dead", Expiry: time.Now().Add(-time.Minute)}

	_, _, err := execRoot(t, "-p", "p", "count", "x")
	if err == nil || !strings.Contains(err.Error(), "glm profile login p") {
		t.Fatalf("a dead refresh token must name the corrective login, got %v", err)
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != exit.Auth {
		t.Errorf("refresh failure maps to exit %d, got %v", exit.Auth, err)
	}
}

func TestOAuthProfileRevokedTokenNamesLogin(t *testing.T) {
	// Codex P2 (PR #25): a clock-valid but instance-revoked token 401s;
	// when the refresh then fails too, the user must see the corrective
	// login command — not the raw 401 the renewal error replaced.
	rec := newOAuthInstance(t)
	rec.api401 = func(string) bool { return true }
	rec.tokenFail = true
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at-revoked", RefreshToken: "rt-dead", Expiry: time.Now().Add(time.Hour)}

	_, _, err := execRoot(t, "-p", "p", "count", "x")
	if err == nil || !strings.Contains(err.Error(), "glm profile login p") {
		t.Fatalf("a failed renewal must surface the corrective login, got %v", err)
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != exit.Auth {
		t.Errorf("want exit %d, got %v", exit.Auth, err)
	}
}

func TestProfileRemoveReportsKeyringFailure(t *testing.T) {
	// Codex P2 (PR #25): the delete helpers treat a missing entry as
	// success, so any error is a real keyring failure — remove must report
	// it instead of printing success while credentials linger.
	rec := newOAuthInstance(t)
	stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	origDT := deleteStoredToken
	deleteStoredToken = func(string) error { return fmt.Errorf("keyring locked") }
	t.Cleanup(func() { deleteStoredToken = origDT })

	_, _, err := execRoot(t, "profile", "remove", "p")
	if err == nil || !strings.Contains(err.Error(), "keyring") {
		t.Fatalf("a real keyring failure must be reported, got %v", err)
	}
	if f, _ := config.Load(); f != nil {
		if _, ok := f.Profiles["p"]; ok {
			t.Error("the config removal itself should still have happened")
		}
	}
}

func TestProfileLoginClientCredentials(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthClientCredentials)
	fs.secrets["p"] = "shh"

	stdout, _, err := execRoot(t, "profile", "login", "p")
	if err != nil {
		t.Fatalf("profile login: %v", err)
	}
	if !strings.Contains(stdout, "signed in as svc.glm") {
		t.Errorf("want resolved identity in output, got %q", stdout)
	}
	if tok := fs.tokens["p"]; tok == nil || tok.AccessToken != "at-1" {
		t.Errorf("minted token must be stored, got %+v", fs.tokens["p"])
	}
	f, err := config.Load()
	if err != nil || f.Profiles["p"].Username != "svc.glm" {
		t.Errorf("login must store the resolved username (O10), got %+v", f.Profiles["p"])
	}
}

func TestProfileLoginOAuth(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	origLogin := runOAuthLogin
	var gotCfg oauth.Config
	runOAuthLogin = func(_ context.Context, cfg oauth.Config) (*oauth.Token, error) {
		gotCfg = cfg
		return &oauth.Token{AccessToken: "at-login", RefreshToken: "rt-login", Expiry: time.Now().Add(30 * time.Minute)}, nil
	}
	t.Cleanup(func() { runOAuthLogin = origLogin })

	stdout, _, err := execRoot(t, "profile", "login", "p")
	if err != nil {
		t.Fatalf("profile login: %v", err)
	}
	if gotCfg.ClientID != "cid" {
		t.Errorf("login must pass the profile's client_id, got %q", gotCfg.ClientID)
	}
	if !strings.Contains(stdout, "signed in as svc.glm") {
		t.Errorf("want resolved identity, got %q", stdout)
	}
	if tok := fs.tokens["p"]; tok == nil || tok.AccessToken != "at-login" {
		t.Errorf("login token must be stored, got %+v", fs.tokens["p"])
	}
	f, _ := config.Load()
	if f.Profiles["p"].Username != "svc.glm" {
		t.Errorf("login must store the resolved username, got %+v", f.Profiles["p"])
	}
}

func TestProfileLoginBasicRejected(t *testing.T) {
	rec := newOAuthInstance(t)
	pointConfigAt(t)
	writeConfig(t, &config.File{Profiles: map[string]config.Profile{
		"b": {Instance: rec.srv.URL, Auth: config.AuthBasic, Username: "u"},
	}})
	_, _, err := execRoot(t, "profile", "login", "b")
	if err == nil || !strings.Contains(err.Error(), "basic auth") {
		t.Fatalf("basic profiles have no login step, got %v", err)
	}
}

func TestProfileLogout(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at", RefreshToken: "rt"}
	fs.secrets["p"] = "shh"

	stdout, _, err := execRoot(t, "profile", "logout", "p")
	if err != nil {
		t.Fatalf("profile logout: %v", err)
	}
	if _, ok := fs.tokens["p"]; ok {
		t.Error("logout must delete the stored tokens")
	}
	if _, ok := fs.secrets["p"]; !ok {
		t.Error("logout must keep the client secret (remove deletes it)")
	}
	if !strings.Contains(stdout, "signed out") {
		t.Errorf("want confirmation, got %q", stdout)
	}
}

func TestProfileRemoveDeletesOAuthState(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	oauthProfile(t, rec, "p", config.AuthOAuth)
	fs.tokens["p"] = &oauth.Token{AccessToken: "at"}
	fs.secrets["p"] = "shh"

	if _, _, err := execRoot(t, "profile", "remove", "p"); err != nil {
		t.Fatalf("profile remove: %v", err)
	}
	if _, ok := fs.tokens["p"]; ok {
		t.Error("remove must delete stored tokens")
	}
	if _, ok := fs.secrets["p"]; ok {
		t.Error("remove must delete the client secret")
	}
}

func TestProfileAddOAuthPublicClient(t *testing.T) {
	// The recommended public PKCE client stores NO secret — profile add
	// must not touch the keyring at all (password or secret).
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	pointConfigAt(t)

	stdout, stderr, err := execRoot(t, "profile", "add", "o", "--instance", rec.srv.URL, "--auth", "oauth", "--client-id", "abc", "--redirect-port", "9999")
	if err != nil {
		t.Fatalf("profile add: %v", err)
	}
	f, _ := config.Load()
	p := f.Profiles["o"]
	if p.Auth != config.AuthOAuth || p.ClientID != "abc" || p.RedirectPort != 9999 {
		t.Errorf("stored profile: %+v", p)
	}
	if len(fs.secrets) != 0 {
		t.Errorf("public client must store no secret, got %v", fs.secrets)
	}
	if !strings.Contains(stdout, "via oauth") {
		t.Errorf("want method in confirmation, got %q", stdout)
	}
	if !strings.Contains(stderr, "glm profile login o") {
		t.Errorf("want the login next-step hint, got %q", stderr)
	}
}

func TestProfileAddClientCredentialsStoresSecret(t *testing.T) {
	rec := newOAuthInstance(t)
	fs := stubStore(t)
	pointConfigAt(t)

	_, _, err := execRootIn(t, "shh-secret\n", "profile", "add", "c", "--instance", rec.srv.URL, "--auth", "client-credentials", "--client-id", "ccid", "--client-secret-stdin")
	if err != nil {
		t.Fatalf("profile add: %v", err)
	}
	f, _ := config.Load()
	if f.Profiles["c"].Auth != config.AuthClientCredentials || f.Profiles["c"].ClientID != "ccid" {
		t.Errorf("stored profile: %+v", f.Profiles["c"])
	}
	if fs.secrets["c"] != "shh-secret" {
		t.Errorf("client secret must be stored via the seam, got %v", fs.secrets)
	}
}

func TestProfileAddOAuthValidation(t *testing.T) {
	rec := newOAuthInstance(t)
	stubStore(t)
	pointConfigAt(t)

	if _, _, err := execRoot(t, "profile", "add", "o", "--instance", rec.srv.URL, "--auth", "oauth"); err == nil || !strings.Contains(err.Error(), "--client-id") {
		t.Errorf("oauth without --client-id must fail, got %v", err)
	}
	if _, _, err := execRoot(t, "profile", "add", "o", "--instance", rec.srv.URL, "--auth", "ntlm"); err == nil || !strings.Contains(err.Error(), "unknown --auth") {
		t.Errorf("unknown auth must fail, got %v", err)
	}
	if _, _, err := execRoot(t, "profile", "add", "b", "--instance", rec.srv.URL); err == nil || !strings.Contains(err.Error(), "--username") {
		t.Errorf("basic without --username must fail, got %v", err)
	}
}

func TestClientCredentialsProfileMissingSecret(t *testing.T) {
	rec := newOAuthInstance(t)
	stubStore(t)
	oauthProfile(t, rec, "c", config.AuthClientCredentials)

	_, _, err := execRoot(t, "-p", "c", "count", "x")
	if err == nil || !strings.Contains(err.Error(), "client secret") {
		t.Fatalf("missing secret must name the remedy, got %v", err)
	}
}
