package platform

import (
	"strings"
	"testing"
)

// A mountpoint whose parent does not exist makes each tool fail at once,
// before mount_nfs contacts a server or umount touches a mount.
const missingMountpoint = "/nonexistent-davdrive-parent/kDrive"

// TestMountToolErrors checks the command lines Mount and Unmount run, the
// user's mount_options included, and that the tool's message reaches the
// error.
func TestMountToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			"mount",
			Mount(2049, missingMountpoint, []string{"soft", "timeo=10"}),
			"/sbin/mount_nfs -o vers=4,port=2049,soft,timeo=10 127.0.0.1:/ " + missingMountpoint + ": mount_nfs: ",
		},
		{"unmount", Unmount(missingMountpoint, false, true), "/sbin/umount " + missingMountpoint + ": "},
		{
			"forced unmount",
			Unmount(missingMountpoint, true, false),
			"/usr/sbin/diskutil unmount force " + missingMountpoint + ": ",
		},
	} {
		if tc.err == nil || !strings.HasPrefix(tc.err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one starting %q", tc.name, tc.err, tc.want)
		}
	}
}
