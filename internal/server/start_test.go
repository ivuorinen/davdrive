package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	nfs "github.com/willscott/go-nfs"
	"golang.org/x/net/webdav"

	"github.com/ivuorinen/davdrive/internal/config"
)

const (
	davUser     = "kdrive-user"
	davPassword = "s3cret"
	defaultMax  = int64(2 * fs.Gibi)
	// optCacheMaxSize is rclone's name for the VFS cache size cap.
	optCacheMaxSize = "vfs_cache_max_size"
)

// isolate points the config file and every XDG directory at temporary
// ones, with a state directory short enough for the control socket.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", shortSocketDir(t))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(config.EnvConfig, filepath.Join(t.TempDir(), "config.toml"))
}

// davOptions shapes the in-process WebDAV server.
type davOptions struct {
	user, password string
	// quotaUsed and quotaFree, when quotaUsed is set, answer rclone's
	// quota PROPFIND as kDrive does; an empty quotaFree is left unknown.
	quotaUsed, quotaFree string
	// holdPUT, when set, keeps every upload waiting until it is closed.
	holdPUT chan struct{}
}

type davServer struct {
	*httptest.Server
	files webdav.FileSystem
}

// newDAV serves an in-memory WebDAV share on the loopback interface.
func newDAV(t *testing.T, opt davOptions) *davServer {
	t.Helper()
	files := webdav.NewMemFS()
	dav := &webdav.Handler{FileSystem: files, LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !opt.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}
		if opt.holdPUT != nil && r.Method == http.MethodPut {
			select {
			case <-opt.holdPUT:
			case <-r.Context().Done():
				return
			}
		}
		if opt.quotaUsed != "" && serveQuota(w, r, opt.quotaUsed, opt.quotaFree) {
			return
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return &davServer{Server: srv, files: files}
}

// authorized checks the request's basic auth when the server has a user.
func (opt davOptions) authorized(r *http.Request) bool {
	if opt.user == "" {
		return true
	}
	u, p, ok := r.BasicAuth()

	return ok && u == opt.user && p == opt.password
}

// serveQuota answers the PROPFIND rclone's About sends, which
// x/net/webdav does not implement, and reports whether it did.
func serveQuota(w http.ResponseWriter, r *http.Request, used, free string) bool {
	if r.Method != "PROPFIND" || r.Header.Get("Depth") != "0" {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || !bytes.Contains(body, []byte("quota-used-bytes")) {
		r.Body = io.NopCloser(bytes.NewReader(body))

		return false
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:"><d:response><d:href>/</d:href><d:propstat><d:prop>
<d:quota-available-bytes>%s</d:quota-available-bytes><d:quota-used-bytes>%s</d:quota-used-bytes>
</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, free, used)

	return true
}

func newMount(name, url string) *config.Mount {
	return &config.Mount{Name: name, URL: url, Mountpoint: "/Volumes/" + name}
}

// startServer starts a server for m and stops it when the test ends,
// unless the test stopped it first with the returned function.
func startServer(t *testing.T, m *config.Mount, password string) (*Server, func(context.Context)) {
	t.Helper()
	s, err := Start(t.Context(), m, password)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var once sync.Once
	stop := func(ctx context.Context) { once.Do(func() { s.Stop(ctx) }) }
	t.Cleanup(func() { stop(context.WithoutCancel(t.Context())) })

	return s, stop
}

// waitFor polls cond until it holds, failing the test after a deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertReleased checks that a server that failed to start, or stopped,
// left neither its control socket nor its lock behind.
func assertReleased(t *testing.T, m *config.Mount) {
	t.Helper()
	if _, err := os.Stat(m.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control socket left behind: stat err = %v", err)
	}
	lock, err := lockMount(m.LockPath())
	if err != nil {
		t.Fatalf("share lock still held: %v", err)
	}
	_ = lock.Close()
}

func TestStartServesShare(t *testing.T) {
	for _, tc := range []struct {
		name, user, password string
	}{
		{"basic auth", davUser, davPassword},
		{"no auth", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			dav := newDAV(t, davOptions{user: tc.user, password: tc.password})
			m := newMount("kd", dav.URL)
			m.User = tc.user
			before := time.Now()
			s, _ := startServer(t, m, tc.password)

			if s.Port() <= 0 {
				t.Fatalf("Port() = %d, want a TCP port", s.Port())
			}
			c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
			if err != nil {
				t.Fatalf("NFS port does not accept connections: %v", err)
			}
			_ = c.Close()

			st, err := Query(t.Context(), m.SocketPath())
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			want := Status{
				Name: m.Name, URL: m.URL, Mountpoint: m.Mountpoint, PID: os.Getpid(), Port: s.Port(),
				CacheMaxBytes: defaultMax, Started: st.Started,
			}
			if *st != want {
				t.Errorf("status = %+v, want %+v", *st, want)
			}
			if st.Started.Before(before.Add(-time.Second)) || st.Started.After(time.Now()) {
				t.Errorf("started = %v, want the start time (after %v)", st.Started, before)
			}
			fi, err := os.Stat(m.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("control socket mode = %o, want 600", perm)
			}
		})
	}
}

// TestStopReleasesShare stops a server and expects every trace of it to
// go, so that the share can be mounted again.
func TestStopReleasesShare(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	m := newMount("restart", dav.URL)
	s, stop := startServer(t, m, "")
	addr := fmt.Sprintf("127.0.0.1:%d", s.Port())

	stop(t.Context())

	if _, err := Query(t.Context(), m.SocketPath()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Query after Stop: err = %v, want ErrNotRunning", err)
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		_ = c.Close()
		t.Errorf("NFS port %s still accepts connections after Stop", addr)
	}
	assertReleased(t, m)

	again, _ := startServer(t, m, "")
	st, err := Query(t.Context(), m.SocketPath())
	if err != nil {
		t.Fatalf("Query after restart: %v", err)
	}
	if st.Port != again.Port() {
		t.Errorf("restarted server reports port %d, want %d", st.Port, again.Port())
	}
}

func TestStartFailures(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "http://" + closed.Addr().String() + "/"
	_ = closed.Close()
	dav := newDAV(t, davOptions{user: davUser, password: davPassword})

	for _, tc := range []struct {
		name, url, password string
		vfs                 map[string]any
		setup               func(t *testing.T)
		want                []string
	}{
		{name: "malformed URL", url: "http://[::1", want: []string{"connecting to http://[::1"}},
		{name: "unreachable server", url: unreachable, want: []string{"listing " + unreachable}},
		{name: "wrong password", url: dav.URL, password: "wrong", want: []string{"listing", "401"}},
		{
			name: "misspelled vfs option", url: dav.URL, password: davPassword,
			vfs: map[string]any{"vfs_cahce_mode": "full"}, want: []string{`unknown option "vfs_cahce_mode"`},
		},
		{
			name: "cache directory cannot be created", url: dav.URL, password: davPassword,
			setup: func(t *testing.T) {
				t.Helper()
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_CACHE_HOME", filepath.Join(file, "cache"))
			},
			want: []string{"could not create the VFS cache"},
		},
		{
			name: "invalid vfs value", url: dav.URL, password: davPassword,
			vfs: map[string]any{optCacheMaxSize: "lots"}, want: []string{"vfs: "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if tc.setup != nil {
				tc.setup(t)
			}
			m := newMount("fail", tc.url)
			m.User = davUser
			m.VFS = tc.vfs
			// One attempt is enough to see a refused connection.
			ctx, ci := fs.AddConfig(t.Context())
			ci.LowLevelRetries = 1

			s, err := Start(ctx, m, tc.password)
			if err == nil {
				s.Stop(ctx)
				t.Fatal("Start succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to mention %q", err, w)
				}
			}
			assertReleased(t, m)
		})
	}
}

