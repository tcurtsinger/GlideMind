package cli

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/output"
)

func newPrimeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prime",
		Short: "Print the agent cheatsheet (~640 tokens)",
		Long: "Emits a compact orientation for AI agents: every command with its\n" +
			"synopsis plus the shared conventions. The command list is generated\n" +
			"from the live registry, so it cannot drift from the binary.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			var lines [][2]string
			var walk func(*cobra.Command)
			walk = func(c *cobra.Command) {
				for _, sub := range c.Commands() {
					if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
						continue
					}
					if sub.HasSubCommands() {
						walk(sub)
						continue
					}
					lines = append(lines, [2]string{synopsis(sub), sub.Short})
				}
			}
			walk(cmd.Root())

			width := 0
			for _, l := range lines {
				if n := utf8.RuneCountInString(l[0]); n > width {
					width = n
				}
			}
			fmt.Fprintln(out, "glm - ServiceNow CLI built for context economy. Data on stdout; summaries, pagination, warnings on stderr.")
			if line := profileLine(); line != "" {
				fmt.Fprintln(out, output.SanitizeLine(line))
			}
			for _, l := range lines {
				fmt.Fprintf(out, "%-*s  # %s\n", width, l[0], l[1])
			}
			fmt.Fprint(out, `
Conventions:
- Filters are native encoded queries; repeat -q to AND clauses. --since 15m|2h|3d on query/count/agg/grep.
- Output: table on TTY, TSV piped; --format table|tsv|csv|json|jsonl|ids; --json = JSONL. json/jsonl/ids always carry sys_id.
- Economy: batch independent glm commands into ONE shell call; count/agg before listing;
  chain glm query <t> ... --format ids | glm get <t> - --json; filter big output in the
  shell so bulk data never enters your context.
- agg: --group-by <field> [--sum f|--avg f|--min f|--max f] - count is implied.
- Truncated values end in a marker; --full lifts caps. grep's remainder marker names the exact glm get to run.
- Discover: glm tables <pattern>, then glm schema <table>. Errors suggest corrections.
- get keys: sys_id, record number, or display value. --profile/-p picks the instance (required with 2+ profiles).
`)
			return nil
		},
	}
}

// profileLine renders the configured instances so an agent's first command
// of the session establishes what exists and whether -p is required
// (DESIGN-INSTANCES.md I4). Empty when nothing is configured or the config
// is unreadable — prime stays useful either way.
func profileLine() string {
	f, err := config.Load()
	if err != nil || len(f.Profiles) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f.Profiles))
	for _, name := range f.Names() {
		marker := ""
		if name == f.Default {
			marker = "*" // the default when -p is omitted
		}
		host := strings.TrimPrefix(f.Profiles[name].Instance, "https://")
		if f.Profiles[name].Writable {
			// Writes stay gated behind --yes, but the agent should know
			// which instances CAN be written at all (DESIGN-WRITES.md W1).
			host += ", rw"
		}
		parts = append(parts, fmt.Sprintf("%s%s (%s)", name, marker, host))
	}
	if len(f.Profiles) == 1 {
		return "Profile: " + parts[0]
	}
	note := " - pass -p <name> on every command"
	if f.Default != "" {
		note = " - * is used when -p is omitted"
	}
	return "Profiles: " + strings.Join(parts, ", ") + note
}

// synopsis renders a command's full invocation line, e.g.
// "glm attach list <table> <sys_id|number|display-value>".
func synopsis(c *cobra.Command) string {
	if i := strings.IndexByte(c.Use, ' '); i >= 0 {
		return c.CommandPath() + c.Use[i:]
	}
	return c.CommandPath()
}
