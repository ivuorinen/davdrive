package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

func TestVFSOptionsDefaults(t *testing.T) {
	opt, err := vfsOptions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if opt.CacheMode != vfscommon.CacheModeFull {
		t.Errorf("cache mode = %v, want full", opt.CacheMode)
	}
	if opt.CacheMaxSize != 2*fs.Gibi {
		t.Errorf("max size = %v, want 2Gi", opt.CacheMaxSize)
	}
	if opt.CacheMaxAge != fs.Duration(time.Hour) {
		t.Errorf("max age = %v, want 1h", opt.CacheMaxAge)
	}
}

func TestVFSOptionsOverrides(t *testing.T) {
	opt, err := vfsOptions(context.Background(), map[string]any{"vfs_write_back": "30s", "vfs_cache_max_size": "512M"})
	if err != nil {
		t.Fatal(err)
	}
	if opt.WriteBack != fs.Duration(30*time.Second) || opt.CacheMaxSize != 512*fs.Mebi {
		t.Fatalf("overrides = write back %v, max size %v; want 30s, 512Mi", opt.WriteBack, opt.CacheMaxSize)
	}
	if opt.CacheMode != vfscommon.CacheModeFull {
		t.Fatalf("an unrelated override changed the cache mode to %v", opt.CacheMode)
	}
}

func TestVFSOptionsUnknownKey(t *testing.T) {
	_, err := vfsOptions(context.Background(), map[string]any{"vfs_cache_max_sise": "1G"})
	if err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("misspelled option: err = %v, want unknown option", err)
	}
}

// shortSocketDir returns a directory whose socket paths fit the 104-byte
// sun_path limit on macOS, which t.TempDir() paths can exceed.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nm") //nolint:usetesting // t.TempDir() paths overflow the macOS socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// TestQueryNotRunning checks the two shapes a dead daemon leaves behind,
// no socket file and a socket file nobody listens on, both of which every
// command must read as "not running" rather than as a failure.
func TestQueryNotRunning(t *testing.T) {
	dir := shortSocketDir(t)
	ctx := context.Background()

	if _, err := Query(ctx, filepath.Join(dir, "absent.sock")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no socket: err = %v, want ErrNotRunning", err)
	}

	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}
	if _, err := Query(ctx, stale); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale socket: err = %v, want ErrNotRunning", err)
	}
}

// TestLockMountExcludesSecondServer starts "two servers" for one share and
// expects the second to be refused while the first holds the lock, and
// admitted once it is released.
func TestLockMountExcludesSecondServer(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "k.lock")
	first, err := lockMount(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockMount(path); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock: err = %v, want already running", err)
	}
	_ = first.Close()
	again, err := lockMount(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = again.Close()
}