// TestStartStateFailures covers the state directory and lock file being
// unusable, which fail before any socket is claimed.
func TestStartStateFailures(t *testing.T) {
	t.Run("state directory is a file", func(t *testing.T) {
		isolate(t)
		file := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_STATE_HOME", file)
		_, err := Start(t.Context(), newMount("st", "http://127.0.0.1:1/"), "")
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("err = %v, want not a directory", err)
		}
	})
	t.Run("lock path is a directory", func(t *testing.T) {
		isolate(t)
		m := newMount("st", "http://127.0.0.1:1/")
		if err := os.MkdirAll(m.LockPath(), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := Start(t.Context(), m, "")
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("err = %v, want is a directory", err)
		}
		if _, err := os.Stat(m.SocketPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("control socket created despite the lock failing: %v", err)
		}
	})
}

// TestStartRefusesSecondServer starts a second server for a share that
// is being served and expects it refused without disturbing the first.
func TestStartRefusesSecondServer(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	m := newMount("twice", dav.URL)
	first, _ := startServer(t, m, "")

	if s, err := Start(t.Context(), m, ""); err == nil {
		s.Stop(t.Context())
		t.Fatal("second Start succeeded")
	} else if !strings.Contains(err.Error(), "already running for this share") {
		t.Fatalf("second Start: err = %v, want already running for this share", err)
	}
	st, err := Query(t.Context(), m.SocketPath())
	if err != nil {
		t.Fatalf("first server stopped answering: %v", err)
	}
	if st.Port != first.Port() {
		t.Errorf("socket answered with port %d, want the first server's %d", st.Port, first.Port())
	}
}

// TestStartRefusesLiveSocket finds something answering on the control
// socket and expects Start to leave it alone and release the lock.
func TestStartRefusesLiveSocket(t *testing.T) {
	isolate(t)
	m := newMount("live", "http://127.0.0.1:1/")
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", m.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	_, err = Start(t.Context(), m, "")
	if err == nil || !strings.Contains(err.Error(), "already running on "+m.SocketPath()) {
		t.Fatalf("err = %v, want already running on the socket", err)
	}
	c, err := net.Dial("unix", m.SocketPath())
	if err != nil {
		t.Fatalf("the live socket was removed: %v", err)
	}
	_ = c.Close()
	lock, err := lockMount(m.LockPath())
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = lock.Close()
}

