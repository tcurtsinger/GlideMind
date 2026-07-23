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

// pointConfigAt redirects the config dir to a per-test temp dir (shared with
// any later runGlm calls via the owner marker) and clears ambient GLM_*
// selection vars so tests control resolution completely.
func pointConfigAt(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(config.EnvProfile, "")
	t.Setenv(config.EnvInstance, "")
	t.Setenv(config.EnvUsername, "")
}

func writeConfig(t *testing.T, f *config.File) {
	t.Helper()
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func fakeUserServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/now/table/sys_user":
			w.Write([]byte(`{"result":[{"user_name":"a","name":"A","email":"a@example.com","title":""}]}`)) //nolint:errcheck
		case "/api/now/table/sys_user_has_role":
			w.Write([]byte(`{"result":[]}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInstanceStampWithMultipleProfiles pins DESIGN-INSTANCES.md I3: with 2+
// profiles configured every command stamps the instance it ran against on
// stderr, so a transcript proves where each answer came from.
func TestInstanceStampWithMultipleProfiles(t *testing.T) {
	pointConfigAt(t)
	srv := fakeUserServer(t)
	writeConfig(t, &config.File{
		Profiles: map[string]config.Profile{
			"dev":       {Instance: srv.URL, Auth: "basic", Username: "a"},
			"smartwork": {Instance: "https://sw.service-now.com", Auth: "basic", Username: "b"},
		},
	})
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"whoami", "-p", "dev"})
	if err := root.Execute(); err != nil {
		t.Fatalf("whoami: %v (stderr: %s)", err, errOut.String())
	}

	if !strings.Contains(errOut.String(), "instance: dev (") {
		t.Errorf("stderr missing instance stamp:\n%s", errOut.String())
	}
	// Flag selection is visible in the command line; no source annotation.
	if strings.Contains(errOut.String(), config.SourceFlag) {
		t.Errorf("flag-selected stamp should not name a source:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "instance:") {
		t.Errorf("stamp must go to stderr, not stdout:\n%s", out.String())
	}
}

// TestInstanceStampNamesInvisibleSource pins the second half of I3: when
// selection came from somewhere not visible in the command line (config
// default, env), the stamp names the source.
func TestInstanceStampNamesInvisibleSource(t *testing.T) {
	pointConfigAt(t)
	srv := fakeUserServer(t)
	writeConfig(t, &config.File{
		Default: "dev",
		Profiles: map[string]config.Profile{
			"dev":       {Instance: srv.URL, Auth: "basic", Username: "a"},
			"smartwork": {Instance: "https://sw.service-now.com", Auth: "basic", Username: "b"},
		},
	})
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"whoami"})
	if err := root.Execute(); err != nil {
		t.Fatalf("whoami: %v (stderr: %s)", err, errOut.String())
	}

	if !strings.Contains(errOut.String(), "[config default]") {
		t.Errorf("default-selected stamp should name its source:\n%s", errOut.String())
	}
}

// TestNoStampWithSingleProfile: one profile means no ambiguity — the stamp
// would be tokens spent on a confusion that cannot occur.
func TestNoStampWithSingleProfile(t *testing.T) {
	pointConfigAt(t)
	srv := fakeUserServer(t)
	writeConfig(t, &config.File{
		Profiles: map[string]config.Profile{
			"dev": {Instance: srv.URL, Auth: "basic", Username: "a"},
		},
	})
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"whoami"})
	if err := root.Execute(); err != nil {
		t.Fatalf("whoami: %v (stderr: %s)", err, errOut.String())
	}
	if strings.Contains(errOut.String(), "instance:") {
		t.Errorf("single profile should not stamp:\n%s", errOut.String())
	}
}

// TestPrimeListsProfiles pins DESIGN-INSTANCES.md I4: prime opens with the
// configured instances so an agent starts the session instance-aware.
func TestPrimeListsProfiles(t *testing.T) {
	pointConfigAt(t)
	writeConfig(t, &config.File{
		Profiles: map[string]config.Profile{
			"dev":       {Instance: "https://dev.service-now.com", Auth: "basic", Username: "a"},
			"smartwork": {Instance: "https://sw.service-now.com", Auth: "basic", Username: "b"},
		},
	})

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"prime"})
	if err := root.Execute(); err != nil {
		t.Fatalf("prime: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Profiles: dev (dev.service-now.com), smartwork (sw.service-now.com)",
		"pass -p <name>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prime missing %q:\n%s", want, got)
		}
	}
}

// TestClearLegacyDefault pins the upgrade migration: configs written when
// `profile add` auto-defaulted the first profile carry a default that was
// never a deliberate choice. Adding the second profile must clear it, or the
// old default keeps silently selecting the first instance — the exact leak
// I1 closes. Defaults in an already-multi world are deliberate and stay.
func TestClearLegacyDefault(t *testing.T) {
	two := func() *config.File {
		return &config.File{
			Default: "dev",
			Profiles: map[string]config.Profile{
				"dev":       {Instance: "https://dev.service-now.com"},
				"smartwork": {Instance: "https://sw.service-now.com"},
			},
		}
	}

	// The 1→2 transition (smartwork newly added): legacy default clears.
	f := two()
	if got := clearLegacyDefault(f, false); got != "dev" || f.Default != "" {
		t.Errorf("1→2 add should clear legacy default: cleared=%q default=%q", got, f.Default)
	}

	// Re-adding (updating) one of two existing profiles: not a transition.
	f = two()
	if got := clearLegacyDefault(f, true); got != "" || f.Default != "dev" {
		t.Errorf("update must not clear default: cleared=%q default=%q", got, f.Default)
	}

	// Adding a third profile: the default was chosen in a multi-profile
	// world, so it is deliberate and stays.
	f = two()
	f.Profiles["qa"] = config.Profile{Instance: "https://qa.service-now.com"}
	if got := clearLegacyDefault(f, false); got != "" || f.Default != "dev" {
		t.Errorf("2→3 add must not clear default: cleared=%q default=%q", got, f.Default)
	}
}

// TestInstanceStampEnvSelection: a GLM_INSTANCE-selected instance next to a
// configured profile is exactly the invisible-selection case — the stamp
// must fire and name the env source.
func TestInstanceStampEnvSelection(t *testing.T) {
	pointConfigAt(t)
	srv := fakeUserServer(t)
	writeConfig(t, &config.File{
		Profiles: map[string]config.Profile{
			"dev": {Instance: "https://dev.service-now.com", Auth: "basic", Username: "a"},
		},
	})
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(config.EnvUsername, "a")
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"whoami"})
	if err := root.Execute(); err != nil {
		t.Fatalf("whoami: %v (stderr: %s)", err, errOut.String())
	}

	if !strings.Contains(errOut.String(), "instance: env (") || !strings.Contains(errOut.String(), "[GLM_INSTANCE env]") {
		t.Errorf("env-selected stamp missing or unannotated:\n%s", errOut.String())
	}
}

// TestProfileUseClear: --clear removes the default so -p becomes required
// again (the reversal path for the I2 opt-out).
func TestProfileUseClear(t *testing.T) {
	pointConfigAt(t)
	writeConfig(t, &config.File{
		Default: "dev",
		Profiles: map[string]config.Profile{
			"dev": {Instance: "https://dev.service-now.com", Auth: "basic", Username: "a"},
		},
	})

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"profile", "use", "--clear"})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile use --clear: %v", err)
	}

	f, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Default != "" {
		t.Errorf("default not cleared: %q", f.Default)
	}
}
