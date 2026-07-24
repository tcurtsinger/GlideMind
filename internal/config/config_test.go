package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pointConfigAt redirects os.UserConfigDir to a temp dir on every platform
// CI runs (APPDATA for Windows, XDG_CONFIG_HOME for Linux).
func pointConfigAt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Ensure ambient GLM_* vars never leak into tests.
	t.Setenv(EnvProfile, "")
	t.Setenv(EnvInstance, "")
	t.Setenv(EnvUsername, "")
	return dir
}

func write(t *testing.T, f *File) {
	t.Helper()
	if err := f.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	pointConfigAt(t)
	f, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(f.Profiles) != 0 || f.Default != "" {
		t.Fatalf("expected empty config, got %+v", f)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := pointConfigAt(t)
	write(t, &File{
		Default: "dev",
		Profiles: map[string]Profile{
			"dev": {Instance: "https://dev.service-now.com", Auth: "basic", Username: "admin"},
		},
	})

	p, err := Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if want := filepath.Join(dir, "glidemind", "config.toml"); p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	f, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := f.Profiles["dev"]
	if f.Default != "dev" || got.Instance != "https://dev.service-now.com" || got.Username != "admin" {
		t.Fatalf("round trip mismatch: %+v", f)
	}
}

func TestResolvePrecedence(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Default: "dev",
		Profiles: map[string]Profile{
			"dev": {Instance: "https://dev.service-now.com", Username: "a"},
			"qa":  {Instance: "https://qa.service-now.com", Username: "b"},
		},
	})

	// Flag beats everything.
	t.Setenv(EnvProfile, "dev")
	r, err := Resolve("qa")
	if err != nil || r.Name != "qa" {
		t.Fatalf("flag should win: %+v, %v", r, err)
	}

	// GLM_PROFILE beats config default.
	r, err = Resolve("")
	if err != nil || r.Name != "dev" || r.Source != EnvProfile+" env" {
		t.Fatalf("env profile should win: %+v, %v", r, err)
	}

	// Config default when nothing else is set.
	t.Setenv(EnvProfile, "")
	r, err = Resolve("")
	if err != nil || r.Name != "dev" || r.Source != "config default" {
		t.Fatalf("config default expected: %+v, %v", r, err)
	}

	// Unknown name errors and names the alternatives.
	if _, err := Resolve("nope"); err == nil {
		t.Fatal("unknown profile should error")
	}
}

func TestResolveEnvInstanceProfile(t *testing.T) {
	pointConfigAt(t)
	t.Setenv(EnvInstance, "acme")
	t.Setenv(EnvUsername, "svc.glm")

	r, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Name != EnvProfileName || r.Profile.Instance != "acme" || r.Profile.Username != "svc.glm" {
		t.Fatalf("env profile mismatch: %+v", r)
	}
	// Pure env world (CI container, no config): no other candidate, no stamp.
	if r.Multi {
		t.Fatal("Multi should be false with no configured profiles")
	}
}

// TestResolveEnvInstanceMultiWithProfiles: an env-selected instance next to
// any configured profile means another instance could have been meant — the
// stamp must fire, so Multi is true even though the env path never consults
// the profile list for selection.
func TestResolveEnvInstanceMultiWithProfiles(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Profiles: map[string]Profile{
			"dev": {Instance: "https://dev.service-now.com", Username: "a"},
		},
	})
	t.Setenv(EnvInstance, "acme")

	r, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Name != EnvProfileName || !r.Multi {
		t.Fatalf("env selection alongside a configured profile should set Multi: %+v", r)
	}
}

func TestResolveEnvUsernameOverridesNamedProfile(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Default: "dev",
		Profiles: map[string]Profile{
			"dev": {Instance: "https://dev.service-now.com", Username: "file-user"},
		},
	})
	t.Setenv(EnvUsername, "env-user")

	r, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Profile.Username != "env-user" {
		t.Fatalf("username = %q, want env override", r.Profile.Username)
	}
}

func TestResolveSingleProfileFallback(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Profiles: map[string]Profile{
			"only": {Instance: "https://only.service-now.com", Username: "a"},
		},
	})

	r, err := Resolve("")
	if err != nil || r.Name != "only" {
		t.Fatalf("single profile fallback: %+v, %v", r, err)
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	pointConfigAt(t)
	if _, err := Resolve(""); err == nil {
		t.Fatal("expected error with no profiles at all")
	}
}

// TestResolveMultiProfileNoDefaultRefusesToGuess pins DESIGN-INSTANCES.md I1:
// with several profiles and no explicit selection, glm must not silently pick
// one — the error lists the candidates so a caller self-heals in one turn.
func TestResolveMultiProfileNoDefaultRefusesToGuess(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Profiles: map[string]Profile{
			"dev":       {Instance: "https://dev.service-now.com", Username: "a"},
			"smartwork": {Instance: "https://sw.service-now.com", Username: "b"},
		},
	})

	_, err := Resolve("")
	if err == nil {
		t.Fatal("expected refusal with 2 profiles and no default")
	}
	for _, want := range []string{"dev", "smartwork", "-p <name>", "glm profile use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestResolvedMulti pins the stamp trigger: Multi reflects whether 2+
// profiles are configured, regardless of how the profile was selected.
func TestResolvedMulti(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{
		Profiles: map[string]Profile{
			"dev": {Instance: "https://dev.service-now.com", Username: "a"},
			"qa":  {Instance: "https://qa.service-now.com", Username: "b"},
		},
	})

	r, err := Resolve("qa")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !r.Multi {
		t.Fatal("Multi should be true with 2 profiles")
	}

	write(t, &File{
		Profiles: map[string]Profile{
			"only": {Instance: "https://only.service-now.com", Username: "a"},
		},
	})
	r, err = Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Multi {
		t.Fatal("Multi should be false with 1 profile")
	}
}

// TestResolveEnvUsernameOnlyForBasic: GLM_USERNAME supplies who a PASSWORD
// belongs to; a token-auth profile's identity is the token's — resolved and
// stored by `glm profile login` — so an env claim must not override it
// (Codex review, PR #25).
func TestResolveEnvUsernameOnlyForBasic(t *testing.T) {
	pointConfigAt(t)
	write(t, &File{Profiles: map[string]Profile{
		"b": {Instance: "https://x.service-now.com", Auth: AuthBasic, Username: "stored"},
		"o": {Instance: "https://x.service-now.com", Auth: AuthOAuth, Username: "resolved"},
		"c": {Instance: "https://x.service-now.com", Auth: AuthClientCredentials, Username: "resolved"},
	}})
	t.Setenv(EnvUsername, "override")

	r, err := Resolve("b")
	if err != nil || r.Profile.Username != "override" {
		t.Errorf("basic profile must honor GLM_USERNAME, got %+v (%v)", r, err)
	}
	for _, name := range []string{"o", "c"} {
		r, err := Resolve(name)
		if err != nil || r.Profile.Username != "resolved" {
			t.Errorf("token-auth profile %q must keep the resolved identity, got %+v (%v)", name, r, err)
		}
	}
}
