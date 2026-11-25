//go:build darwin || linux

package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnescapeMount covers the octal escapes Linux writes in
// /proc/self/mounts. A wrong decode makes a mounted path with a space read
// as unmounted, and mount then mounts it a second time.
func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{
		`/home/me/kDrive`:            "/home/me/kDrive",
		`/home/me/my\040drive`:       "/home/me/my drive",
		`/a\011b\012c`:               "/a\tb\nc",
		`/back\134slash`:             `/back\slash`,
		`/ends\040`:                  "/ends ",
		`/trailing\`:                 `/trailing\`,
		`/short\04`:                  `/short\04`,
		`/not\9octal`:                `/not\9octal`,
		`/two\040spaces\040here`:     "/two spaces here",
		`/unicode\303\244ra\303\266`: "/unicodeäraö",
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonical checks the path compared against the mount table: a
// symlinked parent (/tmp on macOS) resolves, the mountpoint itself is kept
// as named, and a parent that cannot be resolved leaves the path as given.
func TestCanonical(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Canonical(filepath.Join(link, "kDrive")), filepath.Join(resolved, "kDrive"); got != want {
		t.Errorf("Canonical through a symlinked parent = %q, want %q", got, want)
	}
	if got := Canonical(missingMountpoint); got != missingMountpoint {
		t.Errorf("Canonical(%q) = %q, want it unchanged", missingMountpoint, got)
	}
}

// TestRun checks the errors users see when a mount tool fails: its own
// message when it printed one, the exit status otherwise, prefixed by the
// command line that failed.
func TestRun(t *testing.T) {
	if err := run("true"); err != nil {
		t.Errorf("run(true) = %v", err)
	}

	err := run("sh", "-c", "echo 'mount point busy' >&2; exit 3")
	if want := "sh -c echo 'mount point busy' >&2; exit 3: mount point busy"; err == nil || err.Error() != want {
		t.Errorf("run with output = %v, want %q", err, want)
	}

	err = run("false")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || err.Error() != "false: exit status 1" {
		t.Errorf("run(false) = %v, want the exit status of false", err)
	}

	if err := run("/nonexistent-davdrive-tool"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run of a missing tool = %v, want os.ErrNotExist", err)
	}
}

// TestDetachAttr checks that a child started with DetachAttr leads its own
// session, so the terminal's Ctrl-C and hangup do not reach the daemon.
func TestDetachAttr(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = DetachAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	sid, err := unix.Getsid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if sid != cmd.Process.Pid {
		t.Errorf("child session = %d, want its own (%d)", sid, cmd.Process.Pid)
	}
}

func TestIsMounted(t *testing.T) {
	if mounted, err := IsMounted("/"); err != nil || !mounted {
		t.Errorf("IsMounted(/) = %v, %v; want true", mounted, err)
	}
	dir := Canonical(t.TempDir())
	if mounted, err := IsMounted(dir); err != nil || mounted {
		t.Errorf("IsMounted(%s) = %v, %v; want false", dir, mounted, err)
	}
}
