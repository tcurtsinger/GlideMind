package cli

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/schema"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newDiffCmd() *cobra.Command {
	var profiles []string
	var fieldsArg string
	var full bool

	cmd := &cobra.Command{
		Use:   "diff <table> [key] -p A -p B",
		Short: "Compare a record or a table's schema between two instances",
		Long: "Answers \"works on one instance, broken on another — what's different?\"\n" +
			"in one command. Pass -p TWICE: the first is the left/base instance,\n" +
			"the second is the right.\n\n" +
			"With a key (sys_id, record number, or display value) it diffs the\n" +
			"RECORD, printing only the fields whose stored values differ. The key\n" +
			"is resolved per-instance — sys_id equality is never assumed across\n" +
			"instances, so a record number is the reliable cross-instance key.\n" +
			"Without a key it diffs the table's SCHEMA: fields present on one side\n" +
			"only, and type/reference mismatches.\n\n" +
			"Read-only. Differences are data, not errors — they never change the\n" +
			"exit code. A record/table missing on ONE side is reported (exit 0);\n" +
			"missing on BOTH is exit 5.",
		Example: "  glm diff incident INC0012345 -p dev -p smartwork\n" +
			"  glm diff incident INC0012345 -p dev -p smartwork --fields state,priority\n" +
			"  glm diff sys_user_group -p dev -p smartwork",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(profiles) != 2 {
				return fmt.Errorf("diff compares two instances — pass -p exactly twice (left then right), got %d", len(profiles))
			}
			nameA, nameB := profiles[0], profiles[1]
			if nameA == nameB {
				return fmt.Errorf("both -p flags name %q — diff compares two different instances", nameA)
			}

			// Each profile resolves and stamps independently (I3), so the
			// transcript names both instances the diff touched.
			clientA, _, err := clientFor(cmd, nameA)
			if err != nil {
				return err
			}
			clientB, _, err := clientFor(cmd, nameB)
			if err != nil {
				return err
			}

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			opts := output.Options{Format: format, Full: full}
			table := args[0]

			if len(args) == 2 {
				return diffRecord(cmd, clientA, clientB, nameA, nameB, table, args[1], splitFields(fieldsArg), opts)
			}
			if fieldsArg != "" {
				return fmt.Errorf("--fields applies to a record diff; a schema diff always compares every field")
			}
			return diffSchema(cmd, clientA, clientB, nameA, nameB, table, opts)
		},
	}
	// Local StringArray shadows the persistent single-value --profile so diff
	// (and only diff) accepts -p twice; every other command keeps the single
	// -p. clientFor is called with explicit names, so it never reads this flag.
	cmd.Flags().StringArrayVarP(&profiles, "profile", "p", nil, "instance to compare — pass exactly twice: -p A -p B")
	cmd.Flags().StringVar(&fieldsArg, "fields", "", "comma-separated fields to compare (record diff; default all)")
	cmd.Flags().BoolVar(&full, "full", false, "no truncation of long values")
	return cmd
}

// diffRecord fetches one record from each instance and prints the fields whose
// stored values differ. The key is resolved independently on each side.
func diffRecord(cmd *cobra.Command, clientA, clientB *snow.Client, nameA, nameB, table, key string, fields []string, opts output.Options) error {
	ctx := cmd.Context()
	baseQuery := url.Values{}
	baseQuery.Set("sysparm_display_value", "false") // stored values, like query (I5)
	baseQuery.Set("sysparm_exclude_reference_link", "true")
	if len(fields) > 0 {
		// sys_id anchors resolution and labelling even when not compared.
		requested := append([]string{"sys_id"}, fields...)
		baseQuery.Set("sysparm_fields", strings.Join(requested, ","))
	}

	recA, errA := newRecordFetcher(clientA, schemaStore(clientA), table, baseQuery)(ctx, key)
	recB, errB := newRecordFetcher(clientB, schemaStore(clientB), table, baseQuery)(ctx, key)

	missA, err := classifyRecordErr(nameA, errA)
	if err != nil {
		return err
	}
	missB, err := classifyRecordErr(nameB, errB)
	if err != nil {
		return err
	}

	errOut := cmd.ErrOrStderr()
	switch {
	case missA && missB:
		return &notFoundError{table: table, key: key} // exit 5
	case missA:
		fmt.Fprintf(errOut, "record %q not found in %s (present in %s)\n", key, nameA, nameB)
		return nil
	case missB:
		fmt.Fprintf(errOut, "record %q not found in %s (present in %s)\n", key, nameB, nameA)
		return nil
	}

	var names []string
	if len(fields) > 0 {
		names = fields
	} else {
		// Union of both records' keys. sys_id is the cross-instance resolution
		// key, not a data field — it differs by design (I5), so comparing it
		// would be pure noise.
		set := map[string]bool{}
		for k := range recA {
			set[k] = true
		}
		for k := range recB {
			set[k] = true
		}
		delete(set, "sys_id")
		for k := range set {
			names = append(names, k)
		}
		sort.Strings(names)
	}

	var rows []map[string]any
	for _, f := range names {
		a, b := output.Value(recA, f), output.Value(recB, f)
		if a != b {
			rows = append(rows, map[string]any{"field": f, nameA: a, nameB: b})
		}
	}
	return renderDiff(cmd, rows, nameA, nameB, opts,
		fmt.Sprintf("%d differing field(s) for %s/%s between %s and %s", len(rows), table, key, nameA, nameB),
		fmt.Sprintf("%s/%s is identical between %s and %s", table, key, nameA, nameB))
}

