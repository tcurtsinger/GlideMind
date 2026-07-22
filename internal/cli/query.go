package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/schema"
)

// queryOpts are the flags shared by the read commands.
type queryOpts struct {
	fields  string
	queries []string
	limit   int
	offset  int
	orderBy string
	since   string
	raw     bool
	full    bool
}

func newQueryCmd() *cobra.Command {
	var o queryOpts
	cmd := &cobra.Command{
		Use:   "query <table>",
		Short: "List records from a table",
		Long: "Lists records with zero-config default columns derived from the table's\n" +
			"schema. Filters are native ServiceNow encoded queries — copy them from\n" +
			"the platform UI's \"Copy query\", or write them by hand (`active=true`).\n" +
			"Repeat -q to AND clauses without shell-quoting the ^ separator.",
		Example: "  glm query incident -q active=true -q priority=1\n" +
			"  glm query sys_script -q collection=incident --fields name,when,order\n" +
			"  glm query syslog --since 15m --order-by -sys_created_on\n" +
			"  glm query incident -q state=1 --format ids | glm get incident - --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			table := args[0]
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			encoded, err := buildEncodedQuery(&o, true)
			if err != nil {
				return err
			}

			fields := splitFields(o.fields)
			if len(fields) == 0 {
				meta, err := schema.Fetch(ctx, client, table)
				if err != nil {
					return err
				}
				fields = meta.DefaultFields()
			}

			// Machine formats always carry sys_id for chaining; tabular
			// formats stay clean unless it is asked for.
			requested := fields
			if format == "ids" || format == "json" || format == "jsonl" {
				if !contains(fields, "sys_id") {
					requested = append(append([]string{}, fields...), "sys_id")
				}
			}

			q := url.Values{}
			if encoded != "" {
				q.Set("sysparm_query", encoded)
			}
			q.Set("sysparm_fields", strings.Join(requested, ","))
			q.Set("sysparm_limit", strconv.Itoa(o.limit))
			if o.offset > 0 {
				q.Set("sysparm_offset", strconv.Itoa(o.offset))
			}
			q.Set("sysparm_display_value", displayValue(o.raw))
			q.Set("sysparm_exclude_reference_link", "true")

			records, total, err := client.TablePage(ctx, table, q)
			if err != nil {
				return err
			}

			if err := output.Records(cmd.OutOrStdout(), fields, records, output.Options{Format: format, Full: o.full}); err != nil {
				return err
			}
			emitPageMeta(cmd.ErrOrStderr(), o.offset, len(records), total, o.limit)
			return nil
		},
	}
	addQueryFlags(cmd, &o)
	cmd.Flags().StringVar(&o.fields, "fields", "", "comma-separated columns (default: derived from the table's schema)")
	cmd.Flags().IntVar(&o.limit, "limit", 25, "max rows to return")
	cmd.Flags().IntVar(&o.offset, "offset", 0, "row offset for pagination")
	cmd.Flags().StringVar(&o.orderBy, "order-by", "", "sort field; prefix with - for descending")
	cmd.Flags().BoolVar(&o.raw, "raw", false, "raw values instead of display values")
	cmd.Flags().BoolVar(&o.full, "full", false, "no truncation of long values")
	return cmd
}

// addQueryFlags registers the filter flags shared with count.
func addQueryFlags(cmd *cobra.Command, o *queryOpts) {
	cmd.Flags().StringArrayVarP(&o.queries, "query", "q", nil, "encoded query clause; repeatable, joined with ^")
	cmd.Flags().StringVar(&o.since, "since", "", "only records created in the last 15m|2h|3d")
}

// buildEncodedQuery joins -q clauses, --since, and --order-by into one
// encoded query string.
func buildEncodedQuery(o *queryOpts, allowOrder bool) (string, error) {
	var parts []string
	for _, part := range o.queries {
		if p := strings.TrimSpace(part); p != "" {
			parts = append(parts, p)
		}
	}
	if o.since != "" {
		minutes, err := parseSince(o.since)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("sys_created_on>=javascript:gs.minutesAgoStart(%d)", minutes))
	}
	if allowOrder && o.orderBy != "" {
		if rest, ok := strings.CutPrefix(o.orderBy, "-"); ok {
			parts = append(parts, "ORDERBYDESC"+rest)
		} else {
			parts = append(parts, "ORDERBY"+o.orderBy)
		}
	}
	return strings.Join(parts, "^"), nil
}

// parseSince converts 15m/2h/3d into whole minutes for
// gs.minutesAgoStart(n), which evaluates server-side and is timezone-safe.
func parseSince(s string) (int, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid --since %q (use forms like 15m, 2h, 3d)", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("invalid --since %q (use forms like 15m, 2h, 3d)", s)
		}
	}
	minutes := int((d + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	return minutes, nil
}

// resolveFormat picks the output format: --json wins, then --format, then
// table on a terminal and TSV in a pipe (same content, no padding).
func resolveFormat(cmd *cobra.Command) (string, error) {
	if jsonFlag, _ := cmd.Flags().GetBool("json"); jsonFlag {
		return "jsonl", nil
	}
	f, _ := cmd.Flags().GetString("format")
	if f == "" {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return "table", nil
		}
		return "tsv", nil
	}
	if !contains(output.Formats, f) {
		return "", fmt.Errorf("unknown format %q (formats: %s)", f, strings.Join(output.Formats, "|"))
	}
	return f, nil
}

// emitPageMeta writes the pagination summary to stderr: pipes stay clean,
// and both humans and agents see how to get the rest (DESIGN.md §7).
func emitPageMeta(w io.Writer, offset, got, total, limit int) {
	switch {
	case got == 0 && offset == 0:
		fmt.Fprintln(w, "no rows")
	case total >= 0:
		line := fmt.Sprintf("rows %d-%d of %d", offset+1, offset+got, total)
		if offset+got < total {
			line += fmt.Sprintf(" - next: --offset %d", offset+got)
		}
		fmt.Fprintln(w, line)
	default:
		line := fmt.Sprintf("rows %d-%d", offset+1, offset+got)
		if got == limit {
			line += fmt.Sprintf(" - more may exist - next: --offset %d", offset+got)
		}
		fmt.Fprintln(w, line)
	}
}

func displayValue(raw bool) string {
	if raw {
		return "false"
	}
	return "true"
}

func splitFields(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var fields []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
