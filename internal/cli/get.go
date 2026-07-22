package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/exit"
	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/schema"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

var sysIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// notFoundError maps a failed key lookup onto exit 5.
type notFoundError struct{ table, key string }

func (e *notFoundError) Error() string {
	return fmt.Sprintf("no %s record matches %q", e.table, e.key)
}
func (e *notFoundError) ExitCode() int { return exit.NotFound }

func newGetCmd() *cobra.Command {
	var fields string
	var full, raw bool

	cmd := &cobra.Command{
		Use:   "get <table> <sys_id|number|display-value|->",
		Short: "Show one record (all non-empty fields)",
		Long: "Fetches a single record by sys_id or by a human key (record number or\n" +
			"display value). Shows every non-empty field — empty columns are omitted,\n" +
			"which is most of a wide table's payload. Pass - to read keys from stdin\n" +
			"(one per line) and emit JSONL, e.g. fed by `query --format ids`.",
		Example: "  glm get incident INC0012345\n" +
			"  glm get sys_script \"Incident autoclose\" --fields script --full\n" +
			"  glm query incident -q priority=1 --format ids | glm get incident - --json",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			table, key := args[0], args[1]
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			explicit := splitFields(fields)
			baseQuery := url.Values{}
			baseQuery.Set("sysparm_display_value", displayValue(raw))
			baseQuery.Set("sysparm_exclude_reference_link", "true")
			if len(explicit) > 0 {
				requested := explicit
				if !contains(requested, "sys_id") {
					requested = append(append([]string{}, explicit...), "sys_id")
				}
				baseQuery.Set("sysparm_fields", strings.Join(requested, ","))
			}

			fetch := newRecordFetcher(client, table, baseQuery)

			if key == "-" {
				scanner := bufio.NewScanner(cmd.InOrStdin())
				opts := output.Options{Format: "jsonl", Full: full}
				for scanner.Scan() {
					k := strings.TrimSpace(scanner.Text())
					if k == "" {
						continue
					}
					rec, err := fetch(ctx, k)
					if err != nil {
						return fmt.Errorf("key %q: %w", k, err)
					}
					if err := output.RecordDetail(cmd.OutOrStdout(), rec, explicit, opts); err != nil {
						return err
					}
				}
				return scanner.Err()
			}

			rec, err := fetch(ctx, key)
			if err != nil {
				return err
			}

			format, explicitFormat, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			// The key/value detail view is get's default shape on terminals
			// AND in pipes; delimited single-record output happens only when
			// csv/tsv is asked for explicitly.
			if !explicitFormat {
				format = "table"
			}
			// Exactly one requested field in a text format prints the bare
			// value — the shape of "give me this script" for piping.
			if len(explicit) == 1 && (format == "table" || format == "tsv" || format == "csv") {
				fmt.Fprintln(cmd.OutOrStdout(), output.TruncateField(output.Value(rec, explicit[0]), full))
				return nil
			}
			return output.RecordDetail(cmd.OutOrStdout(), rec, explicit, output.Options{Format: format, Full: full})
		},
	}
	cmd.Flags().StringVar(&fields, "fields", "", "comma-separated fields (default: all non-empty)")
	cmd.Flags().BoolVar(&full, "full", false, "no truncation of long values")
	cmd.Flags().BoolVar(&raw, "raw", false, "raw values instead of display values")
	return cmd
}

// newRecordFetcher resolves keys to records: 32-hex keys fetch directly,
// anything else is looked up by record number, then by the table's display
// field (schema metadata is fetched once, lazily).
func newRecordFetcher(client *snow.Client, table string, baseQuery url.Values) func(context.Context, string) (snow.Record, error) {
	var meta *schema.TableMeta
	return func(ctx context.Context, key string) (snow.Record, error) {
		if sysIDPattern.MatchString(key) {
			return client.GetRecord(ctx, table, key, baseQuery)
		}

		if meta == nil {
			m, err := schema.Fetch(ctx, client, table)
			if err != nil {
				return nil, err
			}
			meta = m
		}
		var candidates []string
		if _, ok := meta.Fields["number"]; ok {
			candidates = append(candidates, "number")
		}
		if meta.DisplayField != "number" && meta.DisplayField != "sys_id" {
			candidates = append(candidates, meta.DisplayField)
		}

		for _, field := range candidates {
			q := url.Values{}
			for k, vs := range baseQuery {
				q[k] = vs
			}
			q.Set("sysparm_query", field+"="+key)
			q.Set("sysparm_limit", "2")
			records, err := client.Table(ctx, table, q)
			if err != nil {
				return nil, err
			}
			switch len(records) {
			case 0:
				continue
			case 1:
				return records[0], nil
			default:
				return nil, fmt.Errorf("%q matches multiple %s records on %s (%s and %s) — use a sys_id",
					key, table, field, output.Value(records[0], "sys_id"), output.Value(records[1], "sys_id"))
			}
		}
		return nil, &notFoundError{table: table, key: key}
	}
}
