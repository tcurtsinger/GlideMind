package cli

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/output"
)

func newDeleteCmd() *cobra.Command {
	var yes, dryRun, noAudit bool

	cmd := &cobra.Command{
		Use:   "delete <table> <key>",
		Short: "Delete one record (destructive; needs --yes or a typed confirm)",
		Long: "Deletes a single record, resolved by sys_id, record number, or display\n" +
			"value — the same keys glm get takes. This is destructive and has no\n" +
			"undo, so the confirmation is stricter than other writes: interactively\n" +
			"you must TYPE the record's number or sys_id (a plain y/N is not\n" +
			"enough), and non-interactively --yes is required. Writes pass two\n" +
			"gates: the profile must be write-enabled, and the delete must be\n" +
			"confirmed. --dry-run prints the full preview and sends nothing. Sent\n" +
			"deletes are recorded in the audit log.",
		Example: "  glm delete incident INC0012345 --yes\n" +
			"  glm delete sys_user 62826bf03710200044e0bfc8bcbe5df1 --dry-run",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			table, key := args[0], args[1]

			res, err := resolveProfile(cmd, "")
			if err != nil {
				return err
			}
			// Gate 1 (W1) fires first: before credential lookup, the resolve
			// GET, and the delete.
			if err := requireWritable(res); err != nil {
				return err
			}

			client, err := clientForResolved(cmd, res)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			store := schemaStore(client)

			// Resolve the key. A sys_id with --yes needs no read (the URL is
			// the key and confirmation is satisfied); anything else goes
			// through get's resolver — a human key needs the sys_id for the
			// URL, and an interactive delete needs the record's number/sys_id
			// to type against. A missing key fails (exit 5) before any delete.
			sysID, number := key, ""
			if !yes || !sysIDPattern.MatchString(key) {
				baseQuery := url.Values{}
				baseQuery.Set("sysparm_display_value", "false")
				baseQuery.Set("sysparm_exclude_reference_link", "true")
				baseQuery.Set("sysparm_fields", "sys_id,number")
				rec, err := newRecordFetcher(client, store, table, baseQuery)(ctx, key)
				if err != nil {
					return err
				}
				sysID = output.Value(rec, "sys_id")
				if sysID == "" {
					return fmt.Errorf("record for %q has no readable sys_id — cannot address the delete", key)
				}
				number = output.Value(rec, "number")
			}

			ident := sysID
			if number != "" {
				ident = number
			}
			label := table + "/" + ident
			path := "/api/now/table/" + url.PathEscape(table) + "/" + url.PathEscape(sysID)

			// W4/W7 preview: the exact request, who runs it, and the record
			// being destroyed (both number and sys_id, so the typed confirm
			// has something to match).
			errOut := cmd.ErrOrStderr()
			target, err := client.PreviewURL(path, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(errOut, "DELETE %s\n", target)
			fmt.Fprintln(errOut, identityLine(res))
			fmt.Fprintln(errOut, output.SanitizeLine(fmt.Sprintf("delete %s (sys_id %s)", label, sysID)))

			if dryRun {
				fmt.Fprintln(errOut, "dry run — nothing sent")
				return nil
			}
			// Gate 2 (W5): a delete is destructive, so confirmation is a typed
			// match on the record's number/sys_id, not a plain y/N.
			if !yes {
				if err := confirmDelete(cmd, number, sysID); err != nil {
					return err
				}
			}

			_, err = client.Raw(ctx, http.MethodDelete, path, nil, nil)
			if !noAudit {
				// W6: best-effort local trail — identity, target, outcome. A
				// delete carries no field names. An audit failure warns but
				// must not fail the write it records.
				result := "ok"
				if err != nil {
					result = "error"
				}
				if aerr := audit.Append(audit.Entry{
					Time:     time.Now().UTC(),
					Instance: res.Profile.Instance,
					Profile:  res.Name,
					User:     res.Profile.Username,
					Command:  "delete",
					Method:   http.MethodDelete,
					Target:   path,
					Result:   result,
				}); aerr != nil {
					fmt.Fprintf(errOut, "warning: audit log not written: %v\n", aerr)
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(errOut, output.SanitizeLine("deleted "+label))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the delete non-interactively")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the full preview and send nothing")
	cmd.Flags().BoolVar(&noAudit, "no-audit", false, "skip the local write audit log for this call")
	return cmd
}

// confirmDelete is gate 2 (W5) for the destructive verb when --yes was not
// passed: on a TTY it demands the operator TYPE the record's number or sys_id
// (a mistyped or empty line cancels); anywhere else (pipes, agents, CI) it
// refuses — a non-interactive delete must be explicit via --yes.
func confirmDelete(cmd *cobra.Command, number, sysID string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("non-interactive deletes need --yes (glm is read-only without it)")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "type the record's number or sys_id to confirm delete: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("read confirmation: %w", err)
	}
	typed := strings.TrimSpace(line)
	if typed == "" {
		return fmt.Errorf("delete cancelled")
	}
	if matchesRecordKey(typed, number, sysID) {
		return nil
	}
	return fmt.Errorf("delete cancelled — %q does not match the record's number or sys_id", typed)
}

// matchesRecordKey reports whether a typed confirmation identifies the record.
// An empty typed string never matches; an empty number is not a valid target
// (only the sys_id can confirm a numberless record), so it must not be matched
// by an empty input.
func matchesRecordKey(typed, number, sysID string) bool {
	if typed == "" {
		return false
	}
	return typed == sysID || (number != "" && typed == number)
}
