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
	)
	return cmd
}

func newProfileAddCmd() *cobra.Command {
	var instance, username string
	var passwordStdin bool

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
			_, existed := f.Profiles[name]
			f.Profiles[name] = config.Profile{
				Instance: base.String(),
				Auth:     "basic",
				Username: username,
			}
			if f.Default == "" {
				f.Default = name
			}
			if err := secret.Set(name, password); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}

			verb := "added"
			if existed {
				verb = "updated"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q %s (%s as %s)\n", name, verb, base.String(), username)
			fmt.Fprintf(cmd.ErrOrStderr(), "try: glm profile test %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "instance name or URL, e.g. acme or https://acme.service-now.com (required)")
	cmd.Flags().StringVar(&username, "username", "", "instance username (required)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin instead of prompting")
	_ = cmd.MarkFlagRequired("instance")
	_ = cmd.MarkFlagRequired("username")
	return cmd
}

func readPassword(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		pw := strings.TrimRight(string(data), "\r\n")
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
				fmt.Fprintf(out, "%s %s\t%s\t%s\n", marker, name, p.Instance, p.Username)
			}
			return nil
		},
	}
}

func newProfileUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Set the default profile",
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
			f.Default = name
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "default profile: %s\n", name)
			return nil
		},
	}
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
