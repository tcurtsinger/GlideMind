package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/oauth"
	"github.com/tcurtsinger/GlideMind/internal/secret"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

// Store and flow seams, injectable so cli tests never touch the real OS
// keyring or a real browser (the standing harness rule).
var (
	loadStoredToken    = oauth.LoadToken
	saveStoredToken    = oauth.SaveToken
	deleteStoredToken  = oauth.DeleteToken
	loadClientSecret   = oauth.LoadClientSecret
	saveClientSecret   = oauth.SaveClientSecret
	deleteClientSecret = oauth.DeleteClientSecret
	deletePassword     = secret.Delete
	runOAuthLogin      = oauth.Login
)

// oauthConfigFor assembles the oauth.Config for a resolved profile:
// endpoints derived from the instance, client_id from config (env profile:
// GLM_CLIENT_ID), and the client secret resolved GLM_CLIENT_SECRET-first —
// the same env-overrides-keyring rule GLM_PASSWORD established.
func oauthConfigFor(res *config.Resolved) (oauth.Config, error) {
	base, err := snow.NormalizeInstance(res.Profile.Instance)
	if err != nil {
		return oauth.Config{}, err
	}
	cfg := oauth.Config{
		Endpoints:    oauth.InstanceEndpoints(base.String()),
		ClientID:     res.Profile.ClientID,
		RedirectPort: res.Profile.RedirectPort,
	}
	if cfg.ClientID == "" && res.Name == config.EnvProfileName {
		cfg.ClientID = secret.ClientID()
	}
	if s := secret.ClientSecret(); s != "" {
		cfg.ClientSecret = s
	} else if s, ok := loadClientSecret(res.Name); ok {
		cfg.ClientSecret = s
	}
	return cfg, nil
}

// tokenSource adapts the oauth grants to snow.TokenSource: keyring-cached
// access token, proactive renewal when invalid, single-flight within the
// process. The synthetic env profile never persists (persist=false) — it is
// stateless by design and CI keyrings are often absent.
type tokenSource struct {
	profile string
	persist bool
	renewFn func(ctx context.Context, cur *oauth.Token) (*oauth.Token, error)

	mu  sync.Mutex
	tok *oauth.Token
}

func (s *tokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok == nil && s.persist {
		if t, ok := loadStoredToken(s.profile); ok {
			s.tok = t
		}
	}
	if s.tok.Valid(time.Now()) {
		return s.tok.AccessToken, nil
	}
	return s.renewLocked(ctx)
}

// Renew's error replaces the raw 401 for the caller (snow renewOn401), so
// the corrective message — "run: glm profile login <name>", a rejected
// client secret — survives to the user instead of a bare HTTP 401.
func (s *tokenSource) Renew(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.renewLocked(ctx)
	return err
}

func (s *tokenSource) renewLocked(ctx context.Context) (string, error) {
	t, err := s.renewFn(ctx, s.tok)
	if err != nil {
		return "", err
	}
	s.tok = t
	if s.persist {
		// Best effort: a failed save costs one renewal next run, and the
		// command in flight already has its token.
		_ = saveStoredToken(s.profile, t)
	}
	return t.AccessToken, nil
}

// newOAuthSource renews via the refresh token (O6). A missing session or a
// failed refresh is exit 2 naming the corrective login command (O3/O12) —
// data commands never go interactive.
func newOAuthSource(res *config.Resolved, cfg oauth.Config) *tokenSource {
	name := res.Name
	return &tokenSource{
		profile: name,
		persist: name != config.EnvProfileName,
		renewFn: func(ctx context.Context, cur *oauth.Token) (*oauth.Token, error) {
			refresh := ""
			if cur != nil {
				refresh = cur.RefreshToken
			}
			if refresh == "" {
				return nil, &oauth.AuthError{
					Msg:    fmt.Sprintf("profile %q has no OAuth session", name),
					Detail: fmt.Sprintf("run: glm profile login %s", name),
				}
			}
			t, err := oauth.Refresh(ctx, cfg, refresh)
			if err != nil {
				return nil, fmt.Errorf("profile %q: OAuth session could not be renewed (%w) — run: glm profile login %s", name, err, name)
			}
			return t, nil
		},
	}
}

// newCCSource renews by minting from the client secret (O13) — fully
// self-healing while the secret is valid.
func newCCSource(res *config.Resolved, cfg oauth.Config) *tokenSource {
	return &tokenSource{
		profile: res.Name,
		persist: res.Name != config.EnvProfileName,
		renewFn: func(ctx context.Context, _ *oauth.Token) (*oauth.Token, error) {
			t, err := oauth.ClientCredentials(ctx, cfg)
			if err != nil {
				return nil, fmt.Errorf("profile %q: client-credentials mint failed (%w)", res.Name, err)
			}
			return t, nil
		},
	}
}
