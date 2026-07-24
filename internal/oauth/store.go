package oauth

import (
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

// Token persistence (O5): one OS-keyring entry per profile, distinct from
// the basic-auth password entry, holding the JSON-encoded Token; a client
// secret (O9/O13) gets its own entry. Lookups go through secret.GetStored —
// the GLM_PASSWORD env override must never masquerade as a token. Keys
// derive from the profile name with a suffix; a profile literally named
// "x:oauth" would collide with profile "x"'s token entry — accepted, since
// profile names are user-chosen and the derivation is recorded here.

func tokenKey(profile string) string  { return profile + ":oauth" }
func secretKey(profile string) string { return profile + ":secret" }

// SaveToken stores a profile's tokens in the keyring.
func SaveToken(profile string, t *Token) error {
	s, err := t.encode()
	if err != nil {
		return err
	}
	return secret.Set(tokenKey(profile), s)
}

// LoadToken returns a profile's stored tokens. A missing or corrupt entry
// reads as absent — the remedy for both is the same login.
func LoadToken(profile string) (*Token, bool) {
	s, ok := secret.GetStored(tokenKey(profile))
	if !ok {
		return nil, false
	}
	t, err := decodeToken(s)
	if err != nil {
		return nil, false
	}
	return t, true
}

// DeleteToken removes a profile's stored tokens (profile logout/remove).
// A missing entry is not an error.
func DeleteToken(profile string) error {
	return secret.Delete(tokenKey(profile))
}

// SaveClientSecret stores a confidential client's secret in the keyring.
func SaveClientSecret(profile, value string) error {
	return secret.Set(secretKey(profile), value)
}

// LoadClientSecret returns a profile's stored client secret, if any.
func LoadClientSecret(profile string) (string, bool) {
	return secret.GetStored(secretKey(profile))
}

// DeleteClientSecret removes a profile's client secret (profile remove).
func DeleteClientSecret(profile string) error {
	return secret.Delete(secretKey(profile))
}
