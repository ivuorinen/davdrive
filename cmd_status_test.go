package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/server"
)

// TestPrintStatus covers every state status can report for a share.
func TestPrintStatus(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	m := &config.Mount{Name: "k", Mountpoint: "/mnt/k"}
	st := &server.Status{
		URL:               "https://dav.example/",
		PID:               4242,
		Port:              2049,
		Started:           time.Now().Add(-90 * time.Second),
		UploadsInProgress: 1,
		UploadsQueued:     2,
		ErroredFiles:      3,
		CacheFiles:        4,
		CacheBytes:        5 << 20,
		CacheMaxBytes:     2 << 30,
	}
	stopping := *st
	stopping.Stopping = true
	details := "  remote:  https://dav.example/\n" +
		"  server:  pid 4242, port 2049, up 1m30s\n" +
		"  uploads: 1 in progress, 2 queued, 3 failed files\n" +
		"  cache:   5 MiB in 4 files, limit 2 GiB\n" +
		"  log:     /state/davdrive/k.log\n"
	errQuery := errors.New("status request: 500 Internal Server Error")
	for _, tc := range []struct {
		name string
		s    mountState
		want string
	}{
		{"server does not answer, mounted", mountState{mounted: true, queryErr: errQuery},
			"k: mounted at /mnt/k, but its server does not answer (" + errQuery.Error() + ")\n" +
				"  fix:     davdrive unmount --force k\n"},
		{"server does not answer, not mounted", mountState{queryErr: errQuery},
			"k: not mounted, but its server does not answer (" + errQuery.Error() + ")\n" +
				"  fix:     davdrive unmount --force k\n"},
		{"neither server nor mount", mountState{}, "k: not mounted (/mnt/k)\n"},
		{"mount without server", mountState{mounted: true},
			"k: stale mount at /mnt/k, its server is not running\n  fix:     davdrive unmount --force k\n"},
		{"stopping", mountState{status: &stopping}, "k: stopping, finishing uploads\n" + details},
		{"mounted", mountState{status: st, mounted: true}, "k: mounted at /mnt/k\n" + details},
		{"server without mount", mountState{status: st},
			"k: server running, but /mnt/k is not mounted\n  fix:     davdrive mount k\n" + details},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printStatus(&out, m, tc.s)
			if out.String() != tc.want {
				t.Errorf("printStatus =\n%s\nwant\n%s", out.String(), tc.want)
			}
		})
	}
}

// TestStatusCommand checks status lists every share, or the named ones in
// order, separated by a blank line.
func TestStatusCommand(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(t, e.share("k"), e.share("a"))
	a, k := e.mount(t, "a"), e.mount(t, "k")
	e.mounts.set(k.Mountpoint)
	serveControl(t, a.SocketPath(), statusReply(server.Status{URL: e.dav, PID: 7, Port: 2049, Started: time.Now()}))
	aOut := "a: server running, but " + a.Mountpoint + " is not mounted\n"
	kOut := "k: stale mount at " + k.Mountpoint + ", its server is not running\n" +
		"  fix:     davdrive unmount --force k\n"

	out, err := e.run(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, aOut) || !strings.HasSuffix(out, "\n\n"+kOut) {
		t.Errorf("status =\n%s\nwant a, a blank line, then k", out)
	}

	out, err = e.run(t, "status", "k", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, kOut+"\n"+aOut) {
		t.Errorf("status k a =\n%s\nwant k, a blank line, then a", out)
	}
}
