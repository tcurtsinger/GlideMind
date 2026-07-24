package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/oauth"
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
		newProfileLoginCmd(),
		newProfileLogoutCmd(),
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
			state := "writable — every write (update, non-GET api) still needs --yes"
			if !enable {
				state = "read-only"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q is now %s\n", args[0], state)
			return nil
		},
	}
}

func newProfileAddCmd() *cobra.Command {
	var instance, username, auth, clientID string
	var passwordStdin, clientSecretStdin, writable bool
	var redirectPort int

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a profile (secrets go to the OS keyring)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == config.EnvProfileName {
				return fmt.Errorf("%q is reserved for the GLM_INSTANCE env profile", name)
			}
			// The CLI accepts the hyphenated spelling; config stores the
			// token-endpoint grant name (DESIGN-OAUTH.md O2).
			method := strings.ReplaceAll(auth, "-", "_")
			switch method {
			case config.AuthBasic, config.AuthOAuth, config.AuthClientCredentials:
			default:
				return fmt.Errorf("unknown --auth %q (supported: basic, oauth, client-credentials)", auth)
			}

			base, err := snow.NormalizeInstance(instance)
			if err != nil {
				return err
			}
			if method == config.AuthBasic && username == "" {
				return fmt.Errorf("--username is required for basic auth")
			}
			if method != config.AuthBasic && clientID == "" {
				return fmt.Errorf("--client-id is required for --auth %s (from the instance's Application Registry entry)", auth)
			}

			// Secret material, gathered before any state changes. Basic
			// needs a password; client-credentials always needs the client
			// secret; an oauth profile only when its registry entry is
			// confidential (O9) — the recommended public client stores none.
			var password, clientSecret string
			switch {
			case method == config.AuthBasic:
				password, err = readSecret(cmd, passwordStdin, "password")
			case method == config.AuthClientCredentials || clientSecretStdin:
				clientSecret, err = readSecret(cmd, clientSecretStdin, "client secret")
			}
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
				Instance:     base.String(),
				Auth:         method,
				Username:     username,
				ClientID:     clientID,
				RedirectPort: redirectPort,
				Writable:     keepWritable,
			}
			// Deliberately no auto-default: with one profile it is implicit
			// anyway, and a sticky default armed here would silently route
			// commands to the first-added instance the moment a second one
			// exists — the wrong-instance leak DESIGN-INSTANCES.md I1 closes.
			// A default is only ever set explicitly via `glm profile use`.
			clearedDefault := clearLegacyDefault(f, existed)
			// add is otherwise non-transactional: capture any prior secret
			// so a failed config save can roll the keyring back instead of
			// leaving the old instance/identity paired with a new secret.
			// GetStored reads the keyring only — Get would return a
			// GLM_PASSWORD override and corrupt the rollback.
			undo := func() {}
			switch {
			case password != "":
				oldPw, hadOld := secret.GetStored(name)
				if err := secret.Set(name, password); err != nil {
					return err
				}
				undo = func() {
					if hadOld {
						_ = secret.Set(name, oldPw)
					} else {
						_ = secret.Delete(name)
					}
				}
			case clientSecret != "":
				oldCS, hadCS := loadClientSecret(name)
				if err := saveClientSecret(name, clientSecret); err != nil {
					return err
				}
				undo = func() {
					if hadCS {
						_ = saveClientSecret(name, oldCS)
					} else {
						_ = deleteClientSecret(name)
					}
				}
			}
			if err := f.Save(); err != nil {
				undo()
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
			who := fmt.Sprintf("as %s", username)
			if username == "" {
				who = "via " + method
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q %s (%s %s%s)\n", name, verb, base.String(), who, access)
			switch {
			case clearedDefault != "":
				fmt.Fprintf(cmd.ErrOrStderr(), "cleared implicit default %q — commands now require -p <name> (restore: glm profile use %s)\n", clearedDefault, clearedDefault)
			case len(f.Profiles) >= 2 && f.Default == "":
				fmt.Fprintf(cmd.ErrOrStderr(), "%d profiles configured — commands now require -p <name> (or set a default: glm profile use <name>)\n", len(f.Profiles))
			}
			if method == config.AuthOAuth {
				fmt.Fprintf(cmd.ErrOrStderr(), "next: glm profile login %s\n", name)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "try: glm profile test %s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "instance name or URL, e.g. acme or https://acme.service-now.com (required)")
	cmd.Flags().StringVar(&username, "username", "", "instance username (required for basic auth)")
	cmd.Flags().StringVar(&auth, "auth", "basic", "auth method: basic, oauth (interactive PKCE), client-credentials")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth client_id from the instance's Application Registry")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin instead of prompting")
	cmd.Flags().BoolVar(&clientSecretStdin, "client-secret-stdin", false, "read the OAuth client secret from stdin instead of prompting")
	cmd.Flags().IntVar(&redirectPort, "redirect-port", 0, "override the OAuth callback port (default 8456; must match the registry entry)")
	cmd.Flags().BoolVar(&writable, "writable", false, "allow writes on this profile (default read-only)")
	_ = cmd.MarkFlagRequired("instance")
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

// readSecret reads one secret value (a password, a client secret) from
// stdin or an interactive no-echo prompt.
func readSecret(cmd *cobra.Command, fromStdin bool, label string) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read %s from stdin: %w", label, err)
		}
		// Strip the UTF-8 BOM a PowerShell pipe prepends before trimming the
		// line ending — otherwise the stored password carries U+FEFF and
		// fails auth immediately (matches the batch-key and api-body paths).
		pw := strings.TrimRight(strings.TrimPrefix(string(data), "\ufeff"), "\r\n")
		if pw == "" {
			return "", fmt.Errorf("empty %s on stdin", label)
		}
		return pw, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal — pipe the %s in via the matching --*-stdin flag", label)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", label)
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	if len(pw) == 0 {
		return "", fmt.Errorf("empty %s", label)
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

			// Mirrors whoami: token-derived identity (GLM_TOKEN, later
			// OAuth) is resolved by the instance, never trusted from config —
			// a stored username may not be who the token actually is.
			identity := res.Profile.Username
			tokenIdent := identity == "" || client.TokenIdentity()
			userQuery := "user_name=" + identity
			if tokenIdent {
				userQuery = "sys_id=javascript:gs.getUserID()"
			}
			q := url.Values{}
			q.Set("sysparm_query", userQuery)
			q.Set("sysparm_fields", "sys_id,user_name")
			q.Set("sysparm_limit", "1")
			start := time.Now()
			rows, err := client.Table(cmd.Context(), "sys_user", q)
			if err != nil {
				return err
			}
			if tokenIdent {
				identity = "(token)"
				if len(rows) > 0 {
					if n := field(rows[0], "user_name"); n != "" {
						identity = n
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok — %s as %s (%dms)\n",
				client.BaseURL(), identity, time.Since(start).Milliseconds())
			return nil
		},
	}
}

// newProfileLoginCmd is the ONE interactive auth command (DESIGN-OAUTH.md
// O3): PKCE through the browser for oauth profiles, a token mint for
// client-credentials. Data commands never launch a browser — an expired
// session always errors naming this command (Resolution 5).
func newProfileLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [name]",
		Short: "Sign in to an OAuth profile (opens your browser; PKCE)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			res, err := resolveProfile(cmd, name)
			if err != nil {
				return err
			}
			if res.Name == config.EnvProfileName {
				return fmt.Errorf("the env profile is stateless — supply GLM_TOKEN (or GLM_CLIENT_ID/GLM_CLIENT_SECRET) instead of logging in")
			}
			timeout, _ := cmd.Flags().GetDuration("timeout")

			var tok *oauth.Token
			switch res.Profile.Auth {
			case config.AuthOAuth:
				cfg, err := oauthConfigFor(res)
				if err != nil {
					return err
				}
				if cfg.ClientID == "" {
					return fmt.Errorf("profile %q has no client_id — re-add it: glm profile add %s --instance %s --auth oauth --client-id <id>", res.Name, res.Name, res.Profile.Instance)
				}
				cfg.Notify = func(msg string) { fmt.Fprintln(cmd.ErrOrStderr(), msg) }
				tok, err = runOAuthLogin(cmd.Context(), cfg)
				if err != nil {
					return err
				}
			case config.AuthClientCredentials:
				cfg, err := oauthConfigFor(res)
				if err != nil {
					return err
				}
				tok, err = oauth.ClientCredentials(cmd.Context(), cfg)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("profile %q uses basic auth — there is no login step (verify with: glm profile test %s)", res.Name, res.Name)
			}

			if err := saveStoredToken(res.Name, tok); err != nil {
				return fmt.Errorf("signed in, but storing the token failed: %w", err)
			}

			// O10: resolve who the token actually is and store it on the
			// profile — it feeds the W7 identity line and the per-user
			// schema cache key. Identity failure does not undo the login.
			host := strings.TrimPrefix(res.Profile.Instance, "https://")
			userName, display, ierr := tokenIdentity(cmd.Context(), res, tok.AccessToken, timeout)
			if ierr != nil || userName == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "signed in @ %s (profile %s) — identity not visible\n", host, res.Name)
				return nil
			}
			if userName != res.Profile.Username {
				if f, err := config.Load(); err == nil {
					if p, ok := f.Profiles[res.Name]; ok {
						p.Username = userName
						f.Profiles[res.Name] = p
						_ = f.Save()
					}
				}
			}
			id := userName
			if display != "" && display != userName {
				id = fmt.Sprintf("%s (%s)", userName, display)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed in as %s @ %s (profile %s)\n", id, host, res.Name)
			return nil
		},
	}
}

