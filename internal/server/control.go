package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/rclone/rclone/fs/rc"
)

// ErrNotRunning means no daemon answers on the control socket.
var ErrNotRunning = errors.New("not running")

// Status is what a daemon reports on its control socket.
type Status struct {
	Name              string    `json:"name"`
	URL               string    `json:"url"`
	Mountpoint        string    `json:"mountpoint"`
	PID               int       `json:"pid"`
	Port              int       `json:"port"`
	Started           time.Time `json:"started"`
	Stopping          bool      `json:"stopping"`
	UploadsInProgress int       `json:"uploads_in_progress"`
	UploadsQueued     int       `json:"uploads_queued"`
	ErroredFiles      int       `json:"errored_files"`
	CacheFiles        int       `json:"cache_files"`
	CacheBytes        int64     `json:"cache_bytes"`
	CacheMaxBytes     int64     `json:"cache_max_bytes"`
}

// lockMount takes the share's lock file for the server's lifetime. The
// control socket alone cannot settle a race between two servers starting
// together: both find no listener, the second removes the first one's
// socket file and listens, and the first keeps serving NFS where the CLI
// can no longer reach it. The kernel drops the flock when the process
// exits, so a crash leaves nothing to clean up.
func lockMount(path string) (*os.File, error) {
	// path is Mount.LockPath(): a fixed name under the user's state directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: see above
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("a server is already running for this share")
		}

		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	return f, nil
}

// listenControl claims the control socket. A socket nobody answers on is
// left over from a daemon that died, and is replaced.
func listenControl(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		_ = c.Close()

		return nil, fmt.Errorf("a server is already running on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()

		return nil, err
	}

	return ln, nil
}

func (s *Server) serveControl(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.status())
	})
	s.control = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.control.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.fail(fmt.Errorf("control socket: %w", err))
		}
	}()
}

func (s *Server) status() Status {
	st := Status{
		Name:          s.mount.Name,
		URL:           s.mount.URL,
		Mountpoint:    s.mount.Mountpoint,
		PID:           os.Getpid(),
		Port:          s.port,
		Started:       s.started,
		Stopping:      s.stopping.Load(),
		CacheMaxBytes: int64(s.vfs.Opt.CacheMaxSize),
	}
	if cache, ok := s.vfs.Stats()["diskCache"].(rc.Params); ok {
		st.UploadsInProgress = int(asInt64(cache["uploadsInProgress"]))
		st.UploadsQueued = int(asInt64(cache["uploadsQueued"]))
		st.ErroredFiles = int(asInt64(cache["erroredFiles"]))
		st.CacheFiles = int(asInt64(cache["files"]))
		st.CacheBytes = asInt64(cache["bytesUsed"])
	}

	return st
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}

	return 0
}

// Query asks the daemon listening on socket for its status.
func Query(ctx context.Context, socket string) (*Status, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://daemon/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status request: %s", resp.Status)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}

	return &st, nil
}
