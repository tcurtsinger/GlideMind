package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/schema"
	"github.com/tcurtsinger/GlideMind/internal/secret"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

// resolveProfile picks the active profile (flagName overrides the --profile
// flag when non-empty), stamps the instance, and validates the auth method —
// everything that can be decided WITHOUT credentials. Policy checks that must
// fire even when no credential exists (the W1 write gate) run between this
// and clientForResolved.
func resolveProfile(cmd *cobra.Command, flagName string) (*config.Resolved, error) {
	name := flagName
	if name == "" {
		name, _ = cmd.Flags().GetString("profile")
	}
	res, err := config.Resolve(name)
	if err != nil {
		return nil, err
	}
	// With several profiles configured, stamp which instance this command
	// runs against (DESIGN-INSTANCES.md I3): stderr keeps pipes clean, and
	// the transcript proves where every answer came from. Selection sources
	// other than the -p flag are invisible state, so they are named too.
	if res.Multi {
		stamp := fmt.Sprintf("instance: %s (%s)", res.Name, strings.TrimPrefix(res.Profile.Instance, "https://"))
		if res.Source != config.SourceFlag {
			stamp += " [" + res.Source + "]"
		}
		fmt.Fprintln(cmd.ErrOrStderr(), output.SanitizeLine(stamp))
	}
	// The auth-method check exists to fail fast when glm cannot build a
	// credential for the stored method. A GLM_TOKEN bearer displaces the
	// profile's method entirely (Resolution 2: token overrides ANY profile),
	// so an unknown/future method stays usable under a token.
	switch res.Profile.Auth {
	case "", config.AuthBasic, config.AuthOAuth, config.AuthClientCredentials:
	default:
		if secret.Token() == "" {
			return nil, fmt.Errorf("profile %q: auth method %q is not supported (supported: basic, oauth, client_credentials — or a GLM_TOKEN bearer)", res.Name, res.Profile.Auth)
		}
	}
	return res, nil
}

// requireWritable is gate 1 of the two-gate write model (DESIGN-WRITES.md
// W1): the profile itself must be write-enabled — a stored, deliberate
// property. Every write command calls this straight after resolveProfile,
// before ANYTHING with side effects or blocking potential: a profile that
// could never write must get the one-line refusal naming the fix, not a
// credential-lookup error from a keyring it has no entry in, not a schema
// fetch, and not a hang consuming stdin it would never send.
func requireWritable(res *config.Resolved) error {
	if res.Profile.Writable {
		return nil
	}
	if res.Name == config.EnvProfileName {
		// The env profile is not stored, so write-enable can never apply
		// to it — point at the real remedy.
		return fmt.Errorf("the %s env profile is always read-only — writes need a named, write-enabled profile: `glm profile add <name> --instance <url> --username <user> --writable`", config.EnvInstance)
	}
	return fmt.Errorf("profile %q is read-only — enable writes with `glm profile write-enable %s` (each write still needs --yes)", res.Name, res.Name)
}

// identityLine renders the acting identity for a write preview (W7): who the
// write runs as, where, and through which profile — a write must never land
// under an unexpected identity or instance. Under GLM_TOKEN the stored
// username is NOT who the write runs as — the token's identity is — and W7
// must never name the wrong account, so the line says what it truly knows
// and points at the check.
func identityLine(res *config.Resolved) string {
	who := res.Profile.Username
	if secret.Token() != "" {
		who = "the GLM_TOKEN bearer (verify: glm whoami)"
	}
	return output.SanitizeLine(fmt.Sprintf("as %s @ %s (profile %s)", who, strings.TrimPrefix(res.Profile.Instance, "https://"), res.Name))
}

