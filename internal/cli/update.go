package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newUpdateCmd() *cobra.Command {
	var fields []string
	var yes, dryRun, diff, noDiff, noAudit bool

	cmd := &cobra.Command{
		Use:   "update <table> <key> -f field=value",
		Short: "Update one record (previews a field diff; writes need --yes)",
		Long: "Sets fields on a single record, resolved by sys_id, record number, or\n" +
			"display value — the same keys glm get takes. Field names are checked\n" +
			"against the schema BEFORE sending: ServiceNow silently ignores unknown\n" +
			"fields on a write, so a typo would otherwise be silent data loss.\n" +
			"Values are raw stored values (state=6, not a display label).\n\n" +
			"Interactively, glm reads the record first and shows a field-level diff\n" +
			"(old → new) to approve; --no-diff skips that read. With --yes the\n" +
			"extra read is skipped unless --diff asks for it. --dry-run prints the\n" +
			"full preview and sends nothing. Writes pass two gates: the profile\n" +
			"must be write-enabled, and each update needs --yes (or an interactive\n" +
			"y/N). Sent updates are recorded (field names only) in the audit log.",
		Example: "  glm update incident INC0012345 -f state=6 -f close_notes=\"fixed by reboot\"\n" +
			"  glm update sys_script <sys_id> -f active=false --dry-run\n" +
			"  glm update incident INC0012345 -f state=2 --yes",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			table, key := args[0], args[1]
			changes, names, err := parseFieldArgs(fields)
			if err != nil {
				return err
			}

			res, err := resolveProfile(cmd, "")
			if err != nil {
				return err
			}
			// Gate 1 (W1) fires first: before schema fetches, credential
			// lookup, and the read-before-write GET.
			if err := requireWritable(res); err != nil {
				return err
			}

			client, err := clientForResolved(cmd, res)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			store := schemaStore(client)

			// W3: strict pre-flight — an unknown field is a hard error.
			if err := validateWriteFields(ctx, store, table, names); err != nil {
				return err
			}

			// W4: the diff is on by default interactively; --yes skips the
			// extra GET unless --diff insists; --dry-run always previews in
			// full (that is its whole point).
			showDiff := diff || (!noDiff && (!yes || dryRun))

			// Resolve the key. A sys_id with no diff wanted needs no read at
			// all; anything else goes through get's resolver, which also
			// yields the current values the diff compares against.
			sysID, label := key, table+"/"+key
			var current snow.Record
			if showDiff || !sysIDPattern.MatchString(key) {
				baseQuery := url.Values{}
				baseQuery.Set("sysparm_display_value", "false")
				baseQuery.Set("sysparm_exclude_reference_link", "true")
				requested := "sys_id,number"
				if showDiff {
					requested += "," + strings.Join(names, ",")
				}
				baseQuery.Set("sysparm_fields", requested)
				rec, err := newRecordFetcher(client, store, table, baseQuery)(ctx, key)
				if err != nil {
					return err
				}
				current = rec
				sysID = output.Value(rec, "sys_id")
				if sysID == "" {
					return fmt.Errorf("record for %q has no readable sys_id — cannot address the update", key)
				}
				if n := output.Value(rec, "number"); n != "" {
					label = table + "/" + n
				}
			}

			// The PATCH response echoes the record; sys_id alone keeps it
			// from paying for fields nobody reads.
			sendQuery := url.Values{}
			sendQuery.Set("sysparm_fields", "sys_id")
			path := "/api/now/table/" + url.PathEscape(table) + "/" + url.PathEscape(sysID)

			// W4/W7 preview: the exact request, who runs it, and the change.
			errOut := cmd.ErrOrStderr()
			target, err := client.PreviewURL(path, sendQuery)
			if err != nil {
				return err
			}
			fmt.Fprintf(errOut, "PATCH %s\n", target)
			fmt.Fprintln(errOut, identityLine(res))
			fmt.Fprintln(errOut, output.SanitizeLine("update "+label+":"))
			for _, name := range names {
				line := fmt.Sprintf("  %s = %s", name, changes[name])
				if current != nil && showDiff {
					if _, ok := current[name]; !ok {
						// The read did not return this field — a read ACL can
						// hide a field the caller may still write. Rendering ""
						// as the old value would fabricate record state (and
						// could mislabel a clear as "unchanged"), so mark it
						// unreadable and never claim it is unchanged.
						line = fmt.Sprintf("  %s: (unreadable) → %s", name, changes[name])
					} else {
						old := output.TruncateField(output.Value(current, name), false)
						line = fmt.Sprintf("  %s: %s → %s", name, old, changes[name])
						if old == changes[name] {
							line += " (unchanged)"
						}
					}
				}
				fmt.Fprintln(errOut, output.SanitizeLine(line))
			}

			if dryRun {
				fmt.Fprintln(errOut, "dry run — nothing sent")
				return nil
			}
			// Gate 2 (W5): per-command confirmation.
			if !yes {
				if err := confirmWrite(cmd, "apply? [y/N] "); err != nil {
					return err
				}
			}

			body, err := json.Marshal(changes)
			if err != nil {
				return err
			}
			_, err = client.Raw(ctx, http.MethodPatch, path, sendQuery, body)
			if !noAudit {
				// W6: best-effort local trail — names, identity, outcome;
				// never values. An audit failure warns but must not fail the
				// write it records.
				result := "ok"
				if err != nil {
					result = "error"
				}
				_, params := scrubTarget(path, sendQuery)
				if aerr := audit.Append(audit.Entry{
					Time:     time.Now().UTC(),
					Instance: res.Profile.Instance,
					Profile:  res.Name,
					User:     auditUser(res),
					Command:  "update",
					Method:   http.MethodPatch,
					Target:   path,
					Params:   params,
					Fields:   names,
					Result:   result,
				}); aerr != nil {
					fmt.Fprintf(errOut, "warning: audit log not written: %v\n", aerr)
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(errOut, output.SanitizeLine("updated "+label+" ("+strings.Join(names, ", ")+")"))
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&fields, "field", "f", nil, "field to set, field=value (repeatable, required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the write non-interactively")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the full preview and send nothing")
	cmd.Flags().BoolVar(&diff, "diff", false, "fetch and show the field diff even with --yes")
	cmd.Flags().BoolVar(&noDiff, "no-diff", false, "skip the read-before-write diff")
	cmd.Flags().BoolVar(&noAudit, "no-audit", false, "skip the local write audit log for this call")
	cmd.MarkFlagsMutuallyExclusive("diff", "no-diff")
	return cmd
}

// parseFieldArgs turns repeated -f field=value pairs into the write payload
// plus the sorted field names. Duplicates are rejected — last-one-wins would
// silently drop a change the caller thought they made. Dot-walked names are
// rejected — the Table API cannot set a field across a reference, and
// ServiceNow would silently ignore it. An empty value is legal: it clears
// the field.
func parseFieldArgs(pairs []string) (map[string]string, []string, error) {
	if len(pairs) == 0 {
		return nil, nil, fmt.Errorf("nothing to set — pass at least one -f field=value")
	}
	changes := make(map[string]string, len(pairs))
	names := make([]string, 0, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, nil, fmt.Errorf("-f wants field=value, got %q", p)
		}
		if strings.Contains(k, ".") {
			return nil, nil, fmt.Errorf("cannot set dot-walked field %q — update the referenced record instead", k)
		}
		if _, dup := changes[k]; dup {
			return nil, nil, fmt.Errorf("field %q is set twice", k)
		}
		changes[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	return changes, names, nil
}

// confirmWrite is gate 2 (W5) when --yes was not passed: on a TTY it asks
// y/N; anywhere else (pipes, agents, CI) it refuses — a non-interactive
// write must be explicit, never implied.
func confirmWrite(cmd *cobra.Command, prompt string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("non-interactive writes need --yes (glm is read-only without it)")
	}
	fmt.Fprint(cmd.ErrOrStderr(), prompt)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("update cancelled")
}
