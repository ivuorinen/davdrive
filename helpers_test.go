package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	rlog "github.com/rclone/rclone/fs/log"
	nfs "github.com/willscott/go-nfs"
	"golang.org/x/net/webdav"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/platform"
	"github.com/ivuorinen/davdrive/internal/server"
)

// Credentials the test WebDAV server accepts.
const (
	davUser     = "u"
	davPassword = "secret"
)

// waitTimeout bounds every poll for an event a test waits on.
const waitTimeout = 30 * time.Second

// unmountedK is what unmount prints once share k is fully unmounted.
const unmountedK = "Unmounted k\n"

// nameCommands are the commands that take a share name.
var nameCommands = []string{"mount", "unmount", "status"}

// share is one [mounts.<name>] table of a test config file.
type share struct {
	name, url, user, passwordCommand, mountpoint string
}

// testEnv is an isolated davdrive installation: config, state and cache
// directories in temp dirs, a WebDAV server and a fake mount table.
type testEnv struct {
	cfg    string // config file path
	dav    string // WebDAV server URL
	mp     string // default share's mountpoint
	mounts *fakeMounts
}

// newEnv sets up a config with one share, "k", served by an in-process
// WebDAV server that requires davUser/davPassword.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", shortDir(t))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv(config.EnvPassword, "")
	if err := os.Unsetenv(config.EnvPassword); err != nil {
		t.Fatal(err)
	}
	// A daemon that mount spawns re-executes this test binary; see TestMain.
	t.Setenv(daemonEnv, "1")

	root := t.TempDir()
	dav := &webdav.Handler{FileSystem: webdav.Dir(root), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != davUser || p != davPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	e := &testEnv{
		cfg:    filepath.Join(dir, "config.toml"),
		dav:    srv.URL + "/",
		mp:     filepath.Join(dir, "mnt"),
		mounts: installFakeMounts(t),
	}
	t.Setenv(config.EnvConfig, e.cfg)
	e.writeConfig(t, e.share("k"))

	return e
}

// share returns a share served by the env's WebDAV server with a working
// password_command.
func (e *testEnv) share(name string) share {
	mp := e.mp
	if name != "k" {
		mp = filepath.Join(filepath.Dir(e.mp), name)
	}

	return share{
		name:            name,
		url:             e.dav,
		user:            davUser,
		passwordCommand: "printf " + davPassword,
		mountpoint:      mp,
	}
}

func (e *testEnv) writeConfig(t *testing.T, shares ...share) {
	t.Helper()
	var b strings.Builder
	for _, s := range shares {
		fmt.Fprintf(&b, "[mounts.%s]\nurl = %q\nmountpoint = %q\n", s.name, s.url, s.mountpoint)
		if s.user != "" {
			fmt.Fprintf(&b, "user = %q\n", s.user)
		}
		if s.passwordCommand != "" {
			fmt.Fprintf(&b, "password_command = %q\n", s.passwordCommand)
		}
	}
	if err := os.WriteFile(e.cfg, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mount returns the named share as the CLI resolves it.
func (e *testEnv) mount(t *testing.T, name string) *config.Mount {
	t.Helper()
	cfg, err := config.Load(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := cfg.Mount(name)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// run executes the CLI with args and returns what it printed on stdout.
func (e *testEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = execute(t.Context(), args...) })

	return out, err
}

// execute runs the CLI with args; cobra's own error and usage output is
// discarded, the returned error carries it.
func execute(ctx context.Context, args ...string) error {
	root := newRootCmd() //nolint:contextcheck // the commands get ctx through ExecuteContext below
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	return root.ExecuteContext(ownConfig(ctx))
}

// ownConfig gives ctx its own copy of rclone's config, so that the log
// level setupLogging sets lands there instead of in the global config,
// which goroutines that rclone leaves behind after a server stops still
// read.
func ownConfig(ctx context.Context) context.Context {
	ctx, _ = fs.AddConfig(ctx)

	return ctx
}

// shortDir returns a directory whose socket paths fit the 104-byte
// sun_path limit on macOS, which t.TempDir() paths can exceed.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dd") //nolint:usetesting // t.TempDir() paths overflow the macOS socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// captureStdout returns what fn prints on os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(copied)
	}()
	defer func() {
		os.Stdout = old
		_ = w.Close()
		<-copied
		_ = r.Close()
	}()
	fn()
	os.Stdout = old
	_ = w.Close()
	<-copied

	return buf.String()
}

// mountCall and unmountCall record what the CLI asked of the mount table.
type mountCall struct {
	port       int
	mountpoint string
}

type unmountCall struct {
	mountpoint         string
	force, interactive bool
}

// fakeMounts is an in-memory mount table. Errors queued in mountErrs,
// unmountErrs and tableErrs are returned by successive calls, then calls
// succeed.
type fakeMounts struct {
	mu          sync.Mutex
	mounted     map[string]bool
	mountErrs   []error
	unmountErrs []error
	tableErrs   []error
	mountCalls  []mountCall
	unmounts    []unmountCall
}

func installFakeMounts(t *testing.T) *fakeMounts {
	t.Helper()
	f := &fakeMounts{mounted: map[string]bool{}}
	oldMount, oldUnmount, oldIsMounted := mountNFS, unmountNFS, isMounted
	mountNFS, unmountNFS, isMounted = f.mount, f.unmount, f.isMounted
	t.Cleanup(func() { mountNFS, unmountNFS, isMounted = oldMount, oldUnmount, oldIsMounted })

	return f
}

func popErr(q *[]error) error {
	if len(*q) == 0 {
		return nil
	}
	err := (*q)[0]
	*q = (*q)[1:]

	return err
}

