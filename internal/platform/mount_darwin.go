package platform

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Mount mounts the NFSv4 export served on 127.0.0.1:port at mountpoint.
// macOS lets the owner of the mountpoint mount NFS without root.
func Mount(port int, mountpoint string, options []string) error {
	opts := append([]string{"vers=4", "port=" + strconv.Itoa(port)}, options...)

	return run("/sbin/mount_nfs", "-o", strings.Join(opts, ","), "127.0.0.1:/", mountpoint)
}

// Unmount unmounts mountpoint. force detaches it even while files are open.
// macOS needs no root to unmount a mount the user made, so interactive is
// unused here.
func Unmount(mountpoint string, force, _ bool) error {
	if force {
		return run("/usr/sbin/diskutil", "unmount", "force", mountpoint)
	}

	return run("/sbin/umount", mountpoint)
}

// IsMounted reports whether mountpoint is in the mount table. MNT_NOWAIT
// keeps a dead NFS server from blocking the call.
func IsMounted(mountpoint string) (bool, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return false, err
	}
	buf := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return false, err
	}
	for i := range buf[:n] {
		if unix.ByteSliceToString(buf[i].Mntonname[:]) == mountpoint {
			return true, nil
		}
	}

	return false, nil
}
