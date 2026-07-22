package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
)

func newSchemaCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "schema <table>",
		Short: "Show a table's fields: name, type, reference target, mandatory",
		Long: "Shows the table's dictionary including inherited fields, from a local\n" +
			"per-instance cache (7-day TTL, populated transparently). The chain and\n" +
			"display field go to stderr.",
		Example: "  glm schema incident\n" +
			"  glm schema u_custom_thing --refresh",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			table := args[0]
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			store := schemaStore(client)
			store.Refresh = refresh
			meta, err := store.Get(cmd.Context(), table)
			if err != nil {
				return err
			}

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			var regular, system []string
			for name := range meta.Fields {
				if strings.HasPrefix(name, "sys_") {
					system = append(system, name)
				} else {
					regular = append(regular, name)
				}
			}
			sort.Strings(regular)
			sort.Strings(system)

			rows := make([]map[string]any, 0, len(meta.Fields))
			for _, name := range append(regular, system...) {
				f := meta.Fields[name]
				mandatory := ""
				if f.Mandatory {
					mandatory = "true"
				}
				rows = append(rows, map[string]any{
					"field": name, "type": f.Type, "reference": f.Reference, "mandatory": mandatory,
				})
			}

			cols := []string{"field", "type", "reference", "mandatory"}
			if err := output.Records(cmd.OutOrStdout(), cols, rows, output.Options{Format: format}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%d fields - display field: %s - chain: %s\n",
				len(rows), meta.DisplayField, strings.Join(meta.Chain, " < "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "bypass the schema cache and refetch")
	return cmd
}
