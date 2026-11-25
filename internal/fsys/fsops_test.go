package fsys

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/file"
)

// Names and content the tests share.
const (
	fileA     = "a.txt"     // a real file
	sideA     = "._a.txt"   // its AppleDouble sidecar
	movedSide = "d/._b.txt" // a sidecar in a subdirectory
	dsStore   = ".DS_Store"
	content   = "real"
)

// must fails the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// wantStatus fails unless err carries the NFS status code want, which is
// what go-nfs sends to the client.
func wantStatus(t *testing.T, err error, want nfs.NFSStatus) {
	t.Helper()
	var status *nfs.NFSStatusError
	if !errors.As(err, &status) || status.NFSStatus != want {
		t.Fatalf("err = %v, want NFS status %v", err, want)
	}
}

// wantIs fails unless errors.Is(err, want).
func wantIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// wantAllIs fails unless every error in errs, keyed by the call that
// returned it, matches want.
func wantAllIs(t *testing.T, errs map[string]error, want error) {
	t.Helper()
	for call, err := range errs {
		if !errors.Is(err, want) {
			t.Errorf("%s = %v, want %v", call, err, want)
		}
	}
}

// remoteExists reports whether name is in the remote directory dir.
func remoteExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	return err == nil
}

// readFS reads all of name through a fresh handle and closes it.
func readFS(t *testing.T, f *FS, name string) string {
	t.Helper()
	h, err := f.Open(name)
	must(t, err)
	data, err := io.ReadAll(h)
	must(t, err)
	must(t, h.Close())

	return string(data)
}

// wantNames fails unless the entries are named want, in order.
func wantNames(t *testing.T, entries []os.FileInfo, want ...string) {
	t.Helper()
	if got := names(t, entries); !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

// wantSys fails unless info carries the go-nfs attributes for an entry
// owned by uid:gid with a file id.
func wantSys(t *testing.T, info os.FileInfo, uid, gid uint32) *file.FileInfo {
	t.Helper()
	sys, ok := info.Sys().(*file.FileInfo)
	if !ok {
		t.Fatalf("%s: Sys() = %T, want *file.FileInfo", info.Name(), info.Sys())
	}
	if *sys != (file.FileInfo{Nlink: 1, UID: uid, GID: gid, Fileid: sys.Fileid}) || sys.Fileid == 0 {
		t.Fatalf("%s: Sys() = %+v, want one link, owner %d:%d and a file id", info.Name(), sys, uid, gid)
	}

	return sys
}

func TestMapErr(t *testing.T) {
	tests := []struct {
		name string
		in   error
		code nfs.NFSStatus // zero: in comes back unchanged
	}{
		{"not empty", vfs.ENOTEMPTY, nfs.NFSStatusNotEmpty},
		{"read-only", vfs.EROFS, nfs.NFSStatusROFS},
		{"not implemented", vfs.ENOSYS, nfs.NFSStatusNotSupp},
		{"invalid", os.ErrInvalid, nfs.NFSStatusInval},
		{"wrapped read-only", fmt.Errorf("upload: %w", vfs.EROFS), nfs.NFSStatusROFS},
		{"not exist passes through", os.ErrNotExist, 0},
		{"exist passes through", os.ErrExist, 0},
		{"nil", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.in)
			if tt.code != 0 {
				wantStatus(t, got, tt.code)
				wantIs(t, got, tt.in)

				return
			}
			if got != tt.in { //nolint:errorlint // pass-through means the very same error value
				t.Fatalf("mapErr(%v) = %v, want it unchanged", tt.in, got)
			}
		})
	}
}

