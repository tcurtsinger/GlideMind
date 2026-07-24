package cli

import (
	"context"
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

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			// ids emits sys_ids for chaining; a diff row is a field comparison
			// with no sys_id, so the ids renderer would error — and only when
			// rows exist, which would make the presence of differences change
			// the exit code (I5: differences are data, exit 0). Reject it up
			// front instead, before any network call, so the outcome never
			// depends on whether the two sides differ.
			if format == "ids" {
				return fmt.Errorf("--format ids is not meaningful for diff (rows are field comparisons, not records) — use table, csv, tsv, or json")
			}
			opts := output.Options{Format: format, Full: full}
			table := args[0]

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
	storeA, storeB := schemaStore(clientA), schemaStore(clientB)

	// Explicit fields are validated against both schemas before comparing: a
	// name the Table API omits reads as "" on both sides and would otherwise be
	// silently reported as identical (a typo'd --fields hidden as a match).
	if len(fields) > 0 {
		if err := validateDiffFields(ctx, storeA, storeB, table, fields); err != nil {
			return err
		}
	}

	baseQuery := url.Values{}
	baseQuery.Set("sysparm_display_value", "false") // stored values, like query (I5)
	baseQuery.Set("sysparm_exclude_reference_link", "true")
	if len(fields) > 0 {
		// sys_id anchors resolution and labelling even when not compared.
		requested := append([]string{"sys_id"}, fields...)
		baseQuery.Set("sysparm_fields", strings.Join(requested, ","))
	}

	recA, errA := newRecordFetcher(clientA, storeA, table, baseQuery)(ctx, key)
	recB, errB := newRecordFetcher(clientB, storeB, table, baseQuery)(ctx, key)

	missA, ferr := classifyRecordErr(nameA, errA)
	if ferr != nil {
		return ferr
	}
	missB, ferr := classifyRecordErr(nameB, errB)
	if ferr != nil {
		return ferr
	}
	if missA && missB {
		return &notFoundError{table: table, key: key} // exit 5
	}
	// A record missing on one side is compared against an empty record: every
	// field the other side has "differs" (present vs absent). Humans get the
	// concise not-found line; machine output still gets those rows, so
	// --format json stays valid and informative instead of empty stdout.
	if missA {
		recA = snow.Record{}
	}
	if missB {
		recB = snow.Record{}
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

	// When one side is missing, every compared field is emitted regardless of
	// value — the difference is present-on-one-instance vs absent, so a field
	// that happens to be blank on the present side (e.g. `--fields u_optional`)
	// must still produce a row. Otherwise the one-sided diff could collapse to
	// zero rows and read as identical to a pipe consumer.
	oneSided := missA || missB
	var rows []map[string]any
	for _, f := range names {
		a, b := output.Value(recA, f), output.Value(recB, f)
		if a != b || oneSided {
			rows = append(rows, map[string]any{"field": f, nameA: a, nameB: b})
		}
	}
	return renderDiff(cmd, rows, nameA, nameB, opts,
		oneSidedMsg(missA, missB, nameA, nameB, fmt.Sprintf("record %q", key)),
		fmt.Sprintf("%d differing field(s) for %s/%s between %s and %s", len(rows), table, key, nameA, nameB),
		fmt.Sprintf("%s/%s is identical between %s and %s", table, key, nameA, nameB))
}

// diffSchema compares the dictionary of one table across two instances: fields
// present on one side only, and type/reference mismatches.
func diffSchema(cmd *cobra.Command, clientA, clientB *snow.Client, nameA, nameB, table string, opts output.Options) error {
	ctx := cmd.Context()
	// A schema diff is a direct truth claim about the live dictionaries, so it
	// fetches them fresh (Refetch) rather than trusting cache within the 7-day
	// TTL — a column added or dropped after a cache warmed must not let diff
	// report "identical". Records are already fetched live; this keeps both
	// sides of what diff compares equally current.
	metaA, errA := schemaStore(clientA).Refetch(ctx, table)
	metaB, errB := schemaStore(clientB).Refetch(ctx, table)

	missA, ferr := classifyTableErr(nameA, errA)
	if ferr != nil {
		return ferr
	}
	missB, ferr := classifyTableErr(nameB, errB)
	if ferr != nil {
		return ferr
	}
	if missA && missB {
		return &schema.NotFoundError{Table: table} // exit 5
	}
	// A schema diff is only trustworthy on a COMPLETE dictionary. A ServiceNow
	// dictionary always carries the sys_id row; its absence means the metadata
	// is ACL-filtered/partial (a least-privilege profile that sees the table
	// but not every sys_dictionary row), and comparing partial field maps could
	// report a false "identical" or spurious "only on one side". Refuse rather
	// than make a claim glm cannot stand behind. (Only present dictionaries are
	// checked; a table absent on one side is the one-sided case below.)
	if !missA {
		if err := requireCompleteDict(nameA, table, metaA); err != nil {
			return err
		}
	}
	if !missB {
		if err := requireCompleteDict(nameB, table, metaB); err != nil {
			return err
		}
	}
	// A table absent on one side is compared against an empty schema, so every
	// field the other side has shows as present-vs-absent (fieldDesc "—"). Same
	// contract as the record path: humans get the not-found line, machine
	// output stays a valid array.
	empty := &schema.TableMeta{Fields: map[string]schema.Field{}}
	if missA {
		metaA = empty
	}
	if missB {
		metaB = empty
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
		oneSidedMsg(missA, missB, nameA, nameB, fmt.Sprintf("table %q", table)),
		fmt.Sprintf("%d schema difference(s) for %s between %s and %s", len(rows), table, nameA, nameB),
		fmt.Sprintf("%s schema is identical between %s and %s", table, nameA, nameB))
}

// renderDiff prints diff rows (field, A, B) to stdout and a summary to stderr.
// Only the interactive `table` format suppresses stdout for an identical or
// one-sided result — there the stderr summary is the human answer (a one-sided
// miss is reported as a line, per I5) and a table would be noise. Every other
// format is machine-consumable (json/jsonl, and csv/tsv piped to a script), so
// it always renders a parseable result: `[]`/empty for identical, the
// present-vs-absent rows for a one-sided miss — a pipe consumer must never
// mistake "missing on one side" for "identical". A genuine field diff renders
// in every format. oneSided is empty unless a side is missing.
func renderDiff(cmd *cobra.Command, rows []map[string]any, nameA, nameB string, opts output.Options, oneSided, diffSummary, sameSummary string) error {
	var render bool
	switch {
	case len(rows) == 0:
		// Identical: json/jsonl emit the empty structure; delimited/table stay
		// empty (their emptiness already means "no differences").
		render = opts.Format == "json" || opts.Format == "jsonl"
	case oneSided == "":
		render = true // a genuine field diff — every format
	default:
		render = opts.Format != "table" // one-sided: render rows everywhere but the human table
	}
	if render {
		if err := output.Records(cmd.OutOrStdout(), []string{"field", nameA, nameB}, rows, opts); err != nil {
			return err
		}
	}
	summary := sameSummary
	switch {
	case oneSided != "":
		summary = oneSided
	case len(rows) > 0:
		summary = diffSummary
	}
	fmt.Fprintln(cmd.ErrOrStderr(), summary)
	return nil
}

// oneSidedMsg renders the "present on one instance only" result line (per I5),
// or "" when the subject exists on both sides. subject is e.g. `record "INC1"`
// or `table "incident"`.
func oneSidedMsg(missA, missB bool, nameA, nameB, subject string) string {
	switch {
	case missA:
		return fmt.Sprintf("%s not found in %s (present in %s)", subject, nameA, nameB)
	case missB:
		return fmt.Sprintf("%s not found in %s (present in %s)", subject, nameB, nameA)
	}
	return ""
}

// validateDiffFields rejects a --fields name only when it is provably unknown
// on BOTH instances. A field present on either side is legitimate — schema
// drift between instances is exactly what diff exists to surface — so the
// check is against the union; a name unknown everywhere (a typo) would
// otherwise read as "" on both sides and be silently reported as identical.
// TableMeta.Validate stays lenient (an ACL-filtered/partial dictionary can't
// prove a field wrong). A table absent on one instance contributes no schema;
// if neither has it, the diff itself reports the miss, so validation is
// skipped. On a validation miss it self-heals like validateFields: a field
// created after the caches warmed is refetched once and re-checked before
// being rejected, so a valid new field is never blocked by a stale cache.
func validateDiffFields(ctx context.Context, storeA, storeB *schema.Store, table string, fields []string) error {
	metas, err := diffMetas(ctx, storeA, storeB, table, false)
	if err != nil {
		// Schema ACCESS failed (a least-privilege profile without
		// sys_dictionary/sys_db_object read, or the endpoint temporarily
		// blocked). Unavailable metadata cannot prove a field wrong, and an
		// otherwise valid record diff must not become unusable for such
		// profiles — only a loaded dictionary may reject a field. A real
		// connectivity problem still surfaces from the record fetch itself.
		return nil
	}
	if len(metas) == 0 || firstUnknownField(metas, fields) == nil {
		return nil
	}
	// Something looks unknown on every cached dictionary — it may have been
	// created after the caches warmed. Refetch live and re-check; only reject
	// if it is still unknown everywhere. A refetch that fails to settle it
	// leaves the field unblocked (a false "unknown field" is worse than a
	// missed typo — the same call reads chooses).
	fresh, ferr := diffMetas(ctx, storeA, storeB, table, true)
	if ferr != nil || len(fresh) == 0 {
		return nil
	}
	return firstUnknownField(fresh, fields)
}

// diffMetas loads the table's schema from both stores, cached (refresh=false)
// or live (refresh=true), skipping an instance where the table is absent.
func diffMetas(ctx context.Context, storeA, storeB *schema.Store, table string, refresh bool) ([]*schema.TableMeta, error) {
	var metas []*schema.TableMeta
	for _, s := range []*schema.Store{storeA, storeB} {
		var m *schema.TableMeta
		var err error
		if refresh {
			m, err = s.Refetch(ctx, table)
		} else {
			m, err = s.Get(ctx, table)
		}
		if err != nil {
			var nf *schema.NotFoundError
			if errors.As(err, &nf) {
				continue // table absent here; the other side (or the diff) handles it
			}
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, nil
}

// firstUnknownField returns the validation error for the first field provably
// unknown on EVERY meta (the union check), or nil when each field is known on
// at least one instance (or no dictionary can prove it wrong). The STRICT
// check is used — no sys_* bypass: a diff is a truth claim, and a typo'd
// system field ("sys_update_on") read as "" on both sides would report the
// records as identical. ValidateStrict keeps the partial-dictionary leniency,
// so an ACL-filtered dictionary still cannot prove any field wrong.
func firstUnknownField(metas []*schema.TableMeta, fields []string) error {
	for _, f := range fields {
		known := false
		var reject error
		for _, m := range metas {
			if err := m.ValidateStrict([]string{f}); err == nil {
				known = true
				break
			} else {
				reject = err
			}
		}
		if !known {
			return reject
		}
	}
	return nil
}

// requireCompleteDict rejects a schema diff built on an untrustworthy
// dictionary. A complete ServiceNow dictionary always carries the sys_id row;
// its absence means the metadata is ACL-filtered/partial, so a comparison
// would be a claim glm cannot stand behind.
func requireCompleteDict(name, table string, meta *schema.TableMeta) error {
	if _, ok := meta.Fields["sys_id"]; !ok {
		return fmt.Errorf("%s: the %q dictionary looks ACL-filtered or partial (no sys_id) — cannot make a trustworthy schema diff; the profile may lack full sys_dictionary read access", name, table)
	}
	return nil
}

// fieldDesc renders a field's schema shape for comparison: "—" when absent,
// "<type>→<target>" for a reference (the type is kept, so a reference vs a
// glide_list to the same target still reads as a mismatch), else the raw type.
func fieldDesc(meta *schema.TableMeta, name string) string {
	f, ok := meta.Fields[name]
	if !ok {
		return "—"
	}
	if f.Reference != "" {
		return f.Type + "→" + f.Reference
	}
	return f.Type
}

// classifyRecordErr splits a per-instance fetch error into "record missing on
// this side" (a diff result, not an error) versus a real failure to surface.
// A record is missing when the lookup returns 0 rows (number/display key), the
// sys_id fetch 404s, OR — for a number/display key — the table itself is
// absent on this instance (schema.NotFoundError from the resolver's schema
// fetch): if the whole table is gone, the record is gone too, which is still a
// one-sided miss, not a fatal error.
func classifyRecordErr(name string, err error) (missing bool, fatal error) {
	if err == nil {
		return false, nil
	}
	var nf *notFoundError
	if errors.As(err, &nf) {
		return true, nil
	}
	var snf *schema.NotFoundError
	if errors.As(err, &snf) {
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
