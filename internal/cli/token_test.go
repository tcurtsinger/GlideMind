package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

// countServer serves the stats count endpoint and records the Authorization
// header of every request.
func countServer(t *testing.T, auths *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auths = append(*auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"stats":{"count":"5"}}}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

func execRoot(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestGlmTokenBearerEnvProfile(t *testing.T) {
	// GLM_INSTANCE + GLM_TOKEN alone must work: no username, no password —
	// the pure-env CI shape DESIGN-OAUTH.md O8 promises.
	var auths []string
	srv := countServer(t, &auths)
	pointConfigAt(t)
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(secret.EnvToken, "tok-abc")

	stdout, _, err := execRoot(t, "count", "x")
	if err != nil {
		t.Fatalf("count with bearer env profile: %v", err)
	}
	if !strings.Contains(stdout, "5") {
		t.Errorf("want the count in output, got %q", stdout)
	}
	if len(auths) == 0 || auths[0] != "Bearer tok-abc" {
		t.Errorf("want Authorization %q, got %v", "Bearer tok-abc", auths)
	}
}

func TestGlmTokenBeatsPassword(t *testing.T) {
	// With both credentials in the env, the token wins for a named basic
	// profile — the profile picks the instance, env supplies the credential,
	// and a token is the more specific claim (O8, Resolution 2).
	var auths []string
	srv := countServer(t, &auths)
	pointConfigAt(t)
	writeConfig(t, &config.File{Profiles: map[string]config.Profile{
		"p": {Instance: srv.URL, Auth: "basic", Username: "u"},
	}})
	t.Setenv(secret.EnvPassword, "pw")
	t.Setenv(secret.EnvToken, "tok-2")

	if _, _, err := execRoot(t, "-p", "p", "count", "x"); err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(auths) == 0 || auths[0] != "Bearer tok-2" {
		t.Errorf("GLM_TOKEN must beat GLM_PASSWORD, got Authorization %v", auths)
	}
}

func TestWhoamiBearerResolvesTokenIdentity(t *testing.T) {
	// A bearer credential may not know its own username: whoami must ask the
	// instance who the token is (DESIGN-OAUTH.md O10), then use the answer
	// for the roles query.
	var userQuery, roleQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/now/table/sys_user":
			userQuery = r.URL.Query().Get("sysparm_query")
			w.Write([]byte(`{"result":[{"user_name":"a","name":"A","email":"","title":""}]}`)) //nolint:errcheck
		case "/api/now/table/sys_user_has_role":
			roleQuery = r.URL.Query().Get("sysparm_query")
			w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	pointConfigAt(t)
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(secret.EnvToken, "tok-3")

	stdout, _, err := execRoot(t, "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if userQuery != "sys_id=javascript:gs.getUserID()" {
		t.Errorf("whoami must resolve the token's identity via gs.getUserID(), queried %q", userQuery)
	}
	if !strings.Contains(roleQuery, "user.user_name=a") {
		t.Errorf("roles must use the resolved username, queried %q", roleQuery)
	}
	if !strings.Contains(stdout, "user      a (A)") {
		t.Errorf("want the resolved user in output, got:\n%s", stdout)
	}
}

func TestWhoamiBearerOverridesStoredUsername(t *testing.T) {
	// Codex P2 (PR #23): GLM_TOKEN over a NAMED profile with a stored
	// username must still resolve identity from the instance — the token
	// may be minted for a different account, and whoami falsely confirming
	// the stored name defeats the sanity check it exists for.
	var userQuery, roleQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/now/table/sys_user":
			userQuery = r.URL.Query().Get("sysparm_query")
			w.Write([]byte(`{"result":[{"user_name":"actual","name":"Actual User","email":"","title":""}]}`)) //nolint:errcheck
		case "/api/now/table/sys_user_has_role":
			roleQuery = r.URL.Query().Get("sysparm_query")
			w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	pointConfigAt(t)
	writeConfig(t, &config.File{Profiles: map[string]config.Profile{
		"p": {Instance: srv.URL, Auth: "basic", Username: "stored"},
	}})
	t.Setenv(secret.EnvToken, "tok-5")

	stdout, _, err := execRoot(t, "-p", "p", "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if userQuery != "sys_id=javascript:gs.getUserID()" {
		t.Errorf("token auth must resolve identity from the instance even with a stored username, queried %q", userQuery)
	}
	if !strings.Contains(roleQuery, "user.user_name=actual") {
		t.Errorf("roles must use the token's resolved user, queried %q", roleQuery)
	}
	if !strings.Contains(stdout, "user      actual (Actual User)") || strings.Contains(stdout, "stored") {
		t.Errorf("whoami must report the token's user, never the stored one, got:\n%s", stdout)
	}
}

func TestProfileTestBearerResolvesIdentity(t *testing.T) {
	var userQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		userQuery = r.URL.Query().Get("sysparm_query")
		w.Write([]byte(`{"result":[{"sys_id":"abc","user_name":"actual"}]}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	pointConfigAt(t)
	writeConfig(t, &config.File{Profiles: map[string]config.Profile{
		"p": {Instance: srv.URL, Auth: "basic", Username: "stored"},
	}})
	t.Setenv(secret.EnvToken, "tok-6")

	stdout, _, err := execRoot(t, "profile", "test", "p")
	if err != nil {
		t.Fatalf("profile test: %v", err)
	}
	if userQuery != "sys_id=javascript:gs.getUserID()" {
		t.Errorf("token auth must resolve identity from the instance, queried %q", userQuery)
	}
	if !strings.Contains(stdout, "as actual") || strings.Contains(stdout, "stored") {
		t.Errorf("profile test must report the token's user, never the stored one, got %q", stdout)
	}
}

func TestIdentityLineAndAuditUserUnderToken(t *testing.T) {
	// W7/W6: under GLM_TOKEN the stored username is not who a write runs
	// as — neither the preview nor the audit trail may claim it.
	res := &config.Resolved{Name: "p", Profile: config.Profile{
		Instance: "https://x.service-now.com", Username: "stored",
	}}
	t.Setenv(secret.EnvToken, "")
	if got := identityLine(res); !strings.Contains(got, "stored") {
		t.Errorf("basic identity line should name the stored username, got %q", got)
	}
	if got := auditUser(res); got != "stored" {
		t.Errorf("basic audit user = %q, want %q", got, "stored")
	}
	t.Setenv(secret.EnvToken, "tok")
	if got := identityLine(res); strings.Contains(got, "stored") || !strings.Contains(got, "GLM_TOKEN") {
		t.Errorf("token identity line must not claim the stored username, got %q", got)
	}
	if got := auditUser(res); strings.Contains(got, "stored") || !strings.Contains(got, "GLM_TOKEN") {
		t.Errorf("token audit user must not claim the stored username, got %q", got)
	}
}

func TestGlmTokenEnvProfileStaysReadOnly(t *testing.T) {
	// W1 is untouched by the credential: the synthetic env profile is
	// read-only no matter how it authenticates.
	var auths []string
	srv := countServer(t, &auths)
	pointConfigAt(t)
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(secret.EnvToken, "tok-4")

	_, _, err := execRoot(t, "update", "task", "TASK0001", "-f", "state=2", "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("env profile write must be refused regardless of bearer auth, got %v", err)
	}
	if len(auths) != 0 {
		t.Errorf("the write gate fires before any request, but the instance saw %v", auths)
	}
}
