package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