func (f *fakeMounts) mount(port int, mountpoint string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mountCalls = append(f.mountCalls, mountCall{port, mountpoint})
	if err := popErr(&f.mountErrs); err != nil {
		return err
	}
	f.mounted[platform.Canonical(mountpoint)] = true

	return nil
}

func (f *fakeMounts) unmount(mountpoint string, force, interactive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounts = append(f.unmounts, unmountCall{mountpoint, force, interactive})
	if err := popErr(&f.unmountErrs); err != nil {
		return err
	}
	delete(f.mounted, mountpoint)

	return nil
}

func (f *fakeMounts) isMounted(mountpoint string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := popErr(&f.tableErrs); err != nil {
		return false, err
	}

	return f.mounted[mountpoint], nil
}

// set marks mountpoint as mounted.
func (f *fakeMounts) set(mountpoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted[platform.Canonical(mountpoint)] = true
}

// queue appends errors for the next calls of one kind.
func (f *fakeMounts) queue(q *[]error, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*q = append(*q, errs...)
}

func (f *fakeMounts) isMountedAt(mountpoint string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.mounted[platform.Canonical(mountpoint)]
}

func (f *fakeMounts) calls() ([]mountCall, []unmountCall) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]mountCall(nil), f.mountCalls...), append([]unmountCall(nil), f.unmounts...)
}

// checkUnmounts fails unless got holds one interactive unmount of
// mountpoint per entry of forces, with that force flag.
func checkUnmounts(t *testing.T, got []unmountCall, mountpoint string, forces []bool) {
	t.Helper()
	want := make([]unmountCall, 0, len(forces))
	for _, force := range forces {
		want = append(want, unmountCall{platform.Canonical(mountpoint), force, true})
	}
	if !slices.Equal(got, want) {
		t.Errorf("unmount calls = %+v, want %+v", got, want)
	}
}

// checkMounts fails unless got holds one mount of mountpoint per entry of
// ports, from that port.
func checkMounts(t *testing.T, got []mountCall, mountpoint string, ports []int) {
	t.Helper()
	want := make([]mountCall, 0, len(ports))
	for _, port := range ports {
		want = append(want, mountCall{port, mountpoint})
	}
	if !slices.Equal(got, want) {
		t.Errorf("mount calls = %+v, want %+v", got, want)
	}
}

// serveControl answers on socket as a daemon's control socket would, with
// h deciding each reply. The returned func makes the "daemon" go away.
func serveControl(t *testing.T, socket string, h http.HandlerFunc) (stop func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	stop = func() { once.Do(func() { _ = srv.Close() }) }
	t.Cleanup(stop)

	return stop
}

// statusReply answers every request with st.
func statusReply(st server.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(st)
	}
}

// brokenReply answers every request with a server error, as a daemon that
// is shutting down or wedged can.
func brokenReply(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "broken", http.StatusInternalServerError)
}

// waitFor polls cond until it holds or waitTimeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// notRunning reports whether no daemon answers on m's control socket.
func notRunning(ctx context.Context, m *config.Mount) bool {
	_, err := server.Query(ctx, m.SocketPath())

	return errors.Is(err, server.ErrNotRunning)
}

// reapDaemon kills whatever daemon still answers on m's control socket
// when the test ends; a spawned daemon runs in its own session and would
// outlive the test binary.
func reapDaemon(t *testing.T, m *config.Mount) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		st, err := server.Query(ctx, m.SocketPath())
		if err != nil {
			return
		}
		_ = syscall.Kill(st.PID, syscall.SIGKILL)
		waitFor(t, "the daemon to die", func() bool { return notRunning(ctx, m) })
	})
}

// interruptUntil sends SIGINT to this process until done holds. Its own
// signal.Notify keeps a SIGINT that arrives before the code under test
// listens from killing the test binary, and each signal is confirmed
// delivered before the next, so none is still in flight when it stops.
func interruptUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	deadline := time.Now().Add(waitTimeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out interrupting until %s", what)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-sig:
		case <-time.After(waitTimeout):
			t.Fatal("SIGINT was not delivered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// interruptUntilReturns sends SIGINT until the code under test delivers
// its result on done, and returns that result.
func interruptUntilReturns(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	var err error
	interruptUntil(t, what, func() bool {
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	})

	return err
}

// logRecord is one message that reached rclone's log handler.
type logRecord struct {
	level slog.Level
	text  string
}

// logCapture collects rclone's log output.
type logCapture struct {
	mu   sync.Mutex
	recs []logRecord
}

func (c *logCapture) String() string {
	var b strings.Builder
	for _, r := range c.records() {
		b.WriteString(r.text)
	}

	return b.String()
}

func (c *logCapture) add(level slog.Level, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, logRecord{level, text})
}

func (c *logCapture) records() []logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]logRecord(nil), c.recs...)
}

func (c *logCapture) contains(s string) bool {
	for _, r := range c.records() {
		if strings.Contains(r.text, s) {
			return true
		}
	}

	return false
}

// isolateLogging restores rclone's and go-nfs's global logging setup when
// the test ends, since setupLogging changes it process-wide.
func isolateLogging(t *testing.T) {
	t.Helper()
	opt := rlog.Opt
	nfsLog := nfs.Log
	// SetLevel is the only way to read the handler's level.
	handlerLevel := rlog.Handler.SetLevel(slog.LevelInfo)
	rlog.Handler.SetLevel(handlerLevel)
	t.Cleanup(func() {
		rlog.Opt = opt
		rlog.Handler.SetLevel(handlerLevel)
		nfs.SetLogger(nfsLog)
	})
}

// captureLogs isolates logging and collects everything rclone logs, at
// any level the handler lets through, until the test ends.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	isolateLogging(t)
	c := &logCapture{}
	rlog.Handler.SetOutput(c.add)
	t.Cleanup(rlog.Handler.ResetOutput)

	return c
}
