package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/config"
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
