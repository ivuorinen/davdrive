// Package server runs one mount's daemon: the WebDAV remote behind an
// rclone VFS, served over NFSv4 on the loopback interface, and a control
// socket that reports its status.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	billy "github.com/go-git/go-billy/v5"
	_ "github.com/rclone/rclone/backend/local"  // the VFS cache stores its files through it
	_ "github.com/rclone/rclone/backend/webdav" // the remote
	"github.com/rclone/rclone/fs"
	rconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/fsys"
)

const (
	// shutdownWait bounds how long Stop waits for pending uploads. Files
	// still pending stay in the VFS cache and upload on the next mount.
	shutdownWait = 10 * time.Minute
	// handleLimit is how many NFS file handles are remembered; rclone's
	// default for serve nfs.
	handleLimit = 1_000_000
)

// Server is a running daemon.
type Server struct {
	mount    *config.Mount
	vfs      *vfs.VFS
	nfs      *nfs.Server
	control  *http.Server
	port     int
	started  time.Time
	stopping atomic.Bool
	cancel   context.CancelFunc
	lock     *os.File   // held for the server's lifetime; see lockMount
	done     chan error // see Done
}

// Start connects to the remote and starts serving it. ctx bounds the
// start-up only; the server runs until Stop.
func Start(ctx context.Context, m *config.Mount, password string) (*Server, error) {
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return nil, err
	}
	lock, err := lockMount(m.LockPath())
	if err != nil {
		return nil, err
	}
	controlLn, err := listenControl(m.SocketPath())
	if err != nil {
		_ = lock.Close()

		return nil, err
	}
	s, err := start(ctx, m, password)
	if err != nil {
		_ = controlLn.Close()
		_ = os.Remove(m.SocketPath())
		_ = lock.Close()

		return nil, err
	}
	s.lock = lock
	s.serveControl(controlLn)

	return s, nil
}

func start(ctx context.Context, m *config.Mount, password string) (*Server, error) {
	if err := rconfig.SetCacheDir(config.CacheDir()); err != nil {
		return nil, err
	}
	opt, err := vfsOptions(ctx, m.VFS)
	if err != nil {
		return nil, err
	}
	f, err := newRemote(ctx, m, password)
	if err != nil {
		return nil, err
	}
	// The VFS uploads in the background until Stop, so it keeps the
	// start-up context's values but not its cancellation.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &Server{
		mount:   m,
		vfs:     vfs.New(runCtx, f, &opt),
		started: time.Now(),
		cancel:  cancel,
		done:    make(chan error, 1),
	}
	// The VFS logs and turns its cache off when it cannot create it, which
	// would leave a silently read-only mount.
	if s.vfs.Opt.CacheMode != opt.CacheMode {
		s.vfs.Shutdown()
		cancel()

		return nil, fmt.Errorf("could not create the VFS cache in %s; see the log", config.CacheDir())
	}
	if err := s.serveNFS(); err != nil {
		s.vfs.Shutdown()
		cancel()

		return nil, err
	}

	return s, nil
}

// Port is the loopback TCP port the NFS server listens on.
func (s *Server) Port() int { return s.port }

// vfsOptions starts from the defaults chosen for kDrive and applies the
// mount's [vfs] table, which uses rclone's option names.
//
// The full cache mode is what makes NFS fast: go-nfs opens and closes the
// file for every READ and WRITE call, and only cache items keep the open
// download stream alive between calls (vfs_handle_caching). The size and
// age caps keep files from staying on the device.
func vfsOptions(ctx context.Context, overrides map[string]any) (vfscommon.Options, error) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CacheMaxAge = fs.Duration(time.Hour)
	opt.CacheMaxSize = 2 * fs.Gibi
	conf := configmap.Simple{}
	for name, value := range overrides {
		if !knownVFSOption(name) {
			return opt, fmt.Errorf("vfs: unknown option %q", name)
		}
		conf[name] = fmt.Sprint(value)
	}
	if err := configstruct.Set(conf, &opt); err != nil {
		return opt, fmt.Errorf("vfs: %w", err)
	}
	opt.Init(ctx)

	return opt, nil
}

