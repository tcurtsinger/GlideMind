package cli

import (
	"context"
	"fmt"
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
	if res.Profile.Auth != "" && res.Profile.Auth != "basic" {
		return nil, fmt.Errorf("profile %q: auth method %q is not supported yet (v1 supports: basic)", res.Name, res.Profile.Auth)
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
// under an unexpected identity or instance.
func identityLine(res *config.Resolved) string {
	return output.SanitizeLine(fmt.Sprintf("as %s @ %s (profile %s)", res.Profile.Username, strings.TrimPrefix(res.Profile.Instance, "https://"), res.Name))
}

// clientForResolved builds an authenticated client for an already-resolved
// profile (credential lookup happens here).
func clientForResolved(cmd *cobra.Command, res *config.Resolved) (*snow.Client, error) {
	timeout, _ := cmd.Flags().GetDuration("timeout")

	var client *snow.Client
	var err error
	if token := secret.Token(); token != "" {
		// GLM_TOKEN supplies a static bearer for ANY profile — the same
		// precedence GLM_PASSWORD established (the profile picks the
		// instance, env may supply the credential), beating GLM_PASSWORD
		// when both are set (DESIGN-OAUTH.md O8, Resolution 2). The schema
		// cache is keyed per user (dictionary reads are ACL-filtered); a
		// bearer may not know its identity, so GLM_USERNAME (already folded
		// into the profile by config.Resolve) names it, else a stable
		// pseudo-user keys the cache — fine for the one-identity-per-
		// environment CI norm.
		username := res.Profile.Username
		if username == "" {
			username = "token"
		}
		client, err = snow.NewBearer(res.Profile.Instance, token, username, timeout)
	} else {
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
