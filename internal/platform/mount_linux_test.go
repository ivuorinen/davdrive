package platform

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A mountpoint whose parent does not exist makes each tool fail at once,
// before mount contacts a server or umount touches a mount.
const missingMountpoint = "/nonexistent-davdrive-parent/kDrive"

// TestPrivileged checks when sudo is used and when it may prompt: the
// daemon has no terminal, and a sudo waiting for a password there would
// hang its shutdown.
func TestPrivileged(t *testing.T) {
	args := []string{"umount", missingMountpoint}
	got := [2][]string{privileged(true, args...), privileged(false, args...)}
	want := [2][]string{append([]string{"sudo"}, args...), append([]string{"sudo", "-n"}, args...)}
	if os.Geteuid() == 0 {
		want = [2][]string{args, args}
	}
	for i := range got {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("privileged(interactive=%v) = %q, want %q", i == 0, got[i], want[i])
		}
	}
}

// TestUnmountError runs the non-interactive unmount the daemon uses: sudo
// -n fails instead of prompting, and the error names the command.
func TestUnmountError(t *testing.T) {
	err := Unmount(missingMountpoint, true, false)
	if err == nil || !strings.Contains(err.Error(), "umount --force --lazy "+missingMountpoint) {
		t.Errorf("Unmount = %v, want an error naming the umount command", err)
	}
}

// TestMountError checks the install hint without mount.nfs, and with it
// the error of a mount that cannot succeed. sudo may prompt in Mount, so
// that case runs only where it cannot: as root or with passwordless sudo.
func TestMountError(t *testing.T) {
	if !hasMountNFS() {
		if err := Mount(2049, missingMountpoint, nil); err == nil || !strings.Contains(err.Error(), "nfs-common") {
			t.Errorf("Mount without mount.nfs = %v, want the install hint", err)
		}

		return
	}
	if os.Geteuid() != 0 && exec.Command("sudo", "-n", "true").Run() != nil {
		t.Skip("mount.nfs is installed, and sudo would prompt for a password")
	}
	err := Mount(2049, missingMountpoint, []string{"soft"})
	if want := "mount -t nfs -o vers=4.0,proto=tcp,port=2049,soft 127.0.0.1:/ " + missingMountpoint; err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("Mount = %v, want an error naming %q", err, want)
	}
}
