package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/schema"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newAggCmd() *cobra.Command {
	var o queryOpts
	var groupBy, sumF, avgF, minF, maxF string
	var top int
	var countFlag bool

	cmd := &cobra.Command{
		Use:   "agg <table>",
		Short: "Aggregate records: count/sum/avg/min/max, optionally grouped",
		Long: "Runs a server-side aggregate — the cheap way to answer posture\n" +
			"questions (coverage by state, gaps by owner, totals by family)\n" +
			"without pulling records. Count is implied when no aggregate flag\n" +
			"is given. Groups sort by the first aggregate, descending.",
		Example: "  glm agg incident --group-by state\n" +
			"  glm agg sn_compliance_control --group-by status -q active=true\n" +
			"  glm agg task --group-by assignment_group --avg reassignment_count --top 10",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			table := args[0]
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			encoded, err := buildEncodedQuery(&o, false)
			if err != nil {
				return err
			}
			groups := splitFields(groupBy)
			sums, avgs := splitFields(sumF), splitFields(avgF)
			mins, maxs := splitFields(minF), splitFields(maxF)
			if !countFlag && len(sums)+len(avgs)+len(mins)+len(maxs) == 0 {
				countFlag = true
			}

			// Cache-only typo check; never an extra call.
			if meta := schemaStore(client).GetCached(table); meta != nil {
				var names []string
				for _, set := range [][]string{groups, sums, avgs, mins, maxs} {
					names = append(names, set...)
				}
				names = append(names, schema.ExtractQueryFields(encoded)...)
				if err := meta.Validate(names); err != nil {
					return err
				}
			}

			q := url.Values{}
			if encoded != "" {
				q.Set("sysparm_query", encoded)
			}
			if countFlag {
				q.Set("sysparm_count", "true")
			}
			if len(groups) > 0 {
				q.Set("sysparm_group_by", strings.Join(groups, ","))
			}
			setFields := func(param string, fields []string) {
				if len(fields) > 0 {
					q.Set(param, strings.Join(fields, ","))
				}
			}
			setFields("sysparm_sum_fields", sums)
			setFields("sysparm_avg_fields", avgs)
			setFields("sysparm_min_fields", mins)
			setFields("sysparm_max_fields", maxs)
			q.Set("sysparm_display_value", displayValue(o.raw))

			rows, err := client.Aggregate(ctx, table, q)
			if err != nil {
				return err
			}

			flat, cols := flattenAgg(rows, groups, countFlag, sums, avgs, mins, maxs)
			sortAggRows(flat, cols, len(groups))

			total := len(flat)
			capped := false
			if len(groups) > 0 && top > 0 && total > top {
				flat = flat[:top]
				capped = true
			}

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			if format == "ids" {
				return fmt.Errorf("format ids does not apply to aggregates")
			}
			if err := output.Records(cmd.OutOrStdout(), cols, flat, output.Options{Format: format}); err != nil {
				return err
			}
			if len(groups) > 0 {
				line := fmt.Sprintf("groups 1-%d of %d", len(flat), total)
				if capped {
					line += " - raise --top for more"
				}
				fmt.Fprintln(cmd.ErrOrStderr(), line)
			}
			return nil
		},
	}
	addQueryFlags(cmd, &o)
	cmd.Flags().StringVar(&groupBy, "group-by", "", "comma-separated group fields")
	cmd.Flags().BoolVar(&countFlag, "count", false, "count rows (implied when no other aggregate is given)")
	cmd.Flags().StringVar(&sumF, "sum", "", "comma-separated fields to sum")
	cmd.Flags().StringVar(&avgF, "avg", "", "comma-separated fields to average")
	cmd.Flags().StringVar(&minF, "min", "", "comma-separated fields to take the minimum of")
	cmd.Flags().StringVar(&maxF, "max", "", "comma-separated fields to take the maximum of")
	cmd.Flags().IntVar(&top, "top", 25, "max groups to show after sorting (0 = all)")
	cmd.Flags().BoolVar(&o.raw, "raw", false, "raw group values instead of display values")
	return cmd
}

// flattenAgg turns stats API rows into flat records with deterministic
// columns: groups, then count, then sum(f)/avg(f)/min(f)/max(f) in flag
// order.
func flattenAgg(rows []snow.Record, groups []string, count bool, sums, avgs, mins, maxs []string) ([]map[string]any, []string) {
	cols := append([]string{}, groups...)
	if count {
		cols = append(cols, "count")
	}
	addCols := func(fn string, fields []string) {
		for _, f := range fields {
			cols = append(cols, fn+"("+f+")")
		}
	}
	addCols("sum", sums)
	addCols("avg", avgs)
	addCols("min", mins)
	addCols("max", maxs)

	flat := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{}
		if gfs, ok := r["groupby_fields"].([]any); ok {
			for _, raw := range gfs {
				if g, ok := raw.(map[string]any); ok {
					if field, ok := g["field"].(string); ok {
						m[field] = g["value"]
					}
				}
			}
		}
		stats, _ := r["stats"].(map[string]any)
		if count {
			m["count"] = stats["count"]
		}
		pull := func(fn string, fields []string) {
			sub, _ := stats[fn].(map[string]any)
			for _, f := range fields {
				m[fn+"("+f+")"] = sub[f]
			}
		}
		pull("sum", sums)
		pull("avg", avgs)
		pull("min", mins)
		pull("max", maxs)
		flat = append(flat, m)
	}
	return flat, cols
}

// sortAggRows orders groups by the first aggregate column descending
// (numeric when possible), group values ascending as tiebreak.
func sortAggRows(flat []map[string]any, cols []string, groupCount int) {
	if groupCount == 0 || len(cols) <= groupCount {
		return
	}
	primary := cols[groupCount]
	sort.SliceStable(flat, func(i, j int) bool {
		a := output.Value(flat[i], primary)
		b := output.Value(flat[j], primary)
		fa, errA := strconv.ParseFloat(a, 64)
		fb, errB := strconv.ParseFloat(b, 64)
		if errA == nil && errB == nil && fa != fb {
			return fa > fb
		}
		if errA != nil || errB != nil {
			if a != b {
				return a > b
			}
		}
		for _, g := range cols[:groupCount] {
			ga, gb := output.Value(flat[i], g), output.Value(flat[j], g)
			if ga != gb {
				return ga < gb
			}
		}
		return false
	})
}
