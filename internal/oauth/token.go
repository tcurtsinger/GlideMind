// Package oauth implements glm's OAuth flows against a ServiceNow instance
// (DESIGN-OAUTH.md): the interactive Authorization Code + PKCE login (O1),
// refresh and client-credentials renewal (O6/O13), and the keyring-backed
// token store (O5). The package is deliberately CLI-free: endpoints, the
// browser opener, notification, and timeouts are all injectable, so every
// flow runs headless under go test against httptest servers.
package oauth

import (
	"encoding/json"
	"fmt"
	"time"
)

// expirySkew renews slightly early so a token never dies mid-request.
const expirySkew = 60 * time.Second

// Token is one profile's OAuth state — small opaque strings, persisted as
// one JSON blob in the OS keyring (O5). RefreshToken is empty for
// client-credentials tokens (that grant has none; the secret is the
// long-lived credential).
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Valid reports whether the access token is still usable at now, renewing
// expirySkew early. An unknown expiry (zero) counts as valid: the 401
// renewal path is the backstop, and discarding a possibly-live token would
// force a renewal on every command.
func (t *Token) Valid(now time.Time) bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	if t.Expiry.IsZero() {
		return true
	}
	return now.Before(t.Expiry.Add(-expirySkew))
}

func (t *Token) encode() (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	return string(b), nil
}

func decodeToken(s string) (*Token, error) {
	var t Token
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, fmt.Errorf("decode stored token: %w", err)
	}
	return &t, nil
}
