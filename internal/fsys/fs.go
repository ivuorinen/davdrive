// Package fsys exposes an rclone VFS to go-nfs as a billy filesystem.
//
// It follows rclone's own adapter (cmd/serve/nfs/filesystem.go) and adds
// two things: sidecar files (see IsSidecar) are kept in memory instead of
// being uploaded, and open handles are serialized per file (see
// pathLocks), because overlapping open-write-close cycles on one file lose
// writes in rclone's VFS cache.
package fsys

import (
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/file"
)

// FS is a billy.Filesystem and billy.Change over an rclone VFS.
type FS struct {
	vfs      *vfs.VFS
	side     *sidecars
	locks    pathLocks
	uid, gid uint32
	writable bool
}

var (
	_ billy.Filesystem = (*FS)(nil)
	_ billy.Change     = (*FS)(nil)
	_ billy.Capable    = (*FS)(nil)
)

// New wraps v. Writes need a VFS cache mode other than off: NFS writes at
// arbitrary offsets, which only the cache can turn into whole-file uploads.
func New(v *vfs.VFS) *FS {
	return &FS{
		vfs:      v,
		side:     newSidecars(v.Opt.UID, v.Opt.GID),
		uid:      v.Opt.UID,
		gid:      v.Opt.GID,
		writable: !v.Opt.ReadOnly && v.Opt.CacheMode != vfscommon.CacheModeOff,
	}
}

// Writable reports whether the filesystem accepts changes.
func (f *FS) Writable() bool { return f.writable }

// clean turns a go-nfs path ("/" or "a/b") into a sidecar key ("" or "a/b").
func clean(name string) string { return strings.Trim(path.Clean("/"+name), "/") }

// parent is the directory of a clean path.
func parent(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}

	return ""
}

// mapErr turns VFS errors that go-nfs does not recognize into NFS status
// codes; go-nfs reports anything unknown as an I/O error.
func mapErr(err error) error {
	var code nfs.NFSStatus
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vfs.ENOTEMPTY):
		code = nfs.NFSStatusNotEmpty
	case errors.Is(err, vfs.EROFS):
		code = nfs.NFSStatusROFS
	case errors.Is(err, vfs.ENOSYS):
		code = nfs.NFSStatusNotSupp
	case errors.Is(err, os.ErrInvalid):
		code = nfs.NFSStatusInval
	default:
		return err
	}

	return &nfs.NFSStatusError{NFSStatus: code, WrappedErr: err}
}

// Create creates or truncates name.
func (f *FS) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Open opens name for reading.
func (f *FS) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile opens name with flag.
func (f *FS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if IsSidecar(name) {
		key := clean(name)
		if flag&os.O_CREATE != 0 {
			if err := f.checkDir(parent(key)); err != nil {
				return nil, err
			}
		}

		return f.side.open(key, flag, perm)
	}
	unlock := f.locks.lock(clean(name))
	h, err := f.vfs.OpenFile(name, flag, perm)
	if err != nil {
		unlock()

		return nil, mapErr(err)
	}

	return &lockedFile{File: h, unlock: unlock}, nil
}

// Stat describes name.
func (f *FS) Stat(name string) (os.FileInfo, error) {
	if IsSidecar(name) {
		return f.side.stat(clean(name))
	}
	n, err := f.vfs.Stat(name)
	if err != nil {
		return nil, mapErr(err)
	}
	f.setSys(n)

	return n, nil
}

// Lstat is Stat: the VFS resolves links itself.
func (f *FS) Lstat(name string) (os.FileInfo, error) { return f.Stat(name) }

