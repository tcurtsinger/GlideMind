package cli

import "testing"

// Exit codes are asserted as literals: the numbers themselves are the
// contract (DESIGN.md §8).
func TestRunExitCodes(t *testing.T) {
	if got := Run([]string{"--version"}); got != 0 {
		t.Errorf("--version exit = %d, want 0", got)
	}
	// A typo'd subcommand must not succeed via the help path.
	if got := Run([]string{"qurey"}); got != 1 {
		t.Errorf("stray arg exit = %d, want 1", got)
	}
	if got := Run([]string{"--no-such-flag"}); got != 1 {
		t.Errorf("unknown flag exit = %d, want 1", got)
	}
}