func TestOpenAndStat(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), content)
	f := newTestFSWith(t, dir, func(o *vfscommon.Options) { o.UID, o.GID = 501, 20 })

	if got := readFS(t, f, fileA); got != content {
		t.Fatalf("Open(a.txt) read %q, want %q", got, content)
	}
	for name, stat := range map[string]func(string) (os.FileInfo, error){"Stat": f.Stat, "Lstat": f.Lstat} {
		info, err := stat(fileA)
		must(t, err)
		wantSys(t, info, 501, 20)
		if info.Size() != 4 {
			t.Fatalf("%s(a.txt).Size() = %d, want 4", name, info.Size())
		}
		_, err = stat("missing")
		wantIs(t, err, os.ErrNotExist)
	}
	if info, err := f.Stat("/"); err != nil || !info.IsDir() {
		t.Fatalf("Stat(/) = %v, %v; want a directory", info, err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), content)
	f := newTestFS(t, dir)

	_, errOpen := f.Open("missing")
	_, errCreate := f.Create("nodir/x")
	wantAllIs(t, map[string]error{"Open(missing)": errOpen, "Create(nodir/x)": errCreate}, os.ErrNotExist)
	_, err := f.OpenFile(fileA, os.O_RDONLY|os.O_TRUNC, 0)
	wantStatus(t, err, nfs.NFSStatusInval)
	// A failed open must release the path lock, or this would block.
	if got := readFS(t, f, fileA); got != content {
		t.Fatalf("a.txt = %q after a refused truncate, want %q", got, content)
	}
}

func TestReadDirListsOnlyItsOwnSidecars(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "x"), "x")
	f := newTestFS(t, dir)
	for _, name := range []string{dsStore, "d/.DS_Store"} {
		_, err := f.Create(name)
		must(t, err)
	}

	entries, err := f.ReadDir("/")
	must(t, err)
	wantNames(t, entries, dsStore, "d")
	_, err = f.ReadDir("missing")
	wantIs(t, err, os.ErrNotExist)
}

func TestRenameAcrossDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), content)
	writeFile(t, filepath.Join(dir, "d", "keep"), "x")
	f := newTestFS(t, dir)
	createSidecar(t, f, sideA, xattrs)

	must(t, f.Rename(fileA, "d/b.txt"))
	if remoteExists(t, dir, fileA) || !remoteExists(t, dir, "d/b.txt") {
		t.Fatal("file did not move to d/b.txt on the remote")
	}

	must(t, f.Rename(sideA, movedSide))
	_, err := f.Stat(sideA)
	wantIs(t, err, os.ErrNotExist)
	if got := readFS(t, f, movedSide); got != xattrs {
		t.Fatalf("d/._b.txt = %q, want %q", got, xattrs)
	}
	if remoteExists(t, dir, movedSide) {
		t.Fatal("renamed sidecar reached the remote")
	}
}

func TestRenameErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "d", "b.txt"), content)
	f := newTestFS(t, dir)
	createSidecar(t, f, movedSide, xattrs)

	tests := []struct {
		from, to string
		err      error         // wanted with errors.Is, when code is zero
		code     nfs.NFSStatus // wanted NFS status
	}{
		{from: movedSide, to: "nodir/._b.txt", err: os.ErrNotExist},
		{from: movedSide, to: "d/b.txt/._x", code: nfs.NFSStatusNotDir},
		{from: movedSide, to: "d/plain", err: os.ErrPermission},
		{from: "d/b.txt", to: "d/._c", err: os.ErrPermission},
		{from: "._missing", to: "._other", err: os.ErrNotExist},
		{from: "missing", to: "other", err: os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			err := f.Rename(tt.from, tt.to)
			if tt.code != 0 {
				wantStatus(t, err, tt.code)

				return
			}
			wantIs(t, err, tt.err)
		})
	}
	if !remoteExists(t, dir, "d/b.txt") {
		t.Fatal("a failed rename moved d/b.txt")
	}
	if got := readFS(t, f, movedSide); got != xattrs {
		t.Fatalf("a failed rename changed d/._b.txt to %q", got)
	}
}

func TestRenameSidecarOverAnother(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	createSidecar(t, f, dsStore, "old-old")
	createSidecar(t, f, "._x", "new")

	must(t, f.Rename("._x", dsStore))
	must(t, f.Rename(dsStore, dsStore))
	if got := readFS(t, f, dsStore); got != "new" {
		t.Fatalf(".DS_Store = %q, want %q", got, "new")
	}
	// The overwritten sidecar's bytes no longer count against sidecarLimit.
	if f.side.size != 3 {
		t.Fatalf("sidecar store holds %d bytes, want 3", f.side.size)
	}
}

func TestRenameDirLeavesOtherSidecars(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	must(t, f.MkdirAll("g", 0o755))
	must(t, f.MkdirAll("gx", 0o755))
	for _, name := range []string{dsStore, "gx/.DS_Store"} {
		createSidecar(t, f, name, "")
	}
	must(t, f.Rename("g", "h"))
	for _, name := range []string{dsStore, "gx/.DS_Store"} {
		_, err := f.Stat(name)
		must(t, err)
	}
}

