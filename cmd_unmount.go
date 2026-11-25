package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/platform"
	"github.com/ivuorinen/davdrive/internal/server"
)

func (a *app) unmountCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "unmount [name]",
		Aliases: []string{"umount"},
		Short:   "Unmount a share and stop its server once pending uploads finish",
		Long: `Unmount a share and stop its server once pending uploads finish.

The command waits while the server uploads; Ctrl-C stops waiting, and the
server finishes in the background.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.mount(args)
			if err != nil {
				return err
			}

			return unmount(cmd.Context(), m, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "unmount even while files are open, or when the server has died")

	return cmd
}

// queryFailureLimit is how many status queries in a row may fail before
// stopServer gives up waiting. The control socket keeps answering while
// uploads run, so a run of failures means the server is stuck, not busy.
const queryFailureLimit = 10

// unmount clears m's mount even when its server does not answer: that is
// exactly when --force is needed.
func unmount(ctx context.Context, m *config.Mount, force bool) error {
	s, err := state(ctx, m)
	if err != nil {
		return err
	}
	if s.status == nil && s.queryErr == nil && !s.mounted {
		fmt.Printf("%s is not mounted\n", m.Name)

		return nil
	}
	if s.mounted {
		if err := unmountNFS(platform.Canonical(m.Mountpoint), force, true); err != nil {
			if force {
				return err
			}

			return fmt.Errorf("%w\nclose the files open under %s, or retry with --force", err, m.Mountpoint)
		}
	}
	switch {
	case s.queryErr != nil:
		return notAnsweringAfterUnmount(m, s.queryErr)
	case s.status != nil:
		return stopServer(ctx, m, s.status.PID)
	}
	fmt.Printf("Unmounted %s\n", m.Name)

	return nil
}

func notAnsweringAfterUnmount(m *config.Mount, err error) error {
	return fmt.Errorf("%s is not mounted, but its server does not answer (%v) and may still be running; "+
		"find it with: pgrep -fl 'davdrive daemon'", m.Name, err)
}

// stopServer signals the daemon and waits until its control socket goes
// away, reporting the uploads it is still finishing.
func stopServer(ctx context.Context, m *config.Mount, pid int) error {
	// FindProcess always succeeds on Unix. Signal, unlike kill(2), refuses
	// pid 0 and -1, which would reach this process group or every process
	// of the user: pid comes from the control socket's reply.
	proc, _ := os.FindProcess(pid)
	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("stopping the server (pid %d): %w", pid, err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	return waitForExit(ctx, m)
}

// waitForExit polls the control socket until the server is gone,
// reporting pending uploads meanwhile. Ctrl-C (ctx done) only stops the
// waiting; the server finishes in the background. After queryFailureLimit
// failed queries in a row it gives up and reports the server as not
// answering.
func waitForExit(ctx context.Context, m *config.Mount) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var reported, failures int
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("\nThe server finishes its uploads in the background; see: davdrive status %s\n", m.Name)

			return nil
		case <-tick.C:
		}
		st, err := server.Query(ctx, m.SocketPath())
		switch {
		case errors.Is(err, server.ErrNotRunning):
			fmt.Printf("Unmounted %s\n", m.Name)

			return nil
		case err != nil:
			if failures++; failures >= queryFailureLimit {
				return notAnsweringAfterUnmount(m, err)
			}

			continue
		}
		failures = 0
		reported = reportPending(st, reported)
	}
}

// reportPending prints the pending upload count when it changed to a
// non-zero value since last, and returns the count last printed.
func reportPending(st *server.Status, last int) int {
	pending := st.UploadsInProgress + st.UploadsQueued
	if pending == 0 || pending == last {
		return last
	}
	fmt.Printf("Waiting for %d upload(s) to finish...\n", pending)

	return pending
}