// bearerIdentity names the identity a GLM_TOKEN client runs as, which keys
// the ACL-filtered per-user schema cache (Client.Username). The stored
// profile username is NEVER used here: the token may be a different account,
// and a bearer run must not reuse or overwrite the stored user's cache. An
// explicit GLM_USERNAME is an authoritative claim about the credential in
// use; otherwise a short digest of the token keys the cache — distinct per
// credential, stable for its lifetime.
func bearerIdentity(token string) string {
	if u := os.Getenv(config.EnvUsername); u != "" {
		return u
	}
	sum := sha256.Sum256([]byte(token))
	return "token-" + hex.EncodeToString(sum[:6])
}

// auditUser is the identity stamped into audit entries (W6). Under GLM_TOKEN
// the stored username is not who the write ran as — a wrong name in the
// audit trail is worse than an honest unknown, and the instance's own
// sys_audit holds the authoritative identity.
func auditUser(res *config.Resolved) string {
	if secret.Token() != "" {
		return "(GLM_TOKEN bearer)"
	}
	return res.Profile.Username
}

// clientForResolved builds an authenticated client for an already-resolved
// profile (credential lookup happens here).
func clientForResolved(cmd *cobra.Command, res *config.Resolved) (*snow.Client, error) {
	timeout, _ := cmd.Flags().GetDuration("timeout")

	// Method resolution (DESIGN-OAUTH.md O8): GLM_TOKEN displaces
	// everything; the synthetic env profile infers client-credentials from
	// GLM_CLIENT_ID+GLM_CLIENT_SECRET (env vars never change a NAMED
	// profile's method — only GLM_TOKEN carries method+credential).
	method := res.Profile.Auth
	if method == "" {
		method = config.AuthBasic
	}
	if res.Name == config.EnvProfileName && secret.ClientID() != "" && secret.ClientSecret() != "" {
		method = config.AuthClientCredentials
	}

	var client *snow.Client
	var err error
	switch {
	case secret.Token() != "":
		// A static bearer for ANY profile — the precedence GLM_PASSWORD
		// established, beating GLM_PASSWORD when both are set (Resolution 2).
		client, err = snow.NewBearer(res.Profile.Instance, secret.Token(), bearerIdentity(secret.Token()), timeout)
	case method == config.AuthOAuth:
		cfg, cerr := oauthConfigFor(res)
		if cerr != nil {
			return nil, cerr
		}
		if cfg.ClientID == "" {
			return nil, fmt.Errorf("profile %q has no client_id — set it with `glm profile add %s --instance %s --auth oauth --client-id <id>`", res.Name, res.Name, res.Profile.Instance)
		}
		username := res.Profile.Username
		if username == "" {
			// `profile login` stores the real username (O10); before the
			// first login a stable per-profile identity keys the cache.
			username = "oauth-" + res.Name
		}
		client, err = snow.NewTokenAuth(res.Profile.Instance, newOAuthSource(res, cfg), username, timeout)
	case method == config.AuthClientCredentials:
		cfg, cerr := oauthConfigFor(res)
		if cerr != nil {
			return nil, cerr
		}
		if cfg.ClientID == "" || cfg.ClientSecret == "" {
			return nil, fmt.Errorf("profile %q needs a client_id and a client secret for client-credentials — store them with `glm profile add %s --instance %s --auth client-credentials --client-id <id> --client-secret-stdin`, or set %s/%s", res.Name, res.Name, res.Profile.Instance, secret.EnvClientID, secret.EnvClientSecret)
		}
		username := res.Profile.Username
		if username == "" {
			username = "cc-" + res.Name
		}
		client, err = snow.NewTokenAuth(res.Profile.Instance, newCCSource(res, cfg), username, timeout)
	default:
		var password string
		password, err = secret.Get(res.Name)
		if err != nil {
			return nil, err
		}
		client, err = snow.NewBasic(res.Profile.Instance, res.Profile.Username, password, timeout)
	}
	if err != nil {
		return nil, err
	}

	if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
		errOut := cmd.ErrOrStderr()
		client.SetLogf(func(format string, args ...any) {
			fmt.Fprintf(errOut, "glm: "+format+"\n", args...)
		})
	}
	return client, nil
}

