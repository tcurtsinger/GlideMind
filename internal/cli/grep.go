package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
)

// grepTarget is one table/field pair to search.
type grepTarget struct {
	table string
	field string
}

// defaultGrepTargets are the script surfaces app development actually
// searches; override with --tables.
var defaultGrepTargets = []grepTarget{
	{"sys_script", "script"},         // business rules
	{"sys_script_include", "script"}, // script includes
	{"sys_script_client", "script"},  // client scripts
	{"sys_ui_action", "script"},      // UI actions
}

type grepMatch struct {
	target grepTarget
	sysID  string
	name   string
	lines  []grepLine
	more   int // matching lines beyond the cap
}

type grepLine struct {
	number int
	text   string
}

func newGrepCmd() *cobra.Command {
	var tables, scope string
	var limit, maxMatches int

	cmd := &cobra.Command{
		Use:   "grep <pattern>",
		Short: "Search code across script tables (ripgrep for the instance)",
		Long: "Searches script fields server-side (LIKE) and prints the matching\n" +
			"lines. Default tables: sys_script, sys_script_include,\n" +
			"sys_script_client, sys_ui_action — override with --tables\n" +
			"table[:field],... (field defaults to \"script\").",
		Example: "  glm grep C1Repository\n" +
			"  glm grep getReference --tables sys_script_client\n" +
			"  glm grep gs.eventQueue --scope x_c1s_app --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			if err := encodedQueryValue("pattern", pattern); err != nil {
				return err
			}
			if err := encodedQueryValue("--scope", scope); err != nil {
				return err
			}
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			targets := defaultGrepTargets
			if strings.TrimSpace(tables) != "" {
				targets = nil
				for _, entry := range splitFields(tables) {
					table, field, ok := strings.Cut(entry, ":")
					if !ok || field == "" {
						field = "script"
					}
					targets = append(targets, grepTarget{table: table, field: field})
				}
			}

			var (
				mu      sync.Mutex
				matches []grepMatch
				warns   []string
				capped  []string
				wg      sync.WaitGroup
			)
			for _, tg := range targets {
				wg.Add(1)
				go func(tg grepTarget) {
					defer wg.Done()
					clauses := []string{tg.field + "LIKE" + pattern}
					if scope != "" {
						clauses = append(clauses, "sys_scope.scope="+scope)
					}
					q := url.Values{}
					q.Set("sysparm_query", strings.Join(clauses, "^"))
					q.Set("sysparm_fields", "sys_id,name,"+tg.field)
					q.Set("sysparm_limit", strconv.Itoa(limit))
					q.Set("sysparm_display_value", "false")
					q.Set("sysparm_exclude_reference_link", "true")

					records, err := client.Table(ctx, tg.table, q)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						warns = append(warns, fmt.Sprintf("%s: %v", tg.table, err))
						return
					}
					if len(records) == limit {
						capped = append(capped, tg.table)
					}
					for _, rec := range records {
						m := grepMatch{
							target: tg,
							sysID:  output.Value(rec, "sys_id"),
							name:   output.Value(rec, "name"),
						}
						m.lines, m.more = matchLines(output.Value(rec, tg.field), pattern, maxMatches)
						matches = append(matches, m)
					}
				}(tg)
			}
			wg.Wait()

			if len(matches) == 0 && len(warns) == len(targets) && len(targets) > 0 {
				return fmt.Errorf("every table search failed: %s", strings.Join(warns, "; "))
			}

			order := map[string]int{}
			for i, tg := range targets {
				order[tg.table] = i
			}
			sort.Slice(matches, func(i, j int) bool {
				if a, b := order[matches[i].target.table], order[matches[j].target.table]; a != b {
					return a < b
				}
				return matches[i].name < matches[j].name
			})

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			lineCount := 0
			switch format {
			case "ids":
				for _, m := range matches {
					fmt.Fprintln(out, m.sysID)
				}
			case "json", "jsonl":
				objs := make([]map[string]any, 0, len(matches))
				for _, m := range matches {
					for _, l := range m.lines {
						lineCount++
						objs = append(objs, map[string]any{
							"table": m.target.table, "sys_id": m.sysID, "name": m.name,
							"line": l.number, "text": l.text,
						})
					}
				}
				enc := json.NewEncoder(out)
				enc.SetEscapeHTML(false)
				// json is one document (an array); jsonl is one object per line.
				if format == "json" {
					if err := enc.Encode(objs); err != nil {
						return err
					}
				} else {
					for _, obj := range objs {
						if err := enc.Encode(obj); err != nil {
							return err
						}
					}
				}
			default:
				for _, m := range matches {
					for _, l := range m.lines {
						lineCount++
						fmt.Fprintf(out, "%s:%s:%d: %s\n", m.target.table, m.name, l.number, l.text)
					}
					if m.more > 0 {
						fmt.Fprintf(out, "%s:%s: +%d more matches (glm get %s %s --fields %s --full)\n",
							m.target.table, m.name, m.more, m.target.table, m.sysID, m.target.field)
					}
				}
			}

			errOut := cmd.ErrOrStderr()
			searched := make([]string, len(targets))
			for i, tg := range targets {
				searched[i] = tg.table
			}
			fmt.Fprintf(errOut, "%d matching lines in %d records - searched %s\n",
				lineCount, len(matches), strings.Join(searched, ", "))
			for _, table := range capped {
				fmt.Fprintf(errOut, "%s hit the %d-record cap - raise --limit\n", table, limit)
			}
			for _, warn := range warns {
				fmt.Fprintf(errOut, "warning: %s\n", warn)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tables, "tables", "", "table[:field] list to search (default: script tables)")
	cmd.Flags().StringVar(&scope, "scope", "", "restrict to an application scope (sys_scope.scope)")
	cmd.Flags().IntVar(&limit, "limit", 20, "max records per table")
	cmd.Flags().IntVar(&maxMatches, "max-matches", 5, "max matching lines shown per record")
	return cmd
}

// matchLines finds pattern case-insensitively line by line, capped at max
// lines with the remainder counted. A record whose match spans lines still
// yields one honest marker line.
func matchLines(script, pattern string, max int) ([]grepLine, int) {
	needle := strings.ToLower(pattern)
	var lines []grepLine
	more := 0
	for i, raw := range strings.Split(script, "\n") {
		line := strings.TrimRight(raw, "\r")
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		if len(lines) >= max {
			more++
			continue
		}
		lines = append(lines, grepLine{number: i + 1, text: strings.TrimSpace(line)})
	}
	if len(lines) == 0 {
		lines = append(lines, grepLine{number: 1, text: "(match spans lines)"})
	}
	return lines, more
}
