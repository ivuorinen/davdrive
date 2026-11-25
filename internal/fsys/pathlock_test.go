package fsys

import (
	"path/filepath"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
)

// lockRefs reports how many handles hold or wait for name's path lock.
func lockRefs(t *testing.T, f *FS, name string) int {
	t.Helper()
	f.locks.mu.Lock()
	defer f.locks.mu.Unlock()
	if l := f.locks.locks[name]; l != nil {
		return l.refs
	}

	return 0
}

// waitRefs polls until name's path lock has want holders and waiters.
func waitRefs(t *testing.T, f *FS, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for lockRefs(t, f, name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("path lock on %s has %d holders, want %d", name, lockRefs(t, f, name), want)
		}
		time.Sleep(time.Millisecond)
	}
}

type opened struct {
	h   billy.File
	err error
}

// TestOpenWaitsForPreviousClose opens one file twice: the second open must
// wait until the first handle is closed, and the lock entry must go away
// once both are.
func TestOpenWaitsForPreviousClose(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, fileA), "x")
	f := newTestFS(t, dir)

	first, err := f.Open(fileA)
	must(t, err)
	second := make(chan opened, 1)
	go func() {
		h, err := f.Open("/" + fileA)
		second <- opened{h, err}
	}()
	waitRefs(t, f, fileA, 2)
	select {
	case <-second:
		t.Fatal("second open returned while the first handle was open")
	default:
	}

	must(t, first.Close())
	// Closing twice must not release the lock the second handle now holds.
	_ = first.Close()
	var got opened
	select {
	case got = <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("second open still blocked after the first handle closed")
	}
	must(t, got.err)
	if n := lockRefs(t, f, fileA); n != 1 {
		t.Fatalf("path lock on a.txt has %d holders after a double close, want 1", n)
	}
	must(t, got.h.Close())
	if n := len(f.locks.locks); n != 0 {
		t.Fatalf("%d path locks left after every handle closed", n)
	}
}
