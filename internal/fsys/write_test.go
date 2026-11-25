package fsys

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"io"
	mrand "math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	rconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// newCachedTestFS serves dir through a local-backend VFS in full cache
// mode, the mode the daemon runs in.
func newCachedTestFS(t *testing.T, dir string, writeBack time.Duration) (*FS, *vfs.VFS) {
	t.Helper()
	if err := rconfig.SetCacheDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ri := fs.MustFind("local")
	f, err := ri.NewFs(ctx, "cached", dir, fs.ConfigMap(ri.Prefix, ri.Options, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.WriteBack = fs.Duration(writeBack)
	v := vfs.New(ctx, f, &opt)
	t.Cleanup(v.Shutdown)

	return New(v), v
}

// nfsWrite does what go-nfs does for one WRITE call: stat, open, seek,
// write, close.
func nfsWrite(f *FS, name string, off int64, data []byte) error {
	info, err := f.Stat(name)
	if err != nil {
		return err
	}
	file, err := f.OpenFile(name, os.O_RDWR, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := file.Seek(off, io.SeekStart); err != nil {
		_ = file.Close()

		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return err
	}

	return file.Close()
}

// checkWrites sends a file as 4 KiB WRITE calls in random order from
// workers goroutines, waits for the upload, and checks that the remote
// copy is byte for byte what was written.
func checkWrites(t *testing.T, workers int, writeBack time.Duration) {
	t.Helper()
	const (
		chunk  = 4 << 10
		chunks = 2048
	)
	dir := t.TempDir()
	f, v := newCachedTestFS(t, dir, writeBack)
	want := nonZeroData(t, chunk*chunks)
	created, err := f.Create("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	writeShuffled(t, f, "big.bin", want, chunk, workers)
	v.WaitForWriters(time.Minute)
	compareRemote(t, dir, "big.bin", want, chunk)
}

// nonZeroData returns n random bytes with no zero byte, so that a lost
// write, which reads back as zeros, cannot match.
func nonZeroData(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := crand.Read(data); err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] |= 1
	}

	return data
}

// writeShuffled writes data to name as chunk-sized WRITE calls, in random
// order, from workers goroutines at once.
func writeShuffled(t *testing.T, f *FS, name string, data []byte, chunk, workers int) {
	t.Helper()
	jobs := make(chan int)
	errs := make(chan error, len(data)/chunk)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				off := int64(i * chunk)
				errs <- nfsWrite(f, name, off, data[off:off+int64(chunk)])
			}
		})
	}
	// The order only has to differ from sequential and between runs; the
	// detection rate in TestConcurrentWrites was measured with this shuffle.
	for _, i := range mrand.Perm(len(data) / chunk) { //nolint:gosec // G404: test write order, not security
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// compareRemote checks that the file the VFS uploaded into dir is want, and
// counts the chunks that differ when it is not.
func compareRemote(t *testing.T, dir, name string, want []byte, chunk int) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	got, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, want) {
		return
	}
	lost := 0
	for i := 0; i < len(want); i += chunk {
		if i+chunk > len(got) || !bytes.Equal(got[i:i+chunk], want[i:i+chunk]) {
			lost++
		}
	}
	t.Fatalf("remote copy has %d bytes, want %d; %d of %d chunks wrong", len(got), len(want), lost, len(want)/chunk)
}

// TestConcurrentWrites sends files as out-of-order WRITE calls from 16
// goroutines, as several NFS connections would, with uploads starting
// between writes. Without pathLocks, 9 of 10 runs of it lost writes; one
// round alone catches that rarely, hence the rounds.
//
// Under the race detector it is skipped: its 1 ms write-back reaches a
// data race in rclone's write-back queue (_cancelUpload reads done after
// unlocking). The test-race task runs it again without -race. Remove the
// skip once a fixed rclone is required:
// https://github.com/ivuorinen/davdrive/issues/2
func TestConcurrentWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 20 s")
	}
	if raceEnabled {
		t.Skip("reaches a data race in rclone's write-back queue; see https://github.com/ivuorinen/davdrive/issues/2")
	}
	for range 30 {
		checkWrites(t, 16, time.Millisecond)
	}
}
