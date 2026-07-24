package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/output"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newCreateCmd() *cobra.Command {
	var fields []string
	var yes, dryRun, noAudit bool

	cmd := &cobra.Command{
		Use:   "create <table> -f field=value",
		Short: "Create one record (writes need --yes)",
		Long: "Inserts a single record into <table>. Field names are checked against\n" +
			"the schema BEFORE sending: ServiceNow silently ignores unknown fields\n" +
			"on a write, so a typo would otherwise be silent data loss. Values are\n" +
			"raw stored values (state=6, not a display label).\n\n" +
			"There is no diff (nothing exists yet); the preview lists the fields\n" +
			"being set. Writes pass two gates: the profile must be write-enabled,\n" +
			"and each create needs --yes (or an interactive y/N). --dry-run prints\n" +
			"the full preview and sends nothing. Sent creates are recorded (field\n" +
			"names only) in the audit log; the new sys_id/number is printed on\n" +
			"success.",
		Example: "  glm create incident -f short_description=\"Printer down\" -f urgency=2 --yes\n" +
			"  glm create sys_user -f user_name=jdoe -f name=\"Jane Doe\" --dry-run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			table := args[0]
			changes, names, err := parseFieldArgs(fields)
			if err != nil {
				return err
			}

			res, err := resolveProfile(cmd, "")
			if err != nil {
				return err
			}
			// Gate 1 (W1) fires first: before schema fetches, credential
			// lookup, and the write itself.
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

			// The POST response echoes the new record; sys_id + number are all
			// we need to name it back, and keep the response small.
			sendQuery := url.Values{}
			sendQuery.Set("sysparm_fields", "sys_id,number")
			path := "/api/now/table/" + url.PathEscape(table)

			// W4/W7 preview: the exact request, who runs it, and the fields
			// being set. There is no old value to diff against on a create.
			errOut := cmd.ErrOrStderr()
			target, err := client.PreviewURL(path, sendQuery)
			if err != nil {
				return err
			}
			fmt.Fprintf(errOut, "POST %s\n", target)
			fmt.Fprintln(errOut, identityLine(res))
			fmt.Fprintln(errOut, output.SanitizeLine("create "+table+":"))
			for _, name := range names {
				fmt.Fprintln(errOut, output.SanitizeLine(fmt.Sprintf("  %s = %s", name, changes[name])))
			}

			if dryRun {
				fmt.Fprintln(errOut, "dry run — nothing sent")
				return nil
			}
			// Gate 2 (W5): per-command confirmation.
			if !yes {
				if err := confirmWrite(cmd, "create? [y/N] "); err != nil {
					return err
				}
			}

			body, err := json.Marshal(changes)
			if err != nil {
				return err
			}
			data, err := client.Raw(ctx, http.MethodPost, path, sendQuery, body)
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
					Command:  "create",
					Method:   http.MethodPost,
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

			// Name the new record from the echoed response: number when the
			// table has one, else the sys_id.
			label := table
			if ident := createdIdent(data); ident != "" {
				label = table + "/" + ident
			}
			fmt.Fprintln(errOut, output.SanitizeLine("created "+label+" ("+strings.Join(names, ", ")+")"))
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&fields, "field", "f", nil, "field to set, field=value (repeatable, required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the write non-interactively")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the full preview and send nothing")
	cmd.Flags().BoolVar(&noAudit, "no-audit", false, "skip the local write audit log for this call")
	return cmd
}

// createdIdent pulls the human identity of a freshly created record from the
// POST response envelope — number when present, else sys_id, else empty.
func createdIdent(data []byte) string {
	var resp struct {
		Result snow.Record `json:"result"`
	}
	if json.Unmarshal(data, &resp) != nil || resp.Result == nil {
		return ""
	}
	if n := output.Value(resp.Result, "number"); n != "" {
		return n
	}
	return output.Value(resp.Result, "sys_id")
}
