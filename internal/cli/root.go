// Package cli wires the glm command tree. It owns arg parsing and exit-code
// mapping only; all ServiceNow behavior lives in transport-agnostic packages
// so a future HTTP/MCP facade can reuse them (DESIGN.md §2).
package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// version is stamped at build time via -ldflags "-X ...cli.version=v0.1.0".
var version = "dev"

// exitCoder is implemented by errors that carry a specific glm exit code.
type exitCoder interface {
	ExitCode() int
}

// Run executes the CLI and returns the process exit code.
func Run(args []string) int {
	root := newRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "glm: %v\n", err)
		var ec exitCoder
		if errors.As(err, &ec) {
			return ec.ExitCode()
		}
		return exit.Usage
	}
	return exit.OK
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "glm",
		Short: "GlideMind — a context-economical ServiceNow CLI",
		Long: "glm answers ServiceNow questions with the fewest possible tokens:\n" +
			"compact output, bounded results, zero-config field derivation, and\n" +
			"native encoded queries. Data goes to stdout; summaries and hints go\n" +
			"to stderr so pipes stay clean.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := cmd.PersistentFlags()
	pf.StringP("profile", "p", "", "profile name (env: GLM_PROFILE)")
	pf.Bool("json", false, "output JSONL (shorthand for --format jsonl)")
	pf.String("format", "", "output format: table|tsv|csv|json|jsonl|ids")
	pf.Duration("timeout", 30*time.Second, "HTTP timeout")
	pf.BoolP("verbose", "v", false, "verbose diagnostics on stderr")

	cmd.AddCommand(
		newWhoamiCmd(),
		newProfileCmd(),
	)
	return cmd
}