func TestRemoveSidecar(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	createSidecar(t, f, sideA, xattrs)

	must(t, f.Remove(sideA))
	_, err := f.Stat(sideA)
	wantIs(t, err, os.ErrNotExist)
	wantIs(t, f.Remove(sideA), os.ErrNotExist)
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	writeFile(t, filepath.Join(dir, "e", "x"), "x")
	f := newTestFS(t, dir)
	must(t, f.MkdirAll("d", 0o755))
	createSidecar(t, f, "e/.DS_Store", "")

	if err := f.Remove("d"); err != nil {
		t.Fatalf("Remove(d) with a sidecar in another directory = %v", err)
	}
	must(t, f.Remove(fileA))
	if remoteExists(t, dir, "d") || remoteExists(t, dir, fileA) {
		t.Fatal("removed entries are still on the remote")
	}
	wantIs(t, f.Remove("missing"), os.ErrNotExist)
}

// TestRemoveKeepsRemoteSidecarsItCannotDelete makes the remote refuse to
// delete the hidden sidecars of a directory: the directory must then be
// reported as not empty instead of half-removed.
func TestRemoveKeepsRemoteSidecarsItCannotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk")
	writeFile(t, filepath.Join(junk, "._a"), "x")
	f := newTestFS(t, dir)
	// Read and search, but no write: entries cannot be deleted. A
	// directory needs x to be listed, hence more than 0600.
	must(t, os.Chmod(junk, 0o500))                  //nolint:gosec // G302: see above
	t.Cleanup(func() { _ = os.Chmod(junk, 0o700) }) //nolint:gosec // G302: lets TempDir cleanup delete it

	wantStatus(t, f.Remove("junk"), nfs.NFSStatusNotEmpty)
	if !remoteExists(t, dir, "junk/._a") {
		t.Fatal("the sidecar vanished from the remote")
	}
}

func TestMkdirAll(t *testing.T) {
	dir := t.TempDir()
	f := newTestFS(t, dir)

	must(t, f.MkdirAll("x/y/z", 0o755))
	must(t, f.MkdirAll("x/y/z", 0o755))
	must(t, f.MkdirAll("/", 0o755))
	if info, err := os.Stat(filepath.Join(dir, "x", "y", "z")); err != nil || !info.IsDir() {
		t.Fatalf("x/y/z on the remote = %v, %v; want a directory", info, err)
	}
	wantIs(t, f.MkdirAll("x/._d", 0o755), os.ErrPermission)
}

// TestMkdirAllOverFile is the NFSv4 CREATE of a directory, which goes
// straight to MkdirAll: a name taken by a file must fail, not report a
// directory that is not there.
func TestMkdirAllOverFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)

	wantAllIs(t, map[string]error{
		"MkdirAll(a.txt)":   f.MkdirAll(fileA, 0o755),
		"MkdirAll(a.txt/b)": f.MkdirAll(fileA+"/b", 0o755),
	}, os.ErrExist)
	if info, err := os.Stat(filepath.Join(dir, fileA)); err != nil || info.IsDir() {
		t.Fatalf("a.txt on the remote = %v, %v; want the file", info, err)
	}
}

func TestUnsupportedOperations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)

	_, errTemp := f.TempFile("", "tmp")
	_, errChroot := f.Chroot("x")
	wantAllIs(t, map[string]error{"TempFile": errTemp, "Chroot": errChroot}, billy.ErrNotSupported)
	wantIs(t, f.Symlink(fileA, "._link"), os.ErrPermission)
	wantStatus(t, f.Symlink(fileA, "link"), nfs.NFSStatusNotSupp)
	_, err := f.Readlink(fileA)
	wantStatus(t, err, nfs.NFSStatusNotSupp)
	if remoteExists(t, dir, "link") || remoteExists(t, dir, "._link") {
		t.Fatal("a refused symlink reached the remote")
	}
}

func TestLinksEnabled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFSWith(t, dir, func(o *vfscommon.Options) { o.Links = true })

	_, err := f.Readlink(fileA)
	wantStatus(t, err, nfs.NFSStatusInval)
	_, err = f.Readlink("missing")
	wantIs(t, err, os.ErrNotExist)
	wantIs(t, f.Symlink(fileA, fileA), os.ErrExist)
	must(t, f.Symlink(fileA, "link"))
	if target, err := f.Readlink("link"); err != nil || target != fileA {
		t.Fatalf("Readlink(link) = %q, %v; want a.txt", target, err)
	}
}