// clientFor is resolveProfile + clientForResolved for the common case.
func clientFor(cmd *cobra.Command, flagName string) (*snow.Client, *config.Resolved, error) {
	res, err := resolveProfile(cmd, flagName)
	if err != nil {
		return nil, nil, err
	}
	client, err := clientForResolved(cmd, res)
	if err != nil {
		return nil, nil, err
	}
	return client, res, nil
}

// schemaStore builds the per-instance schema cache; when no cache dir is
// available it degrades to live lookups (Dir == "").
func schemaStore(client *snow.Client) *schema.Store {
	store, err := schema.NewStore(client)
	if err != nil {
		return &schema.Store{Client: client}
	}
	return store
}

// validateFields checks names against the table's schema and self-heals a
// stale cache: on a validation miss it refetches once — a field created
// after the cache was written is the usual cause — and only surfaces the
// error if the field is still unknown against fresh data (a real typo, with
// a fresh did-you-mean). A cold cache or an unreachable refetch never blocks,
// since the SN API silently ignores unknown fields and a false "field does
// not exist" is worse than a missed typo. cached may be nil.
func validateFields(ctx context.Context, store *schema.Store, table string, cached *schema.TableMeta, names []string) error {
	return validateFieldsWith(ctx, store, table, cached, names, false)
}

// validateWriteFields is the write-path variant (DESIGN-WRITES.md W3): the
// schema is FETCHED when the cache is cold (reads never pay that cost, but a
// write must be checked), an unknown field is a hard error, and sys_* names
// are validated too (via ValidateStrict) rather than blanket-accepted as on
// reads. ServiceNow silently ignores unknown fields on a write, so a typo'd
// field name — including a sys_ typo like "sys_update_on" — is silent data
// loss, the single worst write footgun. The one leniency kept from reads is
// the ACL-filtered guard inside validate: an incomplete dictionary cannot
// prove a field wrong, so partial metadata still passes.
func validateWriteFields(ctx context.Context, store *schema.Store, table string, names []string) error {
	return validateFieldsWith(ctx, store, table, nil, names, true)
}

func validateFieldsWith(ctx context.Context, store *schema.Store, table string, cached *schema.TableMeta, names []string, write bool) error {
	meta := cached
	fetchedFresh := false
	if meta == nil {
		meta = store.GetCached(table)
	}
	if meta == nil {
		if !write {
			return nil
		}
		m, err := store.Get(ctx, table)
		if err != nil {
			return err
		}
		meta, fetchedFresh = m, true
	}
	validate := func(m *schema.TableMeta) error {
		// Writes use the strict check (no sys_* bypass): on a write a typo is
		// silent data loss, so sys_-prefixed names are validated too.
		if write {
			return m.ValidateStrict(names)
		}
		return m.Validate(names)
	}
	verr := validate(meta)
	if fetchedFresh {
		return verr
	}
	// A cached PASS is trusted on reads but not on writes. A warm cache (up to
	// the 7-day TTL) can be stale in either direction: the self-heal below
	// already refetches on a MISS (a field created after the cache), but a
	// field REMOVED or RENAMED since the cache was written still passes here,
	// and trusting it would let a now-unknown field reach the PATCH — which
	// ServiceNow silently ignores, reporting a false success and auditing a
	// change that never happened. So a write always settles a cached verdict
	// against fresh schema; a read refetches only on a miss (a false "unknown
	// field" would block a valid query).
	if verr == nil && !write {
		return nil
	}
	fresh, err := store.Refetch(ctx, table)
	if err != nil || fresh == nil {
		// The refetch could not settle it. Reads let the API judge (a false
		// "unknown field" is worse than a missed typo). Writes fall back to
		// the cached verdict — a cached miss still blocks (a typo is silent
		// data loss), and a cached pass proceeds, no worse than trusting the
		// warm cache as before.
		if write {
			return verr
		}
		return nil
	}
	return validate(fresh)
}