// TestStartReplacesStaleSocket finds the socket file of a daemon that
// died and expects Start to take its place.
func TestStartReplacesStaleSocket(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	m := newMount("stale", dav.URL)
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: m.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()

	s, _ := startServer(t, m, "")
	st, err := Query(t.Context(), m.SocketPath())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if st.Port != s.Port() {
		t.Errorf("status port = %d, want %d", st.Port, s.Port())
	}
}

// TestStatusReportsUploads writes a file while the remote holds the
// upload and watches the status report it in progress, then done and
// kept in the cache.
func TestStatusReportsUploads(t *testing.T) {
	isolate(t)
	hold := make(chan struct{})
	dav := newDAV(t, davOptions{holdPUT: hold})
	m := newMount("uploads", dav.URL)
	m.VFS = map[string]any{"vfs_write_back": "10ms", "vfs_cache_poll_interval": "20ms", optCacheMaxSize: "64M"}
	s, _ := startServer(t, m, "")
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(hold) }) }) // runs before the server stops

	data := []byte("hello, kDrive")
	if err := s.vfs.WriteFile("hello.txt", data, 0o644); err != nil {
		t.Fatal(err)
	}
	query := func() Status {
		st, err := Query(t.Context(), m.SocketPath())
		if err != nil {
			t.Fatalf("Query: %v", err)
		}

		return *st
	}
	waitFor(t, "the upload to start", func() bool { return query().UploadsInProgress == 1 })

	release.Do(func() { close(hold) })
	waitFor(t, "the upload to finish and the cache size to be counted", func() bool {
		st := query()

		return st.UploadsInProgress == 0 && st.UploadsQueued == 0 && st.CacheBytes == int64(len(data))
	})
	st := query()
	if st.CacheFiles != 1 || st.ErroredFiles != 0 || st.CacheMaxBytes != 64*int64(fs.Mebi) {
		t.Errorf("status = files %d, errored %d, max %d; want 1, 0, 64Mi",
			st.CacheFiles, st.ErroredFiles, st.CacheMaxBytes)
	}
	f, err := dav.files.OpenFile(t.Context(), "/hello.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("file missing on the remote: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got, _ := io.ReadAll(f); !bytes.Equal(got, data) {
		t.Errorf("remote content = %q, want %q", got, data)
	}
}

// TestHandlerChange checks that go-nfs is offered attribute changes only
// when the VFS can write.
func TestHandlerChange(t *testing.T) {
	dav := newDAV(t, davOptions{})
	for _, tc := range []struct {
		name     string
		vfs      map[string]any
		writable bool
	}{
		{"full cache", nil, true},
		{"cache off", map[string]any{"vfs_cache_mode": "off"}, false},
		{"read only", map[string]any{"read_only": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			m := newMount("change", dav.URL)
			m.VFS = tc.vfs
			s, _ := startServer(t, m, "")
			if got := s.nfs.Change(nil) != nil; got != tc.writable {
				t.Errorf("Change offered = %v, want %v", got, tc.writable)
			}
			// Without a disk cache there is nothing to count.
			st, err := Query(t.Context(), m.SocketPath())
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if st.CacheFiles != 0 || st.CacheBytes != 0 || st.UploadsQueued != 0 {
				t.Errorf("fresh server reports cache use: %+v", *st)
			}
		})
	}
}

// TestFSStat reports the remote's quota to NFS clients, with the free
// space of an account over its quota shown as zero.
func TestFSStat(t *testing.T) {
	for _, tc := range []struct {
		name              string
		used, free        string
		vfs               map[string]any
		wantTotal, wantFr uint64
	}{
		{name: "quota reported", used: "100", free: "900", wantTotal: 1000, wantFr: 900},
		{
			name: "over quota", used: "5000", vfs: map[string]any{"vfs_disk_space_total_size": "1000B"},
			wantTotal: 1000, wantFr: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			dav := newDAV(t, davOptions{quotaUsed: tc.used, quotaFree: tc.free})
			m := newMount("quota", dav.URL)
			m.VFS = tc.vfs
			s, _ := startServer(t, m, "")

			var st nfs.FSStat
			if err := s.nfs.FSStat(t.Context(), nil, &st); err != nil {
				t.Fatal(err)
			}
			if st.TotalSize != tc.wantTotal || st.FreeSize != tc.wantFr || st.AvailableSize != tc.wantFr {
				t.Errorf("FSStat = total %d, free %d, available %d; want %d, %d, %d",
					st.TotalSize, st.FreeSize, st.AvailableSize, tc.wantTotal, tc.wantFr, tc.wantFr)
			}
		})
	}
}

// TestNFSListenerFailureReportsDone breaks a running server's NFS
// listener and expects the failure on Done: the daemon then stops instead
// of looking healthy while every NFS request hangs.
func TestNFSListenerFailureReportsDone(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	s, _ := startServer(t, newMount("nfsfail", dav.URL), "")
	_ = s.nfsLn.Close()

	select {
	case err := <-s.Done():
		if !errors.Is(err, net.ErrClosed) || !strings.HasPrefix(err.Error(), "NFS server: ") {
			t.Fatalf("Done = %v, want an NFS server error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("NFS listener failure was not reported on Done")
	}
}