func TestPathHelpers(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	if got := f.Root(); got != "/" {
		t.Fatalf("Root() = %q, want /", got)
	}
	if got := f.Join("a", "b/", "../c", "d"); got != "a/c/d" {
		t.Fatalf("Join = %q, want a/c/d", got)
	}
}

func TestCapabilities(t *testing.T) {
	const (
		readCaps  = billy.ReadCapability | billy.SeekCapability
		writeCaps = billy.WriteCapability | billy.ReadAndWriteCapability | billy.TruncateCapability
	)
	cached, _ := newCachedTestFS(t, t.TempDir(), time.Second)
	tests := []struct {
		name     string
		f        *FS
		writable bool
	}{
		{"full cache", cached, true},
		{"cache off", newTestFS(t, t.TempDir()), false},
		{"read-only", newTestFSWith(t, t.TempDir(), func(o *vfscommon.Options) {
			o.ReadOnly = true
			o.CacheMode = vfscommon.CacheModeFull
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := billy.Capability(readCaps)
			if tt.writable {
				want |= writeCaps
			}
			if got := tt.f.Capabilities(); got != want {
				t.Fatalf("Capabilities() = %v, want %v", got, want)
			}
			if tt.f.Writable() != tt.writable {
				t.Fatalf("Writable() = %v, want %v", tt.f.Writable(), tt.writable)
			}
		})
	}
}

func TestReadOnlyVFSRefusesChanges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFSWith(t, dir, func(o *vfscommon.Options) { o.ReadOnly = true })

	wantStatus(t, f.Remove(fileA), nfs.NFSStatusROFS)
	wantStatus(t, f.MkdirAll("new", 0o755), nfs.NFSStatusROFS)
	wantStatus(t, f.Chtimes(fileA, time.Now(), time.Now()), nfs.NFSStatusROFS)
	_, err := f.Create("b.txt")
	wantStatus(t, err, nfs.NFSStatusROFS)
	if !remoteExists(t, dir, fileA) || remoteExists(t, dir, "new") || remoteExists(t, dir, "b.txt") {
		t.Fatal("a read-only VFS changed the remote")
	}
}

// TestOwnerAndTimes checks Chown, Lchown and Chtimes on a file and on its
// sidecar. The remote keeps no owners, so Chown succeeds without effect.
func TestOwnerAndTimes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)
	createSidecar(t, f, sideA, "")
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	for _, name := range []string{fileA, sideA} {
		must(t, f.Chown(name, 1, 2))
		must(t, f.Lchown(name, 1, 2))
		must(t, f.Chtimes(name, mtime, mtime))
		info, err := f.Stat(name)
		must(t, err)
		if !info.ModTime().Equal(mtime) {
			t.Fatalf("%s: ModTime() = %v after Chtimes, want %v", name, info.ModTime(), mtime)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, fileA)); err != nil || !info.ModTime().Equal(mtime) {
		t.Fatalf("a.txt on the remote = %v, %v; want mtime %v", info, err, mtime)
	}
}

// TestChmod: the remote keeps no modes, so Chmod on a file succeeds
// without effect; a sidecar keeps the permission bits.
func TestChmod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)
	createSidecar(t, f, sideA, "")

	must(t, f.Chmod(fileA, 0o600))
	must(t, f.Chmod(sideA, os.ModeDir|0o640))
	info, err := f.Stat(sideA)
	must(t, err)
	if info.Mode() != 0o640 {
		t.Fatalf("Stat(._a.txt).Mode() = %v after Chmod, want 0640", info.Mode())
	}
}

func TestAttributeChangesOnMissing(t *testing.T) {
	f := newTestFS(t, t.TempDir())
	mtime := time.Now()
	for _, name := range []string{"missing", "._missing"} {
		wantAllIs(t, map[string]error{
			"Chmod(" + name + ")":   f.Chmod(name, 0o600),
			"Chown(" + name + ")":   f.Chown(name, 1, 2),
			"Lchown(" + name + ")":  f.Lchown(name, 1, 2),
			"Chtimes(" + name + ")": f.Chtimes(name, mtime, mtime),
		}, os.ErrNotExist)
	}
}
