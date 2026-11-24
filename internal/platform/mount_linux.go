package platform

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Mount mounts the NFSv4 export served on 127.0.0.1:port at mountpoint.
// Linux allows NFS mounts only for root, so mount(8) runs through sudo
// unless the caller already is root.
func Mount(port int, mountpoint string, options []string) error {
	if !hasMountNFS() {
		return errors.New("mount.nfs not found: install nfs-common (Debian, Ubuntu, Pop!_OS)")
	}
	opts := append([]string{"vers=4.0", "proto=tcp", "port=" + strconv.Itoa(port)}, options...)
	argv := privileged(true, "mount", "-t", "nfs", "-o", strings.Join(opts, ","), "127.0.0.1:/", mountpoint)

	return run(argv[0], argv[1:]...)
}

func hasMountNFS() bool {
	for _, p := range []string{"/sbin/mount.nfs", "/usr/sbin/mount.nfs"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}

	return false
}

// Unmount unmounts mountpoint. force detaches it even while files are
// open. interactive=false makes sudo fail instead of prompting, for callers
// without a terminal.
func Unmount(mountpoint string, force, interactive bool) error {
	args := []string{"umount"}
	if force {
		args = append(args, "--force", "--lazy")
	}
	argv := privileged(interactive, append(args, mountpoint)...)

	return run(argv[0], argv[1:]...)
}

// privileged prefixes args with sudo unless the caller already is root.
func privileged(interactive bool, args ...string) []string {
	if os.Geteuid() == 0 {
		return args
	}
	if !interactive {
		args = append([]string{"-n"}, args...)
	}

	return append([]string{"sudo"}, args...)
}

// IsMounted reports whether mountpoint is in the mount table. Reading
// /proc/self/mounts never touches the mountpoint, so a dead NFS server
// cannot block it.
func IsMounted(mountpoint string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && unescapeMount(fields[1]) == mountpoint {
			return true, nil
		}
	}

	return false, nil
}
