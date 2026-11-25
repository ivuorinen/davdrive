package fsys

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/rclone/rclone/vfs/vfscommon"
	nfs "github.com/willscott/go-nfs"
)

// xattrs is the content the tests write to AppleDouble files.
const xattrs = "xattrs"

// createSidecar creates name in f holding data and returns the open handle.
func createSidecar(t *testing.T, f *FS, name, data string) billy.File {
	t.Helper()
	h, err := f.Create(name)
	must(t, err)
	_, err = h.Write([]byte(data))
	must(t, err)

	return h
}

// wantRead fails unless read returns exactly data and err.
func wantRead(t *testing.T, call string, read func([]byte) (int, error), data string, want error) {
	t.Helper()
	buf := make([]byte, 8)
	n, err := read(buf)
	if string(buf[:n]) != data || !errors.Is(err, want) {
		t.Fatalf("%s = %q, %v; want %q, %v", call, buf[:n], err, data, want)
	}
}

func TestSidecarOpenFlags(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)
	createSidecar(t, f, sideA, xattrs)

	tests := []struct {
		name string
		path string
		flag int
		err  error         // wanted with errors.Is, when code is zero
		code nfs.NFSStatus // wanted NFS status
		data string        // content after a successful open
	}{
		{name: "missing without create", path: "._b", flag: os.O_RDONLY, err: os.ErrNotExist},
		{name: "exclusive on existing", path: sideA, flag: os.O_RDWR | os.O_CREATE | os.O_EXCL, err: os.ErrExist},
		{name: "create in missing dir", path: "nodir/._b", flag: os.O_RDWR | os.O_CREATE, err: os.ErrNotExist},
		{name: "create below a file", path: fileA + "/._b", flag: os.O_RDWR | os.O_CREATE, code: nfs.NFSStatusNotDir},
		{name: "open existing", path: sideA, flag: os.O_RDWR, data: xattrs},
		{name: "create existing", path: sideA, flag: os.O_RDWR | os.O_CREATE, data: xattrs},
		{name: "truncate existing", path: sideA, flag: os.O_RDWR | os.O_TRUNC, data: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := f.OpenFile(tt.path, tt.flag, 0o644)
			switch {
			case tt.code != 0:
				wantStatus(t, err, tt.code)
			case tt.err != nil:
				wantIs(t, err, tt.err)
			default:
				must(t, err)
				must(t, h.Close())
				if got := readFS(t, f, tt.path); got != tt.data {
					t.Fatalf("after open, %s = %q, want %q", tt.path, got, tt.data)
				}
			}
		})
	}
	if remoteExists(t, dir, sideA) {
		t.Fatal("sidecar reached the remote")
	}
}

func TestSidecarSeek(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	h := createSidecar(t, f, "._x", "hello")

	seeks := []struct {
		offset int64
		whence int
		want   int64
		err    error
	}{
		{0, io.SeekStart, 0, nil},
		{2, io.SeekCurrent, 2, nil},
		{-2, io.SeekEnd, 3, nil},
		{0, 99, 0, os.ErrInvalid},
		{-1, io.SeekStart, 0, os.ErrInvalid},
	}
	for _, s := range seeks {
		got, err := h.Seek(s.offset, s.whence)
		if !errors.Is(err, s.err) || got != s.want {
			t.Fatalf("Seek(%d, %d) = %d, %v; want %d, %v", s.offset, s.whence, got, err, s.want, s.err)
		}
	}
	// The failed seeks left the offset at 3.
	wantRead(t, "Read at 3", h.Read, "lo", io.EOF)
	wantRead(t, "Read at the end", h.Read, "", io.EOF)
}

func TestSidecarFileIO(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "x"), "x")
	f := newTestFS(t, dir)
	h := createSidecar(t, f, "/d/._x", "hello")

	if got := h.Name(); got != "d/._x" {
		t.Fatalf("Name() = %q, want d/._x", got)
	}
	_, err := h.Seek(0, io.SeekStart)
	must(t, err)
	_, err = h.Write([]byte("J"))
	must(t, err)

	wantRead(t, "ReadAt(0)", func(p []byte) (int, error) { return h.ReadAt(p[:5], 0) }, "Jello", nil)
	wantRead(t, "ReadAt(-1)", func(p []byte) (int, error) { return h.ReadAt(p, -1) }, "", os.ErrInvalid)
	wantRead(t, "ReadAt(9)", func(p []byte) (int, error) { return h.ReadAt(p, 9) }, "", io.EOF)
	wantIs(t, h.Truncate(-1), os.ErrInvalid)
	must(t, h.Lock())
	must(t, h.Unlock())
	must(t, h.Close())
	if got := readFS(t, f, "d/._x"); got != "Jello" {
		t.Fatalf("stored sidecar = %q, want Jello", got)
	}
}

func TestSidecarInfo(t *testing.T) {
	f := newTestFSWith(t, t.TempDir(), func(o *vfscommon.Options) { o.UID, o.GID = 501, 20 })
	before := time.Now()
	createSidecar(t, f, "._a", "abc")
	createSidecar(t, f, "._b", "")
	after := time.Now()

	info, err := f.Stat("._a")
	must(t, err)
	if info.Name() != "._a" || info.Size() != 3 || info.Mode() != 0o666 || info.IsDir() {
		t.Fatalf("Stat(._a) = %s %d %v dir=%v; want ._a 3 -rw-rw-rw- false",
			info.Name(), info.Size(), info.Mode(), info.IsDir())
	}
	if mt := info.ModTime(); mt.Before(before) || mt.After(after) {
		t.Fatalf("ModTime() = %v, want between %v and %v", mt, before, after)
	}
	sys := wantSys(t, info, 501, 20)
	if sys.Fileid&(1<<63) == 0 {
		t.Fatalf("file id %#x lacks the high bit that keeps it clear of VFS inodes", sys.Fileid)
	}
	other, err := f.Stat("._b")
	must(t, err)
	if otherSys := wantSys(t, other, 501, 20); otherSys.Fileid == sys.Fileid {
		t.Fatalf("._a and ._b share file id %#x", sys.Fileid)
	}
}

// TestSidecarLimit fills the sidecar store and checks that the space comes
// back when a sidecar is truncated or removed.
func TestSidecarLimit(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	big := createSidecar(t, f, "._big", "")
	small := createSidecar(t, f, "._small", "")

	wantIs(t, big.Truncate(sidecarLimit+1), syscall.ENOSPC)
	_, err := big.Seek(sidecarLimit, io.SeekStart)
	must(t, err)
	if n, err := big.Write([]byte("x")); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Write past the limit = %d, %v; want 0, ENOSPC", n, err)
	}

	must(t, big.Truncate(sidecarLimit))
	_, err = small.Write([]byte("x"))
	wantIs(t, err, syscall.ENOSPC)
	_, err = f.OpenFile("._big", os.O_RDWR|os.O_TRUNC, 0)
	must(t, err)
	_, err = small.Write([]byte("x"))
	must(t, err)

	// ._small holds one byte now: this fills the store again.
	must(t, big.Truncate(sidecarLimit-1))
	_, err = small.Write([]byte("x"))
	wantIs(t, err, syscall.ENOSPC)
	must(t, f.Remove("._big"))
	_, err = small.Write([]byte("x"))
	must(t, err)
}
