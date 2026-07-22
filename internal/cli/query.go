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

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			encoded, err := buildEncodedQuery(&o, true)
			if err != nil {
				return err
			}

			store := schemaStore(client)
			var meta *schema.TableMeta
			fields := splitFields(o.fields)
			if len(fields) == 0 {
				if format == "ids" {
					// The ids renderer only needs sys_id — skip schema
					// derivation entirely (no dictionary ACL dependency,
					// no unused columns fetched).
					fields = []string{"sys_id"}
				} else {
					meta, err = store.Get(ctx, table)
					if err != nil {
						return err
					}
					fields = meta.DefaultFields()
				}
			}

			// Pre-flight validation: catch typo'd fields before the request
			// (the SN API silently returns empty strings for unknown ones).
			// Strictly cache/no-network beyond what this command already
			// fetched — validation never adds API calls and never blocks a
			// query on instances without dictionary access.
			if meta == nil {
				meta = store.GetCached(table)
			}
			if meta != nil {
				names := append([]string{}, splitFields(o.fields)...)
				if o.orderBy != "" {
					names = append(names, strings.TrimPrefix(o.orderBy, "-"))
				}
				names = append(names, schema.ExtractQueryFields(encoded)...)
				if err := meta.Validate(names); err != nil {
					return err
				}
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
// table on a terminal and TSV in a pipe (same content, no padding). The
// second return reports whether the caller asked explicitly — commands with
// their own default shape (get's detail view) only honor explicit choices.
func resolveFormat(cmd *cobra.Command) (string, bool, error) {
	if jsonFlag, _ := cmd.Flags().GetBool("json"); jsonFlag {
		return "jsonl", true, nil
	}
	f, _ := cmd.Flags().GetString("format")
	if f == "" {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return "table", false, nil
		}
		return "tsv", false, nil
	}
	if !contains(output.Formats, f) {
		return "", false, fmt.Errorf("unknown format %q (formats: %s)", f, strings.Join(output.Formats, "|"))
	}
	return f, true, nil
}

// emitPageMeta writes the pagination summary to stderr: pipes stay clean,
// and both humans and agents see how to get the rest (DESIGN.md §7).
// Windows always advance by limit, never by visible rows: sysparm_limit is
// applied BEFORE ACL evaluation, so a short (even empty) window does not
// mean the query is exhausted, and offset+got could re-issue the same
// window forever.
func emitPageMeta(w io.Writer, offset, got, total, limit int) {
	next := offset + limit
	// With a known total, more windows exist while next < total (ACLs can
	// empty any window without exhausting the query). With an unknown total,
	// only a non-empty window suggests continuing — an empty one is the stop
	// signal, never a hint pointing at itself.
	hasMore := next < total
	if total < 0 {
		hasMore = got > 0
	}

	var line string
	switch {
	case got == 0 && offset == 0:
		line = "no rows"
	case got == 0:
		line = "no rows in this window"
	case total >= 0:
		line = fmt.Sprintf("rows %d-%d of %d", offset+1, offset+got, total)
	default:
		line = fmt.Sprintf("rows %d-%d - more may exist", offset+1, offset+got)
	}
	if hasMore {
		line += fmt.Sprintf(" - next: --offset %d", next)
	}
	fmt.Fprintln(w, line)
}

func displayValue(raw bool) string {
	if raw {
		return "false"
	}
	return "true"
}

// encodedQueryValue rejects values that would break out of an encoded-query
// clause — ^ is the clause separator and ServiceNow has no in-value escape
// for it, so a caret would inject query logic or silently miss matches.
func encodedQueryValue(kind, v string) error {
	if strings.Contains(v, "^") {
		return fmt.Errorf(`%s contains "^", the encoded-query separator, which cannot be matched server-side — narrow it to a fragment without "^"`, kind)
	}
	return nil
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
