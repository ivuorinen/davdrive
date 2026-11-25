package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/platform"
	"github.com/ivuorinen/davdrive/internal/server"
)

// readyTimeout bounds how long mount waits for the daemon to connect.
const readyTimeout = time.Minute

// readyFD is the descriptor the daemon reports start-up on: "ok <port>"
// or "error: <message>". It is the first of exec.Cmd.ExtraFiles.
const readyFD = 3

// mountpointPerm is the mode of a mountpoint that mount creates. Once the
// share is mounted, clients see the share's root instead.
const mountpointPerm = 0o750

func (a *app) mountCmd() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:   "mount [name]",
		Short: "Mount a share; its server keeps running in the background",
		Long: `Mount a share; its server keeps running in the background.

Without a name, mounts the only share in the config file. Mounting a share
that is already mounted does nothing; with -f it is an error.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.mount(args)
			if err != nil {
				return err
			}
			if foreground {
				return a.mountForeground(cmd.Context(), m)
			}

			return a.mountBackground(cmd.Context(), m)
		},
	}
	cmd.Flags().
		BoolVarP(&foreground, "foreground", "f", false, "stay in the foreground and log to stderr; Ctrl-C unmounts")

	return cmd
}

func (a *app) mountBackground(ctx context.Context, m *config.Mount) error {
	s, err := state(ctx, m)
	if err != nil {
		return err
	}
	if handled, err := resume(m, s); handled {
		return err
	}
	if err := os.MkdirAll(m.Mountpoint, mountpointPerm); err != nil {
		return err
	}
	password, err := m.Password(ctx)
	if err != nil {
		return err
	}
	daemon, port, err := a.spawnDaemon(m, password)
	if err != nil {
		return err
	}
	if err := attach(m, port); err != nil {
		_ = daemon.Signal(os.Interrupt)

		return err
	}

	return nil
}

// resume handles a share that already has a server or a mount, and reports
// handled=false when it has neither and a server must be started.
func resume(m *config.Mount, s mountState) (handled bool, err error) {
	st := s.status
	switch {
	case s.queryErr != nil:
		return true, notAnsweringError(m, s.queryErr)
	case st != nil && st.Stopping:
		return true, fmt.Errorf("%s is stopping; wait for it to finish (davdrive status %s)", m.Name, m.Name)
	case st != nil && s.mounted:
		fmt.Printf("%s is already mounted at %s\n", m.Name, m.Mountpoint)

		return true, nil
	case st != nil:
		// The server outlived its mount, for example after a manual
		// umount: mount it again.
		return true, attach(m, st.Port)
	case s.mounted:
		return true, staleMountError(m)
	}

	return false, nil
}

func attach(m *config.Mount, port int) error {
	if err := mountNFS(port, m.Mountpoint, m.MountOptions); err != nil {
		return err
	}
	fmt.Printf("Mounted %s at %s (log: %s)\n", m.Name, m.Mountpoint, m.LogPath())

	return nil
}

func staleMountError(m *config.Mount) error {
	return fmt.Errorf(
		"%s is mounted but no server is running for %s; remove the stale mount with: davdrive unmount --force %s",
		m.Mountpoint,
		m.Name,
		m.Name,
	)
}

func notAnsweringError(m *config.Mount, err error) error {
	return fmt.Errorf("the server for %s does not answer (%v); clear the mount with: davdrive unmount --force %s",
		m.Name, err, m.Name)
}

// spawnDaemon starts "davdrive daemon" in its own session, hands it the
// password on stdin and waits for it to report its NFS port.
func (a *app) spawnDaemon(m *config.Mount, password string) (*os.Process, int, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return nil, 0, err
	}
	// Until its own logging starts, the daemon's stderr (a panic, say) goes
	// to the log file too.
	logFile, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = logFile.Close() }()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = readyR.Close() }()

	args := []string{"daemon", "--config", a.configPath, m.Name}
	if a.verbose > 0 {
		args = append(args, "-"+strings.Repeat("v", a.verbose))
	}
	// Re-executes this same binary with fixed arguments; nothing reaches a shell.
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: exe is os.Executable(), not input
	cmd.Stdin = strings.NewReader(password + "\n")
	// The password travels on stdin only; the long-lived daemon does not
	// keep it in its environment.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, config.EnvPassword+"=")
	})
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.SysProcAttr = platform.DetachAttr()
	err = cmd.Start()
	_ = readyW.Close()
	if err != nil {
		return nil, 0, err
	}

	_ = readyR.SetReadDeadline(time.Now().Add(readyTimeout))
	out, readErr := io.ReadAll(readyR)
	port, err := parseReady(out, readErr, m.LogPath())
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return nil, 0, err
	}

	return cmd.Process, port, nil
}

// parseReady interprets what the daemon wrote on the ready pipe before
// closing it: "ok <port>" or "error: <message>". Nothing at all means it
// died before reporting. logPath goes into the errors so the user knows
// where the daemon's own account of the failure is.
func parseReady(out []byte, readErr error, logPath string) (int, error) {
	reply := strings.TrimSpace(string(out))
	if p, ok := strings.CutPrefix(reply, "ok "); ok {
		if port, err := strconv.Atoi(p); err == nil && port > 0 && port <= 65535 {
			return port, nil
		}
	}
	switch {
	case errors.Is(readErr, os.ErrDeadlineExceeded):
		return 0, fmt.Errorf("server did not start within %v; see %s", readyTimeout, logPath)
	case reply == "":
		return 0, fmt.Errorf("server exited during start-up; see %s", logPath)
	}

	return 0, errors.New(strings.TrimPrefix(reply, "error: "))
}

func (a *app) daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon name",
		Short:  "Run a share's server; started by mount",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDaemon(cmd.Context(), args, os.NewFile(readyFD, "ready"))
		},
	}
}

// runDaemon starts the server named in args, reports the start-up on ready
// ("ok <port>" or "error: <message>", then closes it) and serves until a
// stop request can be honored.
func (a *app) runDaemon(ctx context.Context, args []string, ready io.WriteCloser) error {
	report := func(format string, v ...any) {
		_, _ = fmt.Fprintf(ready, format+"\n", v...)
		_ = ready.Close()
	}
	srv, m, err := a.startDaemon(ctx, args)
	if err != nil {
		report("error: %v", err)

		return err
	}
	report("ok %d", srv.Port())
	fs.Logf(nil, "Serving %s for %s on 127.0.0.1:%d", m.URL, m.Mountpoint, srv.Port())
	serveUntilStopped(ctx, srv, m, false)
	fs.Logf(nil, "Stopped")

	return nil
}

func (a *app) startDaemon(ctx context.Context, args []string) (*server.Server, *config.Mount, error) {
	m, err := a.mount(args)
	if err != nil {
		return nil, nil, err
	}
	setupLogging(ctx, m.LogPath(), a.verbose)
	var password string
	if m.User != "" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("reading the password from stdin: %w", err)
		}
		password = strings.TrimSuffix(line, "\n")
	}
	srv, err := server.Start(ctx, m, password)

	return srv, m, err
}

func (a *app) mountForeground(ctx context.Context, m *config.Mount) error {
	s, err := state(ctx, m)
	if err != nil {
		return err
	}
	switch {
	case s.queryErr != nil:
		return notAnsweringError(m, s.queryErr)
	case s.status != nil:
		return fmt.Errorf(
			"%s already has a server running (pid %d); stop it with: davdrive unmount %s",
			m.Name,
			s.status.PID,
			m.Name,
		)
	case s.mounted:
		return staleMountError(m)
	}
	if err := os.MkdirAll(m.Mountpoint, mountpointPerm); err != nil {
		return err
	}
	password, err := m.Password(ctx)
	if err != nil {
		return err
	}
	setupLogging(ctx, "", a.verbose)
	srv, err := server.Start(ctx, m, password)
	if err != nil {
		return err
	}
	if err := mountNFS(srv.Port(), m.Mountpoint, m.MountOptions); err != nil {
		srv.Stop(ctx)

		return err
	}
	fs.Logf(nil, "Mounted %s at %s; press Ctrl-C to unmount", m.Name, m.Mountpoint)
	serveUntilStopped(ctx, srv, m, true)

	return nil
}