// ReadDir lists dir, sorted by name. Sidecar files already on the remote
// (uploaded by an earlier client) are hidden: the in-memory ones replace
// them.
func (f *FS) ReadDir(dir string) ([]os.FileInfo, error) {
	entries, err := f.vfs.ReadDir(dir)
	if err != nil {
		return nil, mapErr(err)
	}
	out := entries[:0]
	for _, e := range entries {
		if IsSidecar(e.Name()) {
			continue
		}
		if n, ok := e.(vfs.Node); ok {
			f.setSys(n)
		}
		out = append(out, e)
	}
	names := make(map[string]bool, len(out))
	for _, e := range out {
		names[e.Name()] = true
	}
	if side := f.side.list(clean(dir), func(name string) bool { return names[name] }); len(side) > 0 {
		out = append(out, side...)
		slices.SortFunc(out, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
	}

	return out, nil
}

// Rename moves from to to. A sidecar can only be renamed to another
// sidecar name: one side lives in memory, the other on the remote.
func (f *FS) Rename(from, to string) error {
	fromSide, toSide := IsSidecar(from), IsSidecar(to)
	switch {
	case fromSide && toSide:
		dst := clean(to)
		if err := f.checkDir(parent(dst)); err != nil {
			return err
		}

		return f.side.rename(clean(from), dst)
	case fromSide || toSide:
		return os.ErrPermission
	}
	if err := f.vfs.Rename(from, to); err != nil {
		return mapErr(err)
	}
	f.side.renameDir(clean(from), clean(to))

	return nil
}

// Remove deletes a file or an empty directory.
func (f *FS) Remove(name string) error {
	key := clean(name)
	if IsSidecar(name) {
		return f.side.remove(key)
	}
	if f.side.hasChildren(key) {
		return mapErr(vfs.ENOTEMPTY)
	}
	err := f.vfs.Remove(name)
	if errors.Is(err, vfs.ENOTEMPTY) && f.removeRemoteSidecars(name) {
		err = f.vfs.Remove(name)
	}

	return mapErr(err)
}

// Join joins path elements.
func (f *FS) Join(elem ...string) string { return path.Join(elem...) }

// TempFile is not supported.
func (f *FS) TempFile(string, string) (billy.File, error) { return nil, billy.ErrNotSupported }

// MkdirAll creates name and any missing parents. A file in the way is
// os.ErrExist: NFSv4 CREATE relies on this to refuse a taken name.
func (f *FS) MkdirAll(name string, perm os.FileMode) error {
	if IsSidecar(name) {
		return os.ErrPermission
	}
	parts := strings.Split(clean(name), "/")
	for i := range parts {
		dir := strings.Join(parts[:i+1], "/")
		n, err := f.vfs.Stat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			err = f.vfs.Mkdir(dir, perm)
		case err == nil && !n.IsDir():
			err = os.ErrExist
		}
		if err != nil {
			return mapErr(err)
		}
	}

	return nil
}

// Symlink creates link pointing at target, if the VFS has links enabled.
func (f *FS) Symlink(target, link string) error {
	if IsSidecar(link) {
		return os.ErrPermission
	}

	return mapErr(f.vfs.Symlink(target, link))
}

// Readlink returns the target of link.
func (f *FS) Readlink(link string) (string, error) {
	target, err := f.vfs.Readlink(link)

	return target, mapErr(err)
}

// Chroot is not supported.
func (f *FS) Chroot(string) (billy.Filesystem, error) { return nil, billy.ErrNotSupported }

// Root is the export root.
func (f *FS) Root() string { return "/" }

// Capabilities reports read-only unless the VFS can take writes.
func (f *FS) Capabilities() billy.Capability {
	if !f.writable {
		return billy.ReadCapability | billy.SeekCapability
	}

	return billy.WriteCapability | billy.ReadCapability | billy.ReadAndWriteCapability |
		billy.SeekCapability | billy.TruncateCapability
}

// Chmod sets a sidecar's mode. The remote keeps no modes, so for other
// files it succeeds without effect, as in rclone's adapter.
func (f *FS) Chmod(name string, mode os.FileMode) error {
	if IsSidecar(name) {
		return f.side.chmod(clean(name), mode)
	}

	return ignoreENOSYS(f.vfs.Chmod(name, mode))
}

// Lchown is Chown.
func (f *FS) Lchown(name string, uid, gid int) error { return f.Chown(name, uid, gid) }

// Chown succeeds without effect: every file belongs to the mounting user.
func (f *FS) Chown(name string, uid, gid int) error {
	if IsSidecar(name) {
		_, err := f.side.stat(clean(name))

		return err
	}

	return ignoreENOSYS(f.vfs.Chown(name, uid, gid))
}

// Chtimes sets the modification time.
func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	if IsSidecar(name) {
		return f.side.chtimes(clean(name), mtime)
	}

	return mapErr(f.vfs.Chtimes(name, atime, mtime))
}

func ignoreENOSYS(err error) error {
	if errors.Is(err, vfs.ENOSYS) {
		return nil
	}

	return mapErr(err)
}

// setSys gives a node the owner and file id go-nfs reports; without a
// file id Linux refuses the mount.
func (f *FS) setSys(n vfs.Node) {
	if _, ok := n.Sys().(*file.FileInfo); ok {
		return
	}
	n.SetSys(&file.FileInfo{Nlink: 1, UID: f.uid, GID: f.gid, Fileid: n.Inode()})
}

// checkDir fails unless dir is an existing directory on the remote.
func (f *FS) checkDir(dir string) error {
	n, err := f.vfs.Stat(dir)
	if err != nil {
		return mapErr(err)
	}
	if !n.IsDir() {
		return &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusNotDir, WrappedErr: os.ErrInvalid}
	}

	return nil
}

// removeRemoteSidecars deletes the hidden sidecar files an earlier client
// uploaded to dir, so that a directory that looks empty can be removed. It
// does nothing, and reports false, if dir holds anything else.
func (f *FS) removeRemoteSidecars(dir string) bool {
	entries, err := f.vfs.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !IsSidecar(e.Name()) {
			return false
		}
	}
	for _, e := range entries {
		if err := f.vfs.Remove(path.Join(dir, e.Name())); err != nil {
			return false
		}
	}

	return true
}
