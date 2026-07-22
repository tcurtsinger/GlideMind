package secret

import "testing"

func TestEnvPasswordOverridesKeyring(t *testing.T) {
	t.Setenv(EnvPassword, "from-env")
	got, err := Get("any-profile")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "from-env" {
		t.Fatalf("got %q, want env override", got)
	}
}
