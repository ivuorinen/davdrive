package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivuorinen/davdrive/internal/config"
)

// serveUnix answers HTTP on a fresh unix socket and returns its path.
func serveUnix(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return path
}

// TestQueryErrors covers a socket that answers but not with a status: each
// must surface as an error, never as a status or as ErrNotRunning.
func TestQueryErrors(t *testing.T) {
	broken := serveUnix(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	garbled := serveUnix(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	healthy := serveUnix(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Status{Name: "healthy"})
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	var noContext context.Context

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		socket string
		check  func(error) bool
		want   string
	}{
		{
			name: "server error", ctx: t.Context(), socket: broken, want: "status request: 500 Internal Server Error",
			check: func(err error) bool { return err.Error() == "status request: 500 Internal Server Error" },
		},
		{
			name: "invalid JSON", ctx: t.Context(), socket: garbled, want: "a JSON syntax error",
			check: func(err error) bool {
				var syn *json.SyntaxError

				return errors.As(err, &syn)
			},
		},
		{
			name: "canceled", ctx: canceled, socket: healthy, want: "context canceled",
			check: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		{
			name: "no context", ctx: noContext, socket: healthy, want: "nil Context",
			check: func(err error) bool { return strings.Contains(err.Error(), "nil Context") },
		},
	} {
		st, err := Query(tc.ctx, tc.socket)
		if err == nil || errors.Is(err, ErrNotRunning) || !tc.check(err) {
			t.Errorf("%s: Query = %+v, %v; want %s", tc.name, st, err, tc.want)
		}
	}

	if st, err := Query(t.Context(), healthy); err != nil || st.Name != "healthy" {
		t.Errorf("healthy socket: Query = %+v, %v; want name healthy", st, err)
	}
}

func TestListenControlErrors(t *testing.T) {
	dir := shortSocketDir(t)

	busy := filepath.Join(dir, "busy")
	if err := os.MkdirAll(filepath.Join(busy, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := listenControl(busy); err == nil {
		t.Error("a non-empty directory at the socket path was replaced")
	}
	if _, err := os.Stat(filepath.Join(busy, "child")); err != nil {
		t.Errorf("directory at the socket path was damaged: %v", err)
	}

	if _, err := listenControl(filepath.Join(dir, "missing", "d.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket in a missing directory: err = %v, want not exist", err)
	}
}

func TestAsInt64(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int64
	}{
		{3, 3},
		{int64(1) << 40, 1 << 40},
		{"3", 0},
		{3.0, 0},
		{nil, 0},
	} {
		if got := asInt64(tc.in); got != tc.want {
			t.Errorf("asInt64(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestControlSocketFailureReportsDone breaks the control listener and
// expects the failure on Done, which is what stops the daemon.
func TestControlSocketFailureReportsDone(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "c.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	s := &Server{mount: &config.Mount{Name: "broken"}, done: make(chan error, 1)}
	s.serveControl(ln)

	select {
	case err := <-s.Done():
		if !errors.Is(err, net.ErrClosed) || !strings.HasPrefix(err.Error(), "control socket: ") {
			t.Fatalf("Done = %v, want a control socket error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("control listener failure was not reported on Done")
	}
}

func TestFailKeepsFirstError(t *testing.T) {
	s := &Server{done: make(chan error, 1)}
	first, second := errors.New("NFS server: first"), errors.New("control socket: second")
	s.fail(first)
	s.fail(second)

	if got := <-s.Done(); !errors.Is(got, first) {
		t.Fatalf("Done = %v, want %v", got, first)
	}
	select {
	case got := <-s.Done():
		t.Fatalf("Done delivered a second error: %v", got)
	default:
	}
}
