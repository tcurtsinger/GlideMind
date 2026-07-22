package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/schema"
)

func newCountCmd() *cobra.Command {
	var o queryOpts
	cmd := &cobra.Command{
		Use:   "count <table>",
		Short: "Count matching records (just the number)",
		Example: "  glm count incident -q active=true\n" +
			"  glm count syslog -q level=2 --since 1h",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			encoded, err := buildEncodedQuery(&o, false)
			if err != nil {
				return err
			}
			// Cache-only typo check on query fields; never an extra call.
			if meta := schemaStore(client).GetCached(args[0]); meta != nil {
				if err := meta.Validate(schema.ExtractQueryFields(encoded)); err != nil {
					return err
				}
			}
			n, err := client.Count(cmd.Context(), args[0], encoded)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), n)
			return nil
		},
	}
	addQueryFlags(cmd, &o)
	return cmd
}
