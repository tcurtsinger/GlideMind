package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/secret"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage instance profiles (secrets live in the OS keyring)",
	}
	cmd.AddCommand(
		newProfileAddCmd(),
		newProfileListCmd(),
		newProfileUseCmd(),
		newProfileTestCmd(),
		newProfileRemoveCmd(),
		newProfileWritableCmd("write-enable", true),
		newProfileWritableCmd("write-disable", false),
	)
	return cmd
}

// newProfileWritableCmd flips the per-profile write gate (DESIGN-WRITES.md
// W1). It is a stored, deliberate property — the first of the two gates
// every write must pass (the second is per-command confirmation).
func newProfileWritableCmd(name string, enable bool) *cobra.Command {
	short := "Allow writes on a profile (each still needs --yes)"
	if !enable {
		short = "Make a profile read-only again (the default)"
	}
	return &cobra.Command{
		Use:   name + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load()
			if err != nil {
				return err
			}
			p, ok := f.Profiles[args[0]]
			if !ok {
				return fmt.Errorf("profile %q not found (have: %v)", args[0], f.Names())
			}
			p.Writable = enable
			f.Profiles[args[0]] = p
			if err := f.Save(); err != nil {
				return err
			}
			state := "writable — non-GET `glm api` calls run with --yes"
			if !enable {
				state = "read-only"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q is now %s\n", args[0], state)
			return nil
		},
	}
}

func newProfileAddCmd() *cobra.Command {
	var instance, username string
	var passwordStdin, writable bool

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a profile and store its password in the keyring",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == config.EnvProfileName {
				return fmt.Errorf("%q is reserved for the GLM_INSTANCE env profile", name)
			}

			base, err := snow.NormalizeInstance(instance)
			if err != nil {
				return err
			}

			password, err := readPassword(cmd, passwordStdin)
			if err != nil {
				return err
			}

			f, err := config.Load()
			if err != nil {
				return err
			}
			old, existed := f.Profiles[name]
			keepWritable := preserveWritable(old, existed, cmd.Flags().Changed("writable"), writable)
			f.Profiles[name] = config.Profile{
				Instance: base.String(),
				Auth:     "basic",
				Username: username,
				Writable: keepWritable,
			}
			// Deliberately no auto-default: with one profile it is implicit
			// anyway, and a sticky default armed here would silently route
			// commands to the first-added instance the moment a second one
			// exists — the wrong-instance leak DESIGN-INSTANCES.md I1 closes.
			// A default is only ever set explicitly via `glm profile use`.
			clearedDefault := clearLegacyDefault(f, existed)
			// add is otherwise non-transactional: capture any prior credential
			// so a failed config save can roll the keyring back instead of
			// leaving the old instance/username paired with a new password.
			// GetStored reads the keyring only — Get would return a
			// GLM_PASSWORD override and corrupt the rollback.
			oldPw, hadOld := secret.GetStored(name)
			if err := secret.Set(name, password); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				if hadOld {
					_ = secret.Set(name, oldPw)
				} else {
					_ = secret.Delete(name)
				}
				return err
			}

			verb := "added"
			if existed {
				verb = "updated"
			}
			access := ""
			if keepWritable {
				// Surface preserved writability so a credential rotation
				// shows the write gate is (still) open.
				access = ", rw"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q %s (%s as %s%s)\n", name, verb, base.String(), username, access)
			switch {
			case clearedDefault != "":
				fmt.Fprintf(cmd.ErrOrStderr(), "cleared implicit default %q — commands now require -p <name> (restore: glm profile use %s)\n", clearedDefault, clearedDefault)
			case len(f.Profiles) >= 2 && f.Default == "":
				fmt.Fprintf(cmd.ErrOrStderr(), "%d profiles configured — commands now require -p <name> (or set a default: glm profile use <name>)\n", len(f.Profiles))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "try: glm profile test %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "instance name or URL, e.g. acme or https://acme.service-now.com (required)")
	cmd.Flags().StringVar(&username, "username", "", "instance username (required)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin instead of prompting")
	cmd.Flags().BoolVar(&writable, "writable", false, "allow writes on this profile (default read-only)")
	_ = cmd.MarkFlagRequired("instance")
	_ = cmd.MarkFlagRequired("username")
	return cmd
}

// preserveWritable decides the stored Writable when `profile add` runs: a
// new profile takes the flag (default read-only), but an UPDATE — rotating a
// password, changing the username — keeps the existing value unless
// --writable was explicitly passed. A deliberately write-enabled profile
// must not silently lose its gate to a credential rotation; write-disable
// is the dedicated way to revoke.
func preserveWritable(old config.Profile, existed, flagChanged, flagVal bool) bool {
	if !existed || flagChanged {
		return flagVal
	}
	return old.Writable
}

// clearLegacyDefault migrates configs written when `profile add` still
// auto-defaulted the first profile: when this add takes the config from one
// profile to two and a default is set, the default is cleared (and returned
// for messaging). A default set while only one profile existed never had an
// observable effect — the single-profile fallback covered it — so nothing
// deliberate is lost, and keeping it would silently preserve the
// wrong-instance path DESIGN-INSTANCES.md I1 closes. Defaults chosen in an
// already-multi-profile world are deliberate and stay.
func clearLegacyDefault(f *config.File, existed bool) string {
	if existed || len(f.Profiles) != 2 || f.Default == "" {
		return ""
	}
	old := f.Default
	f.Default = ""
	return old
}

func readPassword(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		// Strip the UTF-8 BOM a PowerShell pipe prepends before trimming the
		// line ending — otherwise the stored password carries U+FEFF and
		// fails auth immediately (matches the batch-key and api-body paths).
		pw := strings.TrimRight(strings.TrimPrefix(string(data), "\ufeff"), "\r\n")
		if pw == "" {
			return "", fmt.Errorf("empty password on stdin")
		}
		return pw, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal — use --password-stdin")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "password: ")
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(pw) == 0 {
		return "", fmt.Errorf("empty password")
	}
	return string(pw), nil
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.Load()
			if err != nil {
				return err
			}
			if len(f.Profiles) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no profiles — add one with: glm profile add <name> --instance <url> --username <user>")
				return nil
			}
			out := cmd.OutOrStdout()
			for _, name := range f.Names() {
				p := f.Profiles[name]
				marker := " "
				if name == f.Default {
					marker = "*"
				}
				access := "ro"
				if p.Writable {
					access = "rw"
				}
				fmt.Fprintf(out, "%s %s\t%s\t%s\t%s\n", marker, name, p.Instance, p.Username, access)
			}
			return nil
		},
	}
}

func newProfileUseCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "use <name> | --clear",
		Short: "Set or clear the default profile",
		Long: "Sets the profile used when -p is omitted. With several profiles\n" +
			"configured this is a deliberate opt-out of glm's refuse-to-guess\n" +
			"rule — every command still stamps the instance it ran against.\n" +
			"--clear removes the default, making -p required again.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load()
			if err != nil {
				return err
			}
			if clear {
				if len(args) != 0 {
					return fmt.Errorf("--clear takes no profile name")
				}
				f.Default = ""
				if err := f.Save(); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "default profile cleared")
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("usage: glm profile use <name> (or --clear)")
			}
			name := args[0]
			if _, ok := f.Profiles[name]; !ok {
				return fmt.Errorf("profile %q not found (have: %v)", name, f.Names())
			}
			f.Default = name
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "default profile: %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the default profile so -p is required")
	return cmd
}

func newProfileTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test [name]",
		Short: "Verify a profile can authenticate to its instance",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			client, res, err := clientFor(cmd, name)
			if err != nil {
				return err
			}

			q := url.Values{}
			q.Set("sysparm_query", "user_name="+res.Profile.Username)
			q.Set("sysparm_fields", "sys_id")
			q.Set("sysparm_limit", "1")
			start := time.Now()
			if _, err := client.Table(cmd.Context(), "sys_user", q); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok — %s as %s (%dms)\n",
				client.BaseURL(), res.Profile.Username, time.Since(start).Milliseconds())
			return nil
		},
	}
}

func newProfileRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a profile and its keyring credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			f, err := config.Load()
			if err != nil {
				return err
			}
			if _, ok := f.Profiles[name]; !ok {
				return fmt.Errorf("profile %q not found (have: %v)", name, f.Names())
			}
			delete(f.Profiles, name)
			if f.Default == name {
				f.Default = ""
			}
			// Save the config before touching the keyring: a failed save
			// then strands only an orphan credential (harmless, overwritten
			// on re-add) instead of a configured profile with no secret.
			if err := f.Save(); err != nil {
				return err
			}
			if err := secret.Delete(name); err != nil {
				return fmt.Errorf("profile removed from config, but deleting its keyring credential failed: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q removed\n", name)
			return nil
		},
	}
}
