package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"

	"github.com/ivuorinen/davdrive/internal/config"
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "status [name...]",
		Short:             "Show each share's mount, server, uploads and cache",
		ValidArgsFunction: a.completeNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(a.configPath)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = cfg.Names()
			}
			for i, name := range names {
				m, err := cfg.Mount(name)
				if err != nil {
					return err
				}
				s, err := state(cmd.Context(), m)
				if err != nil {
					return err
				}
				if i > 0 {
					fmt.Println()
				}
				printStatus(os.Stdout, m, s)
			}

			return nil
		},
	}
}

func printStatus(w io.Writer, m *config.Mount, s mountState) {
	st := s.status
	switch {
	case s.queryErr != nil:
		mount := "not mounted"
		if s.mounted {
			mount = "mounted at " + m.Mountpoint
		}
		_, _ = fmt.Fprintf(w, "%s: %s, but its server does not answer (%v)\n  fix:     davdrive unmount --force %s\n",
			m.Name, mount, s.queryErr, m.Name)

		return
	case st == nil && !s.mounted:
		_, _ = fmt.Fprintf(w, "%s: not mounted (%s)\n", m.Name, m.Mountpoint)

		return
	case st == nil:
		_, _ = fmt.Fprintf(
			w,
			"%s: stale mount at %s, its server is not running\n  fix:     davdrive unmount --force %s\n",
			m.Name,
			m.Mountpoint,
			m.Name,
		)

		return
	case st.Stopping:
		_, _ = fmt.Fprintf(w, "%s: stopping, finishing uploads\n", m.Name)
	case s.mounted:
		_, _ = fmt.Fprintf(w, "%s: mounted at %s\n", m.Name, m.Mountpoint)
	default:
		_, _ = fmt.Fprintf(w, "%s: server running, but %s is not mounted\n"+
			"  fix:     davdrive mount %s\n", m.Name, m.Mountpoint, m.Name)
	}
	_, _ = fmt.Fprintf(w, "  remote:  %s\n", st.URL)
	_, _ = fmt.Fprintf(
		w,
		"  server:  pid %d, port %d, up %s\n",
		st.PID,
		st.Port,
		time.Since(st.Started).Round(time.Second),
	)
	_, _ = fmt.Fprintf(w, "  uploads: %d in progress, %d queued, %d failed files\n",
		st.UploadsInProgress, st.UploadsQueued, st.ErroredFiles)
	_, _ = fmt.Fprintf(w, "  cache:   %s in %d files, limit %s\n",
		fs.SizeSuffix(st.CacheBytes).ByteUnit(), st.CacheFiles, fs.SizeSuffix(st.CacheMaxBytes).ByteUnit())
	_, _ = fmt.Fprintf(w, "  log:     %s\n", m.LogPath())
}
