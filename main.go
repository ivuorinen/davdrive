// Command davdrive mounts a WebDAV share, such as an Infomaniak kDrive,
// as a local drive. It serves the share over NFSv4 on the loopback
// interface and mounts that with the operating system's own NFS client, so
// it needs no kernel extension or FUSE.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/rclone/rclone/fs"
	rlog "github.com/rclone/rclone/fs/log"
	"github.com/spf13/cobra"
	nfs "github.com/willscott/go-nfs"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/platform"
	"github.com/ivuorinen/davdrive/internal/server"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

type app struct {
	configPath string
	verbose    int
}

// The CLI reaches the mount table through these, so its tests can swap in a fake.
var (
	mountNFS   = platform.Mount
	unmountNFS = platform.Unmount
	isMounted  = platform.IsMounted
)

func newRootCmd() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:               "davdrive",
		Short:             "Mount a WebDAV share such as Infomaniak kDrive as a local drive",
		Version:           buildVersion(),
		SilenceUsage:      true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return config.CheckDirs() },
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", config.DefaultPath(), "config file")
	root.PersistentFlags().CountVarP(&a.verbose, "verbose", "v", "log more; -vv logs debug output")
	root.AddCommand(a.mountCmd(), a.daemonCmd(), a.unmountCmd(), a.statusCmd())

	return root
}

// version is stamped by GoReleaser (-ldflags "-X main.version=..."). Builds
// from source leave it empty and fall back to the module version.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}

	return "dev"
}

// mount loads the config and selects the mount named in args, or the only
// one configured.
func (a *app) mount(args []string) (*config.Mount, error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, err
	}
	var name string
	if len(args) > 0 {
		name = args[0]
	}

	return cfg.Mount(name)
}

func (a *app) completeNames(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cfg.Names(), cobra.ShellCompDirectiveNoFileComp
}

// mountState is what the CLI can learn about one share without trusting
// its server to answer.
type mountState struct {
	status  *server.Status // nil when no server answered
	mounted bool
	// queryErr is set when a control socket exists but its server did not
	// answer properly (hung, or exiting). Callers that clear mounts must
	// still act on mounted; the escape hatch cannot depend on the server.
	queryErr error
}

// state reports whether m's mountpoint is mounted and what its server, if
// any, says. Only a failure to read the mount table is an error.
func state(ctx context.Context, m *config.Mount) (mountState, error) {
	mounted, err := isMounted(platform.Canonical(m.Mountpoint))
	if err != nil {
		return mountState{}, err
	}
	st, err := server.Query(ctx, m.SocketPath())
	if errors.Is(err, server.ErrNotRunning) {
		err = nil
	}

	return mountState{status: st, mounted: mounted, queryErr: err}, nil
}

// unmountIfMounted unmounts m before its server stops: once the server has
// stopped, the mount would hang every process that touches it. A busy
// mount is forced, since the server is going away either way. It reports
// whether the mountpoint is unmounted afterwards; a mount table that
// cannot be read counts as still mounted, so nobody stops the server under
// a live mount on a guess.
func unmountIfMounted(m *config.Mount, interactive bool) (unmounted bool) {
	mp := platform.Canonical(m.Mountpoint)
	mounted, err := isMounted(mp)
	if err != nil {
		fs.Errorf(nil, "Reading the mount table: %v", err)

		return false
	}
	if !mounted {
		return true
	}
	if err := unmountNFS(mp, false, interactive); err != nil {
		fs.Errorf(nil, "Unmounting %s: %v; forcing", mp, err)
		if err := unmountNFS(mp, true, interactive); err != nil {
			fs.Errorf(nil, "Forced unmount of %s failed: %v", mp, err)
		}
	}
	mounted, err = isMounted(mp)

	return err == nil && !mounted
}

// runningServer is what serveUntilStopped needs of a server: *server.Server,
// or a fake whose failure a test controls.
type runningServer interface {
	Done() <-chan error
	Stop(ctx context.Context)
}

// waitForStop blocks until SIGINT or SIGTERM arrives or the server fails,
// and returns the failure, nil for a signal. Default signal handling is
// back in place when it returns, so a second signal quits at once.
func waitForStop(srv runningServer) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
		return nil
	case err := <-srv.Done():
		return err
	}
}

// serveUntilStopped runs srv until a stop request can be honored safely.
// A signal while the share is still mounted (the unmount failed, for
// instance because sudo needs a password the daemon cannot ask for) keeps
// the server running: stopping it would strand a hard NFS mount that
// blocks every process touching it until root intervenes. A failed server
// stops either way, since it can no longer serve the mount.
func serveUntilStopped(ctx context.Context, srv runningServer, m *config.Mount, interactive bool) {
	for {
		failure := waitForStop(srv)
		if failure != nil {
			fs.Errorf(nil, "Server failed: %v; unmounting and stopping", failure)
		}
		if !unmountIfMounted(m, interactive) && failure == nil {
			fs.Errorf(
				nil,
				"%s is still mounted, so the server keeps running; clear it with: davdrive unmount --force %s",
				m.Mountpoint,
				m.Name,
			)

			continue
		}
		fs.Logf(nil, "Stopping; pending uploads finish first (signal again to quit now, they resume on the next mount)")
		srv.Stop(ctx)

		return
	}
}

// setupLogging routes rclone's and go-nfs's logs to logFile, rotated, or
// to stderr when logFile is empty.
func setupLogging(ctx context.Context, logFile string, verbose int) {
	if logFile != "" {
		rlog.Opt.File = logFile
		rlog.Opt.MaxSize = 10 * fs.Mebi
		rlog.Opt.MaxBackups = 3
	}
	rlog.InitLogging()
	level, nfsLevel := fs.LogLevelNotice, nfs.WarnLevel
	switch {
	case verbose >= 2:
		level, nfsLevel = fs.LogLevelDebug, nfs.DebugLevel
	case verbose == 1:
		level = fs.LogLevelInfo
	}
	fs.GetConfig(ctx).LogLevel = level
	rlog.Handler.SetLevel(fs.LogLevelToSlog(level))
	nfs.SetLogger(&nfsLogger{level: nfsLevel})
}
