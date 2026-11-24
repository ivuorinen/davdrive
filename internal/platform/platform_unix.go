//go:build darwin || linux

// Package platform runs the operating system's NFS mount tools.
package platform

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DetachAttr starts a child in its own session, so closing the terminal or
// pressing Ctrl-C in it does not reach the child.
func DetachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// Canonical resolves symlinks in the parent of mountpoint, giving the path
// the mount table records (/tmp is /private/tmp on macOS). The mountpoint
// itself is never touched: stat on a dead NFS mount blocks.
func Canonical(mountpoint string) string {
	parent, err := filepath.EvalSymlinks(filepath.Dir(mountpoint))
	if err != nil {
		return mountpoint
	}

	return filepath.Join(parent, filepath.Base(mountpoint))
}

// run runs a mount tool and folds its output into the error. name is always
// a fixed tool (mount_nfs, mount, umount, diskutil, sudo) and the arguments
// are argv, never a shell line, so neither the mountpoint nor the user's
// mount_options can inject a command. sudo still prompts: it reads the
// password from /dev/tty, not from stdin.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: fixed tool, argv only; see above
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s: %s", strings.Join(cmd.Args, " "), msg)
	}

	return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
}

// unescapeMount decodes the octal escapes (\040 for a space) the kernel
// writes in /proc/self/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3

				continue
			}
		}
		b.WriteByte(s[i])
	}

	return b.String()
}