func knownVFSOption(name string) bool {
	for _, o := range vfscommon.OptionsInfo {
		if o.Name == name {
			return true
		}
	}

	return false
}

// newRemote builds the webdav backend from the mount alone, never from an
// rclone config file. kDrive is a plain WebDAV server: vendor "other", as
// in Infomaniak's rclone guides.
func newRemote(ctx context.Context, m *config.Mount, password string) (fs.Fs, error) {
	ri, err := fs.Find("webdav")
	if err != nil {
		return nil, err
	}
	conf := configmap.Simple{"url": m.URL, "vendor": "other"}
	if m.User != "" {
		conf["user"] = m.User
		conf["pass"] = obscure.MustObscure(password)
	}
	f, err := ri.NewFs(ctx, m.Name, "", fs.ConfigMap(ri.Prefix, ri.Options, "", conf))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", m.URL, err)
	}
	// Listing the root fails fast on a wrong URL or password, before
	// anything is mounted.
	if _, err := f.List(ctx, ""); err != nil {
		return nil, fmt.Errorf("listing %s: %w", m.URL, err)
	}

	return f, nil
}

// Stop shuts the server down in the order that keeps data safe: NFS stops,
// pending uploads finish, and the control socket closes last so that
// status can report the uploads meanwhile. The caller unmounts first; the
// server must not stop under a live mount, which would then hang.
func (s *Server) Stop(ctx context.Context) {
	s.stopping.Store(true)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := s.nfs.Shutdown(ctx); err != nil {
		_ = s.nfs.Close()
	}
	cancel()
	s.vfs.WaitForWriters(shutdownWait)
	s.vfs.Shutdown()
	s.cancel()
	_ = s.control.Close()
	_ = os.Remove(s.mount.SocketPath())
	_ = s.lock.Close()
}

// Done delivers the error of a listener that stopped by itself. The
// daemon keeps running after its NFS server dies otherwise, looking
// healthy while every NFS request hangs.
func (s *Server) Done() <-chan error { return s.done }

// fail reports an unexpected listener exit through Done. Only the first is
// kept: one is enough to stop the server.
func (s *Server) fail(err error) {
	select {
	case s.done <- err:
	default:
	}
}

func (s *Server) serveNFS() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()

		return fmt.Errorf("NFS listener has a non-TCP address %v", ln.Addr())
	}
	s.port = addr.Port
	bfs := fsys.New(s.vfs)
	h := &handler{Handler: helpers.NewNullAuthHandler(bfs), vfs: s.vfs, fs: bfs}
	// ConcurrentHandlers stays at its default, one request at a time per
	// connection, as in rclone's serve nfs. Requests on several
	// connections still run concurrently; fsys serializes open handles per
	// file, which keeps the VFS cache from losing writes either way.
	s.nfs = &nfs.Server{
		Handler:            helpers.NewCachingHandler(h, handleLimit),
		EnabledNFSVersions: []uint32{4},
		Authorize:          ownerOnly(uint32(os.Getuid())), //nolint:gosec // G115: uids are 32-bit on darwin and linux
	}
	if runtime.GOOS == "linux" {
		ln = reservedPortListener{ln}
	}
	go func() {
		if err := s.nfs.Serve(ln); err != nil && !errors.Is(err, nfs.ErrServerClosed) {
			s.fail(fmt.Errorf("NFS server: %w", err))
		}
	}()

	return nil
}

// handler is go-nfs's null-auth handler with the change and statfs calls
// wired to the VFS.
type handler struct {
	nfs.Handler
	vfs *vfs.VFS
	fs  *fsys.FS
}

func (h *handler) Change(billy.Filesystem) billy.Change {
	if !h.fs.Writable() {
		return nil
	}

	return h.fs
}

func (h *handler) FSStat(_ context.Context, _ billy.Filesystem, s *nfs.FSStat) error {
	total, _, free := h.vfs.Statfs()
	s.TotalSize = nonNegative(total)
	s.FreeSize = nonNegative(free)
	s.AvailableSize = s.FreeSize

	return nil
}

// nonNegative clamps a size the VFS reports to zero: free space comes out
// negative when an account is over its quota.
func nonNegative(n int64) uint64 {
	if n < 0 {
		return 0
	}

	return uint64(n)
}
