package fsys

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	nfs "github.com/willscott/go-nfs"
)

// newTestFS serves dir through a local-backend VFS with the cache off, so
// the directory on disk is exactly what reached the remote.
func newTestFS(t *testing.T, dir string) *FS {
	t.Helper()
	ctx := context.Background()
	ri := fs.MustFind("local")
	f, err := ri.NewFs(ctx, "test", dir, fs.ConfigMap(ri.Prefix, ri.Options, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	v := vfs.New(ctx, f, &opt)
	t.Cleanup(v.Shutdown)

	return New(v)
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, infos []os.FileInfo) []string {
	t.Helper()
	out := make([]string, len(infos))
	for i, fi := range infos {
		out[i] = fi.Name()
	}

	return out
}

func TestSidecarStaysOffRemote(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "real")
	f := newTestFS(t, dir)

	file, err := f.OpenFile("._a.txt", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("xattrs")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "._a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar reached the remote: %v", err)
	}
	buf := make([]byte, 16)
	n, err := file.ReadAt(buf, 0)
	if !errors.Is(err, io.EOF) || string(buf[:n]) != "xattrs" {
		t.Fatalf("ReadAt = %q, %v", buf[:n], err)
	}
	entries, err := f.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, entries); len(got) != 2 || got[0] != "._a.txt" || got[1] != "a.txt" {
		t.Fatalf("ReadDir = %v, want [._a.txt a.txt]", got)
	}
}

func TestRemoteSidecarsAreHidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "._old"), "junk")
	writeFile(t, filepath.Join(dir, "d", "real"), "data")
	f := newTestFS(t, dir)

	entries, err := f.ReadDir("d")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, entries); len(got) != 1 || got[0] != "real" {
		t.Fatalf("ReadDir = %v, want [real]", got)
	}
	if _, err := f.Stat("d/._old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat of a remote sidecar = %v, want ErrNotExist", err)
	}
}

func TestRemoveDirHoldingOnlyRemoteSidecars(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "junk", "._a"), "x")
	writeFile(t, filepath.Join(dir, "junk", ".DS_Store"), "x")
	writeFile(t, filepath.Join(dir, "full", "._a"), "x")
	writeFile(t, filepath.Join(dir, "full", "kept"), "x")
	f := newTestFS(t, dir)

	if err := f.Remove("junk"); err != nil {
		t.Fatalf("Remove(junk) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("junk still on the remote: %v", err)
	}

	var status *nfs.NFSStatusError
	if err := f.Remove("full"); !errors.As(err, &status) || status.NFSStatus != nfs.NFSStatusNotEmpty {
		t.Fatalf("Remove(full) = %v, want NOTEMPTY", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "full", "._a")); err != nil {
		t.Fatalf("Remove(full) deleted a sidecar from a non-empty directory: %v", err)
	}
}

func TestRenameDirCarriesSidecars(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	if err := f.MkdirAll("g", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create("g/._x"); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("g"); err == nil {
		t.Fatal("Remove(g) succeeded while it held a sidecar")
	}
	if err := f.Rename("g", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat("h/._x"); err != nil {
		t.Fatalf("sidecar did not move with its directory: %v", err)
	}
	if _, err := f.Stat("g/._x"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar left at the old path: %v", err)
	}
}

func TestSidecarTruncateZeroFills(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	file, err := f.Create("._z")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(3); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := file.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "a\x00\x00" {
		t.Fatalf("after shrink and grow = %q, want %q", buf, "a\x00\x00")
	}
}

// TestOrphanedAppleDoubleIsDropped deletes a file behind the mount's back,
// as the web app or another device would, and expects its in-memory
// AppleDouble file to leave both the listing and the store. .DS_Store
// belongs to the directory and stays.
func TestOrphanedAppleDoubleIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "note.md"), "x")
	f := newTestFS(t, dir)
	for _, name := range []string{"._note.md", ".DS_Store"} {
		if _, err := f.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "note.md")); err != nil {
		t.Fatal(err)
	}
	f.vfs.FlushDirCache()

	entries, err := f.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, entries); len(got) != 1 || got[0] != ".DS_Store" {
		t.Fatalf("ReadDir = %v, want [.DS_Store]", got)
	}
	if _, err := f.Stat("._note.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned ._note.md is still stored: %v", err)
	}
}
