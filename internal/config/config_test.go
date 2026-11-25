package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testURL = "https://dav.example"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

// mountTOML is one [mounts.<name>] table.
func mountTOML(name, url, mountpoint string) string {
	return "[mounts." + name + "]\nurl = \"" + url + "\"\nmountpoint = \"" + mountpoint + "\"\n"
}

func TestLoadValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const https = "https://dav.example"
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"https", mountTOML("k", https, "/mnt/k"), ""},
		{"http to loopback", mountTOML("k", "http://127.0.0.1:8080", "/mnt/k"), ""},
		{"http to localhost", mountTOML("k", "http://localhost", "/mnt/k"), ""},
		{"http to a remote host", mountTOML("k", "http://dav.example", "/mnt/k"), "use https://"},
		{"not http", mountTOML("k", "ftp://dav.example", "/mnt/k"), "not an http(s) URL"},
		{"unknown key", mountTOML("k", https, "/mnt/k") + "mountpiont = \"x\"\n", "unknown keys"},
		{"bad name", mountTOML(`"a b"`, https, "/mnt/k"), "name may contain only"},
		{"relative mountpoint", mountTOML("k", https, "kDrive"), "must be absolute"},
		{"missing mountpoint", "[mounts.k]\nurl = \"" + https + "\"\n", "mountpoint is required"},
	} {
		_, err := Load(writeConfig(t, tc.body))
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestLoadExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(writeConfig(t, "[mounts.k]\nurl = \"https://dav.example\"\nmountpoint = \"~/kDrive/\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Mounts["k"].Mountpoint, filepath.Join(home, "kDrive"); got != want {
		t.Fatalf("mountpoint = %q, want %q", got, want)
	}
}

func TestMountSelection(t *testing.T) {
	one, err := Load(writeConfig(t, "[mounts.a]\nurl = \"https://dav.example\"\nmountpoint = \"/mnt/a\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := one.Mount(""); err != nil || m.Name != "a" {
		t.Fatalf("Mount(\"\") with one mount = %v, %v; want a", m, err)
	}
	two, err := Load(writeConfig(t, "[mounts.a]\nurl = \"https://dav.example\"\nmountpoint = \"/mnt/a\"\n"+
		"[mounts.b]\nurl = \"https://dav.example\"\nmountpoint = \"/mnt/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := two.Mount(""); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("Mount(\"\") with two mounts = %v, want an error naming a, b", err)
	}
	if _, err := two.Mount("c"); err == nil {
		t.Fatal("Mount(\"c\") succeeded for an unconfigured name")
	}
}

func TestCheckDirs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	if err := CheckDirs(); err == nil {
		t.Fatal("CheckDirs succeeded with no home directory, so state would land in the working directory")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := CheckDirs(); err != nil {
		t.Fatalf("CheckDirs with absolute XDG directories = %v", err)
	}
}

// TestPathsFollowXDG checks where the config, the daemon's socket, log and
// lock, and the cache live: the locations the README documents.
func TestPathsFollowXDG(t *testing.T) {
	home, cfgHome, state, cache := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", cache)
	m := &Mount{Name: "k"}
	for _, tc := range []struct{ name, got, want string }{
		{"config", DefaultPath(), filepath.Join(cfgHome, "davdrive", "config.toml")},
		{"socket", m.SocketPath(), filepath.Join(state, "davdrive", "k.sock")},
		{"log", m.LogPath(), filepath.Join(state, "davdrive", "k.log")},
		{"lock", m.LockPath(), filepath.Join(state, "davdrive", "k.lock")},
		{"cache", CacheDir(), filepath.Join(cache, "davdrive")},
	} {
		if tc.got != tc.want {
			t.Errorf("%s path = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// The spec ignores a relative XDG value; the default under HOME applies.
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if got, want := DefaultPath(), filepath.Join(home, ".config", "davdrive", "config.toml"); got != want {
		t.Errorf("DefaultPath with a relative XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	t.Setenv(EnvConfig, "/etc/davdrive.toml")
	if got := DefaultPath(); got != "/etc/davdrive.toml" {
		t.Errorf("DefaultPath with %s set = %q", EnvConfig, got)
	}
}

func TestLoadErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.toml")
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), missing+" does not exist") {
		t.Errorf("Load of a missing file = %v, want it named as missing", err)
	}
	broken := writeConfig(t, "[mounts.k\n")
	if _, err := Load(broken); err == nil || !strings.HasPrefix(err.Error(), "reading "+broken+": ") {
		t.Errorf("Load of invalid TOML = %v, want a read error naming the file", err)
	}
	for _, raw := range []string{"https://", "http://[::1"} {
		if _, err := Load(writeConfig(t, mountTOML("k", raw, "/mnt/k"))); err == nil ||
			!strings.Contains(err.Error(), "not an http(s) URL") {
			t.Errorf("Load with url %q = %v, want it rejected", raw, err)
		}
	}
}

func TestMountpointHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(writeConfig(t, mountTOML("k", testURL, "~")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Mounts["k"].Mountpoint; got != home {
		t.Errorf("mountpoint ~ = %q, want %q", got, home)
	}

	t.Setenv("HOME", "")
	if _, err := Load(writeConfig(t, mountTOML("k", testURL, "~/kDrive"))); err == nil {
		t.Error("a ~/ mountpoint loaded with no home directory")
	}
}

func TestMountLookup(t *testing.T) {
	none, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := none.Mount(""); err == nil || !strings.Contains(err.Error(), "no mounts defined") {
		t.Errorf("Mount(\"\") with no mounts = %v", err)
	}
	one, err := Load(writeConfig(t, mountTOML("a", testURL, "/mnt/a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.Mount("b"); err == nil || err.Error() != `no mount "b" in `+one.Path+" (configured: a)" {
		t.Errorf("Mount(\"b\") = %v, want it to list the configured mounts", err)
	}
	if m, err := one.Mount("a"); err != nil || m.Name != "a" || m.Mountpoint != "/mnt/a" {
		t.Errorf("Mount(\"a\") = %v, %v; want mount a", m, err)
	}
}

// withStdin points os.Stdin at a file holding content for the test.
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		_ = f.Close()
	})
}

func TestPassword(t *testing.T) {
	for _, tc := range []struct {
		name, command, stdin string
		noUser               bool
		env                  *string
		want, wantErr        string
	}{
		{name: "no user needs none", noUser: true, command: "exit 1", want: ""},
		{name: "environment wins", command: "exit 1", env: new("from-env"), want: "from-env"},
		{name: "empty environment value counts", env: new(""), want: ""},
		{name: "command, newline trimmed", command: `printf 'secret\r\n'`, want: "secret"},
		{name: "command reads stdin", command: "head -n 1", stdin: "typed\n", want: "typed"},
		{name: "command fails", command: "exit 3", wantErr: "password_command: exit status 3"},
		{name: "command prints nothing", command: "true", wantErr: "password_command printed nothing"},
		{name: "no terminal", wantErr: "no password for k: set password_command or " + EnvPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvPassword, "")
			if tc.env == nil {
				_ = os.Unsetenv(EnvPassword)
			} else {
				t.Setenv(EnvPassword, *tc.env)
			}
			withStdin(t, tc.stdin)
			m := &Mount{Name: "k", URL: testURL, User: "me", PasswordCommand: tc.command}
			if tc.noUser {
				m.User = ""
			}
			got, err := m.Password(t.Context())
			switch {
			case tc.wantErr != "":
				if err == nil || err.Error() != tc.wantErr {
					t.Errorf("Password() error = %v, want %q", err, tc.wantErr)
				}
			case err != nil || got != tc.want:
				t.Errorf("Password() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
