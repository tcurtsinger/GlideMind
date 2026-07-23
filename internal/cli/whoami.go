package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newWhoamiCmd() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show your identity and roles on the instance",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, res, err := clientFor(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			fmt.Fprintf(out, "profile   %s (%s)\n", res.Name, res.Source)
			fmt.Fprintf(out, "instance  %s\n", client.BaseURL())

			q := url.Values{}
			q.Set("sysparm_query", "user_name="+res.Profile.Username)
			q.Set("sysparm_fields", "user_name,name,email,title")
			q.Set("sysparm_limit", "1")
			q.Set("sysparm_display_value", "true")
			q.Set("sysparm_exclude_reference_link", "true")
			users, err := client.Table(ctx, "sys_user", q)
			if err != nil {
				return err
			}
			if len(users) == 0 {
				fmt.Fprintf(out, "user      %s (authenticated, but its sys_user record is not visible)\n", res.Profile.Username)
				return nil
			}
			u := users[0]
			fmt.Fprintf(out, "user      %s (%s)\n", field(u, "user_name"), field(u, "name"))
			if email := field(u, "email"); email != "" {
				fmt.Fprintf(out, "email     %s\n", email)
			}
			if title := field(u, "title"); title != "" {
				fmt.Fprintf(out, "title     %s\n", title)
			}

			// Roles are paginated to exhaustion: a heavily-privileged
			// identity can exceed a single window, and reporting a capped
			// list as the exact total would understate its authorization.
			const rolePage = 200
			names := map[string]bool{}
			for offset := 0; ; offset += rolePage {
				rq := url.Values{}
				rq.Set("sysparm_query", "user.user_name="+res.Profile.Username+"^ORDERBYsys_id")
				rq.Set("sysparm_fields", "role.name")
				rq.Set("sysparm_limit", strconv.Itoa(rolePage))
				rq.Set("sysparm_offset", strconv.Itoa(offset))
				rq.Set("sysparm_exclude_reference_link", "true")
				grants, err := client.Table(ctx, "sys_user_has_role", rq)
				if err != nil {
					return fmt.Errorf("list roles: %w", err)
				}
				for _, g := range grants {
					if n := field(g, "role.name"); n != "" {
						names[n] = true
					}
				}
				if len(grants) < rolePage {
					break
				}
			}
			if len(names) > 0 {
				roles := make([]string, 0, len(names))
				for n := range names {
					roles = append(roles, n)
				}
				sort.Strings(roles)
				// A privileged account can carry hundreds of roles; dumping
				// them all into an agent's context on a sanity check is the
				// anti-pattern glm exists to avoid. Preview a few, name the
				// total, and offer --full for the complete list.
				const preview = 10
				if !full && len(roles) > preview {
					fmt.Fprintf(out, "roles     %s … +%d more (%d total) - --full for all\n",
						strings.Join(roles[:preview], ", "), len(roles)-preview, len(roles))
				} else {
					fmt.Fprintf(out, "roles     %s (%d)\n", strings.Join(roles, ", "), len(roles))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "list every role instead of a preview")
	return cmd
}

// field reads a Table API value as a string, tolerating both plain values and
// {display_value, link} reference objects.
func field(r snow.Record, name string) string {
	switch v := r[name].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["display_value"].(string); ok {
			return s
		}
	}
	return ""
}
