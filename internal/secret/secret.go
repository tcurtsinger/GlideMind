// Package secret stores per-profile credentials in the OS keyring (Windows
// Credential Manager, macOS Keychain, Secret Service). GLM_PASSWORD overrides
// the keyring so headless environments never touch it. Secrets never live in
// the config file.
package secret

import (
	"errors"
	"fmt"
	"os"

	"github.com/zalando/go-keyring"
)

const (
	service = "glidemind"

	// EnvPassword overrides the keyring lookup for every profile.
	EnvPassword = "GLM_PASSWORD"

	// EnvToken supplies a static bearer token (DESIGN-OAUTH.md O8). Set, it
	// overrides the keyring AND EnvPassword for any profile — the profile
	// picks the instance, env may supply the credential, and a token is the
	// more specific claim. Headless/CI use; never renewed: when it expires
	// the 401 surfaces (exit 2) and the environment re-mints.
	EnvToken = "GLM_TOKEN"
)

// Token returns the GLM_TOKEN bearer credential, or "" when unset.
func Token() string { return os.Getenv(EnvToken) }

// Get returns the credential for a profile: GLM_PASSWORD if set, else the
// OS keyring entry.
func Get(profile string) (string, error) {
	if v := os.Getenv(EnvPassword); v != "" {
		return v, nil
	}
	v, err := keyring.Get(service, profile)
	if err != nil {
		return "", fmt.Errorf("no credential for profile %q — store one with `glm profile add` or set %s: %w", profile, EnvPassword, err)
	}
	return v, nil
}

// GetStored returns only the OS keyring credential, ignoring GLM_PASSWORD.
// Use it where the true persisted state matters — e.g. transactional
// rollback — rather than the effective credential Get resolves.
func GetStored(profile string) (string, bool) {
	v, err := keyring.Get(service, profile)
	if err != nil {
		return "", false
	}
	return v, true
}

// Set stores the credential for a profile in the OS keyring.
func Set(profile, value string) error {
	if err := keyring.Set(service, profile, value); err != nil {
		return fmt.Errorf("store credential for profile %q: %w", profile, err)
	}
	return nil
}

// Delete removes a profile's credential; a missing entry is not an error.
func Delete(profile string) error {
	err := keyring.Delete(service, profile)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete credential for profile %q: %w", profile, err)
	}
	return nil
}