// tokenIdentity asks the instance who an access token belongs to
// (DESIGN-OAUTH.md O10).
func tokenIdentity(ctx context.Context, res *config.Resolved, accessToken string, timeout time.Duration) (string, string, error) {
	client, err := snow.NewBearer(res.Profile.Instance, accessToken, "login", timeout)
	if err != nil {
		return "", "", err
	}
	q := url.Values{}
	q.Set("sysparm_query", "sys_id=javascript:gs.getUserID()")
	q.Set("sysparm_fields", "user_name,name")
	q.Set("sysparm_limit", "1")
	q.Set("sysparm_display_value", "true")
	q.Set("sysparm_exclude_reference_link", "true")
	rows, err := client.Table(ctx, "sys_user", q)
	if err != nil || len(rows) == 0 {
		return "", "", err
	}
	return field(rows[0], "user_name"), field(rows[0], "name"), nil
}

func newProfileLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [name]",
		Short: "Discard a profile's stored OAuth tokens",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			res, err := resolveProfile(cmd, name)
			if err != nil {
				return err
			}
			if res.Name == config.EnvProfileName {
				return fmt.Errorf("the env profile stores no tokens — unset GLM_TOKEN/GLM_CLIENT_SECRET instead")
			}
			if err := deleteStoredToken(res.Name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q signed out (stored tokens removed; the client secret, if any, is kept — `glm profile remove` deletes everything)\n", res.Name)
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
			// Every keyring entry goes with the profile: password, OAuth
			// tokens, client secret. The delete helpers already treat a
			// missing entry as success, so any error here is a REAL keyring
			// failure — and printing success while credentials linger would
			// break remove's promise, so it is reported, not swallowed.
			var kerrs []error
			if err := deleteStoredToken(name); err != nil {
				kerrs = append(kerrs, err)
			}
			if err := deleteClientSecret(name); err != nil {
				kerrs = append(kerrs, err)
			}
			if err := deletePassword(name); err != nil {
				kerrs = append(kerrs, err)
			}
			if len(kerrs) > 0 {
				return fmt.Errorf("profile removed from config, but deleting its keyring material failed: %w", errors.Join(kerrs...))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q removed\n", name)
			return nil
		},
	}
}
