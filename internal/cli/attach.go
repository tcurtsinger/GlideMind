package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
)

// attachFields is the fixed column set for attach list. sys_id is visible in
// every format — it is the handle attach get needs.
var attachFields = []string{"file_name", "size_bytes", "content_type", "sys_updated_on", "sys_id"}

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "List and download record attachments",
	}
	cmd.AddCommand(newAttachListCmd(), newAttachGetCmd())
	return cmd
}

func newAttachListCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "list <table> <sys_id|number|display-value>",
		Short: "List attachments on a record",
		Long: "Lists a record's attachments with the sys_ids that glm attach get\n" +
			"downloads. The record key resolves like glm get: sys_id, record\n" +
			"number, or display value.",
		Example: "  glm attach list incident INC0012345\n" +
			"  glm attach list incident INC0012345 --format ids",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			table, key := args[0], args[1]
			if err := encodedQueryValue("table", table); err != nil {
				return err
			}
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			sysID := key
			if !sysIDPattern.MatchString(key) {
				// Resolve human keys exactly like get, fetching only sys_id.
				baseQuery := url.Values{}
				baseQuery.Set("sysparm_fields", "sys_id")
				fetch := newRecordFetcher(client, schemaStore(client), table, baseQuery)
				rec, err := fetch(ctx, key)
				if err != nil {
					return err
				}
				sysID = output.Value(rec, "sys_id")
			}

			q := url.Values{}
			q.Set("sysparm_query", "table_name="+table+"^table_sys_id="+sysID+"^ORDERBYfile_name")
			q.Set("sysparm_fields", strings.Join(attachFields, ","))
			q.Set("sysparm_limit", strconv.Itoa(limit))
			q.Set("sysparm_display_value", "false")
			q.Set("sysparm_exclude_reference_link", "true")
			records, total, err := client.TablePage(ctx, "sys_attachment", q)
			if err != nil {
				return err
			}

			format, _, err := resolveFormat(cmd)
			if err != nil {
				return err
			}
			if err := output.Records(cmd.OutOrStdout(), attachFields, records, output.Options{Format: format}); err != nil {
				return err
			}
			emitPageMeta(cmd.ErrOrStderr(), 0, len(records), total, limit)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "max attachments to list")
	return cmd
}

func newAttachGetCmd() *cobra.Command {
	var dest string

	cmd := &cobra.Command{
		Use:   "get <sys_id>",
		Short: "Download an attachment",
		Long: "Downloads one attachment by its sys_attachment sys_id (from glm\n" +
			"attach list). Writes the attachment's own file name in the current\n" +
			"directory unless -o names a path; -o - streams to stdout. The written\n" +
			"path goes to stdout, the size summary to stderr.",
		Example: "  glm attach get 003a3ef24ff1120031577d2ca310c74b\n" +
			"  glm attach get 003a3ef24ff1120031577d2ca310c74b -o error.log\n" +
			"  glm attach get 003a3ef24ff1120031577d2ca310c74b -o - | head",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !sysIDPattern.MatchString(id) {
				return fmt.Errorf("%q is not an attachment sys_id - copy one from glm attach list", id)
			}
			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			meta, err := client.Attachment(ctx, id)
			if err != nil {
				return err
			}
			// The server controls file_name — never let it traverse paths.
			name := filepath.Base(output.Value(meta, "file_name"))
			if name == "" || name == "." || name == string(filepath.Separator) {
				name = id
			}
			summary := func(n int64) {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s - %d bytes (%s)\n", name, n, output.Value(meta, "content_type"))
			}

			if dest == "-" {
				n, err := client.DownloadAttachment(ctx, id, cmd.OutOrStdout())
				if err != nil {
					return err
				}
				summary(n)
				return nil
			}

			target := dest
			if target == "" {
				target = name
				// A derived name never overwrites; an explicit -o does.
				if _, err := os.Stat(target); err == nil {
					return fmt.Errorf("%s already exists - pass -o <path> to choose a destination", target)
				}
			} else if info, err := os.Stat(target); err == nil && info.IsDir() {
				target = filepath.Join(target, name)
			}

			// Download into a sibling temp file and rename only on success —
			// a failed download must never truncate an existing target.
			f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".glm*")
			if err != nil {
				return err
			}
			tmp := f.Name()
			n, err := client.DownloadAttachment(ctx, id, f)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = os.Rename(tmp, target)
			}
			if err != nil {
				os.Remove(tmp) //nolint:errcheck
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), target)
			summary(n)
			return nil
		},
	}
	cmd.Flags().StringVarP(&dest, "output", "o", "", "output path (- for stdout; default: the attachment's file name)")
	return cmd
}