// diffSchema compares the dictionary of one table across two instances: fields
// present on one side only, and type/reference mismatches.
func diffSchema(cmd *cobra.Command, clientA, clientB *snow.Client, nameA, nameB, table string, opts output.Options) error {
	ctx := cmd.Context()
	metaA, errA := schemaStore(clientA).Get(ctx, table)
	metaB, errB := schemaStore(clientB).Get(ctx, table)

	missA, err := classifyTableErr(nameA, errA)
	if err != nil {
		return err
	}
	missB, err := classifyTableErr(nameB, errB)
	if err != nil {
		return err
	}

	errOut := cmd.ErrOrStderr()
	switch {
	case missA && missB:
		return &schema.NotFoundError{Table: table} // exit 5
	case missA:
		fmt.Fprintf(errOut, "table %q not found in %s (present in %s)\n", table, nameA, nameB)
		return nil
	case missB:
		fmt.Fprintf(errOut, "table %q not found in %s (present in %s)\n", table, nameB, nameA)
		return nil
	}

	set := map[string]bool{}
	for k := range metaA.Fields {
		set[k] = true
	}
	for k := range metaB.Fields {
		set[k] = true
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)

	var rows []map[string]any
	for _, f := range names {
		a := fieldDesc(metaA, f)
		b := fieldDesc(metaB, f)
		if a != b {
			rows = append(rows, map[string]any{"field": f, nameA: a, nameB: b})
		}
	}
	return renderDiff(cmd, rows, nameA, nameB, opts,
		fmt.Sprintf("%d schema difference(s) for %s between %s and %s", len(rows), table, nameA, nameB),
		fmt.Sprintf("%s schema is identical between %s and %s", table, nameA, nameB))
}

// renderDiff prints diff rows (field, A, B) to stdout and a summary to stderr.
// Zero rows emit no table — just the "identical" summary — matching query's
// "0 rows" convention so a machine consumer sees empty, not a bare header.
func renderDiff(cmd *cobra.Command, rows []map[string]any, nameA, nameB string, opts output.Options, diffSummary, sameSummary string) error {
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), sameSummary)
		return nil
	}
	if err := output.Records(cmd.OutOrStdout(), []string{"field", nameA, nameB}, rows, opts); err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), diffSummary)
	return nil
}

// fieldDesc renders a field's schema shape for comparison: "—" when absent,
// "reference→<table>" for a reference, else the raw type.
func fieldDesc(meta *schema.TableMeta, name string) string {
	f, ok := meta.Fields[name]
	if !ok {
		return "—"
	}
	if f.Reference != "" {
		return "reference→" + f.Reference
	}
	return f.Type
}

// classifyRecordErr splits a per-instance fetch error into "record missing on
// this side" (a diff result, not an error) versus a real failure to surface.
// A missing record is a 0-row lookup (number/display key) or a 404 (sys_id).
func classifyRecordErr(name string, err error) (missing bool, fatal error) {
	if err == nil {
		return false, nil
	}
	var nf *notFoundError
	if errors.As(err, &nf) {
		return true, nil
	}
	var ae *snow.APIError
	if errors.As(err, &ae) && ae.StatusCode == 404 {
		return true, nil
	}
	return false, fmt.Errorf("%s: %w", name, err)
}

// classifyTableErr is classifyRecordErr for the schema path: a missing table
// is schema.NotFoundError; anything else is fatal.
func classifyTableErr(name string, err error) (missing bool, fatal error) {
	if err == nil {
		return false, nil
	}
	var nf *schema.NotFoundError
	if errors.As(err, &nf) {
		return true, nil
	}
	return false, fmt.Errorf("%s: %w", name, err)
}
