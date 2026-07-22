package cli

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
)

func newTablesCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "tables [pattern]",
		Short: "Find tables by name or label",
		Example: "  glm tables compliance\n" +
			"  glm tables u_          # custom tables",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}

			clauses := []string{}
			if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
				p := strings.TrimSpace(args[0])
				clauses = append(clauses, "nameLIKE"+p+"^ORlabelLIKE"+p)
			}
			clauses = append(clauses, "ORDERBYname")

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			fields := []string{"name", "label", "super_class"}
			requested := fields
			if format == "ids" || format == "json" || format == "jsonl" {
				requested = append(append([]string{}, fields...), "sys_id")
			}

			q := url.Values{}
			q.Set("sysparm_query", strings.Join(clauses, "^"))
			q.Set("sysparm_fields", strings.Join(requested, ","))
			q.Set("sysparm_limit", strconv.Itoa(limit))
			if offset > 0 {
				q.Set("sysparm_offset", strconv.Itoa(offset))
			}
			q.Set("sysparm_display_value", "true")
			q.Set("sysparm_exclude_reference_link", "true")

			records, total, err := client.TablePage(cmd.Context(), "sys_db_object", q)
			if err != nil {
				return err
			}
			if err := output.Records(cmd.OutOrStdout(), fields, records, output.Options{Format: format}); err != nil {
				return err
			}
			emitPageMeta(cmd.ErrOrStderr(), offset, len(records), total, limit)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "max tables to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "row offset for pagination")
	return cmd
}
