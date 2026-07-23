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

// TestWhoamiEndToEnd drives the real command tree against a fake instance,
// configured purely through GLM_* env vars (the container/CI path).
func TestWhoamiEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/now/table/sys_user":
			w.Write([]byte(`{"result":[{"user_name":"svc.glm","name":"Service GLM","email":"svc@example.com","title":""}]}`)) //nolint:errcheck
		case "/api/now/table/sys_user_has_role":
			w.Write([]byte(`{"result":[{"role.name":"admin"},{"role.name":"itil"},{"role.name":"admin"}]}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Isolate the config dir: with GLM_INSTANCE selection, any real profiles
	// on the developer's machine would flip the instance stamp on.
	pointConfigAt(t)
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(config.EnvUsername, "svc.glm")
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"whoami"})
	if err := root.Execute(); err != nil {
		t.Fatalf("whoami: %v (stderr: %s)", err, errOut.String())
	}

	got := out.String()
	for _, want := range []string{
		"user      svc.glm (Service GLM)",
		"email     svc@example.com",
		"roles     admin, itil (2)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "title") {
		t.Errorf("empty title should be omitted:\n%s", got)
	}
}
