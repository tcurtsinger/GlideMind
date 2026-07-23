// Package config loads and resolves glm profiles. Secrets never live in the
// config file — they belong to the secret package or GLM_* env vars.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
)

// Env vars honored during profile resolution.
const (
	EnvProfile  = "GLM_PROFILE"
	EnvInstance = "GLM_INSTANCE"
	EnvUsername = "GLM_USERNAME"
)

// EnvProfileName is the synthetic profile name used when the connection is
// built entirely from GLM_* env vars (containers, CI).
const EnvProfileName = "env"

// Profile is one named instance connection.
type Profile struct {
	Instance string `toml:"instance"`
	Auth     string `toml:"auth,omitempty"` // "basic" (default); more methods later
	Username string `toml:"username,omitempty"`
}

// File is the on-disk config: %APPDATA%\glidemind\config.toml on Windows,
// XDG config dir elsewhere.
type File struct {
	Default  string             `toml:"default,omitempty"`
	Profiles map[string]Profile `toml:"profiles,omitempty"`
}

// Path returns the config file location.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "glidemind", "config.toml"), nil
}

// Load reads the config file; a missing file yields an empty config.
func Load() (*File, error) {
	f := &File{Profiles: map[string]Profile{}}
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if err := toml.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	return f, nil
}

// Save writes the config file, creating its directory if needed.
func (f *File) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := toml.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	// Write to a temp file in the same directory, then rename over the
	// target: an interrupted or concurrent write can no longer truncate or
	// corrupt the config — readers see either the old file or the new one.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

// Names returns profile names sorted for stable output.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolved is the chosen profile plus where it came from, for --verbose and
// error messages.
type Resolved struct {
	Name    string
	Source  string
	Profile Profile
}

// Resolve picks the active profile. Precedence:
//
//	--profile flag > GLM_PROFILE > GLM_INSTANCE (pure-env profile)
//	> config default > the only configured profile.
func Resolve(flagName string) (*Resolved, error) {
	name, source := flagName, "--profile flag"
	if name == "" {
		name, source = os.Getenv(EnvProfile), EnvProfile+" env"
	}

	if name == "" {
		if inst := os.Getenv(EnvInstance); inst != "" {
			return &Resolved{
				Name:   EnvProfileName,
				Source: EnvInstance + " env",
				Profile: Profile{
					Instance: inst,
					Auth:     "basic",
					Username: os.Getenv(EnvUsername),
				},
			}, nil
		}
	}

	f, err := Load()
	if err != nil {
		return nil, err
	}

	if name == "" {
		switch {
		case f.Default != "":
			name, source = f.Default, "config default"
		case len(f.Profiles) == 1:
			name, source = f.Names()[0], "only configured profile"
		default:
			return nil, fmt.Errorf("no profile selected — run `glm profile add <name> --instance <url> --username <user>`, pass --profile, or set %s/%s/%s", EnvInstance, EnvUsername, "GLM_PASSWORD")
		}
	}

	p, ok := f.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("profile %q not found (have: %v) — see `glm profile list`", name, f.Names())
	}
	// Credentials are env-overridable even for named profiles: the profile
	// picks the instance, GLM_USERNAME/GLM_PASSWORD may supply who — the
	// same rule the secret package applies to passwords.
	if u := os.Getenv(EnvUsername); u != "" {
		p.Username = u
	}
	return &Resolved{Name: name, Source: source, Profile: p}, nil
}
