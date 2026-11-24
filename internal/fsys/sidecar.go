package fsys

import (
	"hash/fnv"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/file"
)

// sidecarLimit caps the bytes held for all sidecar files together.
// AppleDouble files hold a few KiB of extended attributes each.
const sidecarLimit = 64 << 20

// IsSidecar reports whether name is a file macOS writes next to real files
// on filesystems without native extended attributes: AppleDouble "._*"
// files and Finder's ".DS_Store". These are kept in memory and never
// reach the remote. The list matches what macFUSE's noappledouble blocks.
func IsSidecar(name string) bool {
	base := path.Base(name)

	return strings.HasPrefix(base, "._") || base == ".DS_Store"
}

// sidecars is an in-memory store of sidecar files keyed by clean path.
// Every field of every sidecar is guarded by mu.
type sidecars struct {
	mu       sync.Mutex
	files    map[string]*sidecar
	size     int64
	uid, gid uint32
}

type sidecar struct {
	name  string
	data  []byte
	mode  os.FileMode
	mtime time.Time
}

func newSidecars(uid, gid uint32) *sidecars {
	return &sidecars{files: make(map[string]*sidecar), uid: uid, gid: gid}
}

func (s *sidecars) open(name string, flag int, perm os.FileMode) (billy.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[name]
	switch {
	case !ok && flag&os.O_CREATE == 0:
		return nil, os.ErrNotExist
	case ok && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		return nil, os.ErrExist
	case !ok:
		sc = &sidecar{name: name, mode: perm.Perm(), mtime: time.Now()}
		s.files[name] = sc
	case flag&os.O_TRUNC != 0:
		if err := s.resize(sc, 0); err != nil {
			return nil, err
		}
	}

	return &sidecarFile{s: s, sc: sc}, nil
}

func (s *sidecars) stat(name string) (os.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}

	return s.info(sc), nil
}

// info snapshots sc; the caller holds mu.
func (s *sidecars) info(sc *sidecar) os.FileInfo {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sc.name))

	return &sidecarInfo{
		name:  path.Base(sc.name),
		size:  int64(len(sc.data)),
		mode:  sc.mode,
		mtime: sc.mtime,
		// rclone's VFS numbers inodes upwards from 1; the high bit keeps
		// sidecar file ids clear of them.
		sys: &file.FileInfo{Nlink: 1, UID: s.uid, GID: s.gid, Fileid: h.Sum64() | 1<<63},
	}
}

// list returns the sidecars directly inside dir. An AppleDouble file
// ("._name") whose name is no longer in the directory, because it was
// deleted or renamed outside this mount, is dropped from the store instead
// of listed: it would otherwise linger in listings and count against
// sidecarLimit until the daemon restarts. exists reports whether a name is
// in the directory's remote listing.
func (s *sidecars) list(dir string, exists func(name string) bool) []os.FileInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []os.FileInfo
	for name, sc := range s.files {
		if parent(name) != dir {
			continue
		}
		if owner, ok := strings.CutPrefix(path.Base(name), "._"); ok && !exists(owner) {
			s.size -= int64(len(sc.data))
			delete(s.files, name)

			continue
		}
		out = append(out, s.info(sc))
	}

	return out
}

// hasChildren reports whether any sidecar lives below dir.
func (s *sidecars) hasChildren(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := dir + "/"
	for name := range s.files {
		if dir == "" || strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

func (s *sidecars) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[name]
	if !ok {
		return os.ErrNotExist
	}
	s.size -= int64(len(sc.data))
	delete(s.files, name)

	return nil
}

func (s *sidecars) rename(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[from]
	if !ok {
		return os.ErrNotExist
	}
	if old, ok := s.files[to]; ok && old != sc {
		s.size -= int64(len(old.data))
	}
	delete(s.files, from)
	sc.name = to
	s.files[to] = sc

	return nil
}

// renameDir carries the sidecars below a renamed directory along with it.
func (s *sidecars) renameDir(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := from + "/"
	for name, sc := range s.files {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			delete(s.files, name)
			sc.name = to + "/" + rest
			s.files[sc.name] = sc
		}
	}
}

func (s *sidecars) chtimes(name string, mtime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[name]
	if !ok {
		return os.ErrNotExist
	}
	sc.mtime = mtime

	return nil
}

func (s *sidecars) chmod(name string, mode os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.files[name]
	if !ok {
		return os.ErrNotExist
	}
	sc.mode = mode.Perm()

	return nil
}

// resize sets the length of sc's data, zero-filling growth; the caller
// holds mu.
func (s *sidecars) resize(sc *sidecar, size int64) error {
	old := int64(len(sc.data))
	if s.size+size-old > sidecarLimit {
		return syscall.ENOSPC
	}
	if size > int64(cap(sc.data)) {
		grown := make([]byte, size)
		copy(grown, sc.data)
		sc.data = grown
	} else {
		sc.data = sc.data[:size]
		if size > old {
			clear(sc.data[old:])
		}
	}
	s.size += size - old
	sc.mtime = time.Now()

	return nil
}

// sidecarFile is an open sidecar. Its offset is guarded by s.mu.
type sidecarFile struct {
	s   *sidecars
	sc  *sidecar
	off int64
}

func (f *sidecarFile) Name() string {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()

	return f.sc.name
}

func (f *sidecarFile) Read(p []byte) (int, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	n, err := f.readAt(p, f.off)
	f.off += int64(n)

	return n, err
}

func (f *sidecarFile) ReadAt(p []byte, off int64) (int, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()

	return f.readAt(p, off)
}

func (f *sidecarFile) Write(p []byte) (int, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if end := f.off + int64(len(p)); end > int64(len(f.sc.data)) {
		if err := f.s.resize(f.sc, end); err != nil {
			return 0, err
		}
	}
	copy(f.sc.data[f.off:], p)
	f.sc.mtime = time.Now()
	f.off += int64(len(p))

	return len(p), nil
}

func (f *sidecarFile) Seek(offset int64, whence int) (int64, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += int64(len(f.sc.data))
	default:
		return 0, os.ErrInvalid
	}
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	f.off = offset

	return offset, nil
}

func (f *sidecarFile) Truncate(size int64) error {
	if size < 0 {
		return os.ErrInvalid
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()

	return f.s.resize(f.sc, size)
}

func (f *sidecarFile) Close() error  { return nil }
func (f *sidecarFile) Lock() error   { return nil }
func (f *sidecarFile) Unlock() error { return nil }

func (f *sidecarFile) readAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, os.ErrInvalid
	}
	if off >= int64(len(f.sc.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.sc.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

type sidecarInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	sys   *file.FileInfo
}

func (i *sidecarInfo) Name() string       { return i.name }
func (i *sidecarInfo) Size() int64        { return i.size }
func (i *sidecarInfo) Mode() os.FileMode  { return i.mode }
func (i *sidecarInfo) ModTime() time.Time { return i.mtime }
func (i *sidecarInfo) IsDir() bool        { return false }
func (i *sidecarInfo) Sys() any           { return i.sys }
