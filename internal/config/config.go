// Package config loads the mount definitions and resolves the paths and
// secrets that go with them.
package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/term"
)

const (
	appName = "davdrive"

	// EnvConfig replaces the default config file path; --config still
	// overrides it.
	EnvConfig = "DAVDRIVE_CONFIG"
	// EnvPassword names the environment variable that supplies the WebDAV
	// password without a prompt or command. It is a variable name, not a
	// credential.
	EnvPassword = "DAVDRIVE_PASSWORD"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Config is the parsed config file.
type Config struct {
	Path   string            `toml:"-"`
	Mounts map[string]*Mount `toml:"mounts"`
}

// Mount is one WebDAV share and where it is mounted.
type Mount struct {
	Name            string   `toml:"-"`
	URL             string   `toml:"url"`
	User            string   `toml:"user"`
	PasswordCommand string   `toml:"password_command"`
	Mountpoint      string   `toml:"mountpoint"`
	MountOptions    []string `toml:"mount_options"`
	// VFS holds rclone VFS options by their config names, for example
	// vfs_cache_max_size. They override the defaults set by the server.
	VFS map[string]any `toml:"vfs"`
}

// DefaultPath is $DAVDRIVE_CONFIG, else $XDG_CONFIG_HOME/davdrive/config.toml.
func DefaultPath() string {
	if p := os.Getenv(EnvConfig); p != "" {
		return p
	}

	return filepath.Join(xdgDir("XDG_CONFIG_HOME", ".config"), appName, "config.toml")
}

// StateDir holds the control sockets, lock files and logs.
func StateDir() string {
	return filepath.Join(xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")), appName)
}

// CacheDir holds the rclone VFS cache.
func CacheDir() string {
	return filepath.Join(xdgDir("XDG_CACHE_HOME", ".cache"), appName)
}

// CheckDirs fails when the state and cache directories cannot be placed:
// with $HOME unset and no absolute XDG_STATE_HOME or XDG_CACHE_HOME, they
// would resolve against the working directory, scattering sockets, logs
// and caches by where each command ran so that commands lose track of
// their own daemons.
func CheckDirs() error {
	for _, dir := range []string{StateDir(), CacheDir()} {
		if !filepath.IsAbs(dir) {
			return errors.New(
				"cannot determine the home directory: set HOME, or absolute XDG_STATE_HOME and XDG_CACHE_HOME",
			)
		}
	}

	return nil
}

// xdgDir follows the XDG base directory spec on every platform: a relative
// value is invalid and falls back to the default under $HOME. Without a
// home directory the result is relative; CheckDirs rejects that for the
// state and cache directories.
func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fallback
	}

	return filepath.Join(home, fallback)
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	cfg := &Config{Path: path}
	md, err := toml.DecodeFile(path, cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config file %s does not exist; see the README for an example", path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}

		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	for name, m := range cfg.Mounts {
		m.Name = name
		if err := m.validate(); err != nil {
			return nil, fmt.Errorf("%s: mounts.%s: %w", path, name, err)
		}
	}

	return cfg, nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("url %q is not an http(s) URL", raw)
	}
	// Basic auth over http hands the password to everyone on the path;
	// only a server on this machine is exempt.
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("use https:// for %s: http sends the password in cleartext", u.Host)
	}

	return nil
}

// resolveMountpoint expands ~ and requires an absolute result: a relative
// path would resolve against each command's working directory, and two
// commands run from different places would mount the same server twice.
func resolveMountpoint(p string) (string, error) {
	if p == "" {
		return "", errors.New("mountpoint is required")
	}
	mp, err := expandHome(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(mp) {
		return "", fmt.Errorf("mountpoint %q must be absolute or start with ~/", p)
	}

	return filepath.Clean(mp), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, p[1:]), nil
}

// Names lists the configured mounts, sorted.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Mounts))
	for name := range c.Mounts {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// Mount returns the named mount. An empty name selects the only configured
// mount.
func (c *Config) Mount(name string) (*Mount, error) {
	if name != "" {
		if m, ok := c.Mounts[name]; ok {
			return m, nil
		}

		return nil, fmt.Errorf("no mount %q in %s (configured: %s)", name, c.Path, strings.Join(c.Names(), ", "))
	}
	switch len(c.Mounts) {
	case 0:
		return nil, fmt.Errorf("no mounts defined in %s", c.Path)
	case 1:
		return c.Mounts[c.Names()[0]], nil
	default:
		return nil, fmt.Errorf("several mounts configured, name one of: %s", strings.Join(c.Names(), ", "))
	}
}

// SocketPath is the daemon's control socket.
func (m *Mount) SocketPath() string { return filepath.Join(StateDir(), m.Name+".sock") }

// LogPath is the daemon's log file.
func (m *Mount) LogPath() string { return filepath.Join(StateDir(), m.Name+".log") }

// LockPath is the file the daemon holds a lock on while it runs.
func (m *Mount) LockPath() string { return filepath.Join(StateDir(), m.Name+".lock") }

// Password resolves the WebDAV password: $DAVDRIVE_PASSWORD, then
// password_command, then a prompt when stdin is a terminal.
func (m *Mount) Password(ctx context.Context) (string, error) {
	if m.User == "" {
		return "", nil
	}
	if pw, ok := os.LookupEnv(EnvPassword); ok {
		return pw, nil
	}
	if m.PasswordCommand != "" {
		// password_command is the user's own shell command from their own
		// config file, run as them through /bin/sh, as trusted as rclone's
		// --password-command.
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", m.PasswordCommand) //nolint:gosec // G204: see above
		cmd.Stdin = os.Stdin
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("password_command: %w", err)
		}
		pw := strings.TrimRight(string(out), "\r\n")
		if pw == "" {
			return "", errors.New("password_command printed nothing")
		}

		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("no password for %s: set password_command or %s", m.Name, EnvPassword)
	}
	fmt.Fprintf(os.Stderr, "Password for %s at %s: ", m.User, m.URL)
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	return string(pw), nil
}

func (m *Mount) validate() error {
	if !validName.MatchString(m.Name) {
		return errors.New("name may contain only letters, digits, '.', '_' and '-'")
	}
	if err := validateURL(m.URL); err != nil {
		return err
	}
	mp, err := resolveMountpoint(m.Mountpoint)
	if err != nil {
		return err
	}
	m.Mountpoint = mp

	return nil
}
