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

// clientFor resolves the active profile (flagName overrides the --profile
// flag when non-empty, e.g. `glm profile test <name>`) and builds an
// authenticated client from it.
func clientFor(cmd *cobra.Command, flagName string) (*snow.Client, *config.Resolved, error) {
	name := flagName
	if name == "" {
		name, _ = cmd.Flags().GetString("profile")
	}
	res, err := config.Resolve(name)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("profile %q: auth method %q is not supported yet (v1 supports: basic)", res.Name, res.Profile.Auth)
	}

	password, err := secret.Get(res.Name)
	if err != nil {
		return nil, nil, err
	}

	timeout, _ := cmd.Flags().GetDuration("timeout")
	client, err := snow.NewBasic(res.Profile.Instance, res.Profile.Username, password, timeout)
	if err != nil {
		return nil, nil, err
	}

	if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
		errOut := cmd.ErrOrStderr()
		client.SetLogf(func(format string, args ...any) {
			fmt.Fprintf(errOut, "glm: "+format+"\n", args...)
		})
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
	meta := cached
	if meta == nil {
		meta = store.GetCached(table)
	}
	if meta == nil {
		return nil
	}
	if err := meta.Validate(names); err == nil {
		return nil
	}
	fresh, err := store.Refetch(ctx, table)
	if err != nil || fresh == nil {
		return nil
	}
	return fresh.Validate(names)
}
