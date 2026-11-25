package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/server"
)

// TestParseReady covers every reply the daemon can leave on the ready pipe.
func TestParseReady(t *testing.T) {
	const log = "/state/k.log"
	for _, tc := range []struct {
		name    string
		out     string
		readErr error
		port    int
		wantErr string
	}{
		{"ready", "ok 54321\n", nil, 54321, ""},
		{"start-up error", "error: listing https://dav.example: 401\n", nil, 0, "listing https://dav.example: 401"},
		{"died silently", "", nil, 0, "exited during start-up; see " + log},
		{"timed out", "", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), 0, "did not start within"},
		{"port zero", "ok 0\n", nil, 0, "ok 0"},
		{"port out of range", "ok 70000\n", nil, 0, "ok 70000"},
	} {
		port, err := parseReady([]byte(tc.out), tc.readErr, log)
		switch {
		case tc.wantErr == "" && (err != nil || port != tc.port):
			t.Errorf("%s: = %d, %v; want %d", tc.name, port, err, tc.port)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestMountResume covers mount on a share that already has a server, a
// mount, or both.
func TestMountResume(t *testing.T) {
	errMount := errors.New("mount_nfs: permission denied")
	for _, tc := range []struct {
		name       string
		reply      http.HandlerFunc
		mounted    bool
		mountErr   error
		wantOut    func(m *config.Mount) string
		wantErr    string
		mountPorts []int // port of each expected mount call
	}{
		{
			name:    "already mounted",
			reply:   statusReply(server.Status{Port: 2049}),
			mounted: true,
			wantOut: func(m *config.Mount) string { return "k is already mounted at " + m.Mountpoint + "\n" },
		},
		{
			name:    "stopping",
			reply:   statusReply(server.Status{Port: 2049, Stopping: true}),
			wantErr: "k is stopping; wait for it to finish (davdrive status k)",
		},
		{
			name:       "server outlived its mount",
			reply:      statusReply(server.Status{Port: 4321}),
			mountPorts: []int{4321},
			wantOut: func(m *config.Mount) string {
				return "Mounted k at " + m.Mountpoint + " (log: " + m.LogPath() + ")\n"
			},
		},
		{
			name:       "server outlived its mount, mounting fails",
			reply:      statusReply(server.Status{Port: 4321}),
			mountErr:   errMount,
			mountPorts: []int{4321},
			wantErr:    errMount.Error(),
		},
		{
			name:    "stale mount refused",
			mounted: true,
			wantErr: "is mounted but no server is running for k; remove the stale mount with: davdrive unmount --force k",
		},
		{
			name:    "server not answering refused",
			reply:   brokenReply,
			wantErr: "the server for k does not answer (status request: 500 Internal Server Error)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			m := e.mount(t, "k")
			if tc.reply != nil {
				serveControl(t, m.SocketPath(), tc.reply)
			}
			if tc.mounted {
				e.mounts.set(m.Mountpoint)
			}
			if tc.mountErr != nil {
				e.mounts.queue(&e.mounts.mountErrs, tc.mountErr)
			}
			out, err := e.run(t, "mount")
			checkErr(t, err, tc.wantErr)
			if tc.wantOut != nil && out != tc.wantOut(m) {
				t.Errorf("stdout = %q, want %q", out, tc.wantOut(m))
			}
			mounts, _ := e.mounts.calls()
			checkMounts(t, mounts, m.Mountpoint, tc.mountPorts)
		})
	}
}

// checkErr fails unless err contains want, or is nil when want is empty.
func checkErr(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("err = %v, want none", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Fatalf("err = %v, want one containing %q", err, want)
	}
}

// refusal sets up a share that mount must refuse before it starts a
// server, and returns the error it must report.
type refusal struct {
	name  string
	setup func(t *testing.T, e *testEnv) string
}

// startRefusals apply to mount with and without -f: each stops the mount
// before a server runs.
var startRefusals = []refusal{
	{"mountpoint cannot be created", func(t *testing.T, e *testEnv) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s := e.share("k")
		s.mountpoint = filepath.Join(file, "mnt")
		e.writeConfig(t, s)

		return "not a directory"
	}},
	{"password_command fails", func(t *testing.T, e *testEnv) string {
		t.Helper()
		s := e.share("k")
		s.passwordCommand = "exit 3"
		e.writeConfig(t, s)

		return "password_command: exit status 3"
	}},
	{"wrong password", func(t *testing.T, e *testEnv) string {
		t.Helper()
		s := e.share("k")
		s.passwordCommand = "printf wrong"
		e.writeConfig(t, s)

		return "listing " + e.dav
	}},
}

// TestMountBackgroundRefusals checks mount reports what keeps the daemon
// from starting, and leaves no daemon behind.
func TestMountBackgroundRefusals(t *testing.T) {
	cases := slices.Concat(startRefusals, []refusal{{
		"log file cannot be opened", func(t *testing.T, e *testEnv) string {
			t.Helper()
			if err := os.MkdirAll(e.mount(t, "k").LogPath(), 0o700); err != nil {
				t.Fatal(err)
			}

			return "is a directory"
		},
	}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			want := tc.setup(t, e)
			m := e.mount(t, "k")
			reapDaemon(t, m)
			_, err := e.run(t, "mount")
			checkErr(t, err, want)
			if !notRunning(t.Context(), m) {
				t.Error("a daemon is running after a failed mount")
			}
			mounts, _ := e.mounts.calls()
			checkMounts(t, mounts, m.Mountpoint, nil)
		})
	}
}

// TestMountBackground mounts a share with a real daemon, checks its
// status, and unmounts it again, which stops the daemon.
func TestMountBackground(t *testing.T) {
	e := newEnv(t)
	m := e.mount(t, "k")
	reapDaemon(t, m)
	st := mountInBackground(t, e, m)

	out, err := e.run(t, "mount")
	if err != nil || out != "k is already mounted at "+m.Mountpoint+"\n" {
		t.Errorf("second mount = %q, %v; want already mounted", out, err)
	}
	checkStatusOutput(t, e, m, st)

	out, err = e.run(t, "unmount")
	if err != nil || out != unmountedK {
		t.Errorf("unmount = %q, %v; want Unmounted k", out, err)
	}
	if e.mounts.isMountedAt(m.Mountpoint) || !notRunning(t.Context(), m) {
		t.Error("the share is still mounted or its daemon still answers after unmount")
	}
	if log, err := os.ReadFile(m.LogPath()); err != nil || !strings.Contains(string(log), "Serving "+e.dav) {
		t.Errorf("daemon log = %q, %v; want the serving line", log, err)
	}
}

// mountInBackground runs mount -v for m and checks that a daemon of its
// own serves the share and that the share is mounted from it. It returns
// the daemon's status.
func mountInBackground(t *testing.T, e *testEnv, m *config.Mount) *server.Status {
	t.Helper()
	out, err := e.run(t, "mount", "-v")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Mounted k at " + m.Mountpoint + " (log: " + m.LogPath() + ")\n"; out != want {
		t.Errorf("mount printed %q, want %q", out, want)
	}
	st, err := server.Query(t.Context(), m.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	mounts, _ := e.mounts.calls()
	checkMounts(t, mounts, m.Mountpoint, []int{st.Port})
	if st.PID == os.Getpid() || st.URL != e.dav {
		t.Errorf("daemon status = %+v, want its own pid, url %s", st, e.dav)
	}
	if fi, err := os.Stat(m.Mountpoint); err != nil || !fi.IsDir() {
		t.Errorf("mountpoint not created: %v", err)
	}

	return st
}

// checkStatusOutput runs status for the mounted share m, served by the
// daemon that reported st.
func checkStatusOutput(t *testing.T, e *testEnv, m *config.Mount, st *server.Status) {
	t.Helper()
	out, err := e.run(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"k: mounted at " + m.Mountpoint + "\n",
		"  remote:  " + e.dav + "\n",
		"  server:  pid " + strconv.Itoa(st.PID) + ", port " + strconv.Itoa(st.Port) + ", up ",
		"  uploads: 0 in progress, 0 queued, 0 failed files\n",
		"  log:     " + m.LogPath() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status = %q, want it to contain %q", out, want)
		}
	}
}

// TestMountBackgroundAttachFailure checks that a daemon whose share cannot
// be mounted is stopped rather than left running unseen.
func TestMountBackgroundAttachFailure(t *testing.T) {
	e := newEnv(t)
	m := e.mount(t, "k")
	reapDaemon(t, m)
	e.mounts.queue(&e.mounts.mountErrs, errors.New("mount_nfs: permission denied"))

	_, err := e.run(t, "mount")
	checkErr(t, err, "mount_nfs: permission denied")
	waitFor(t, "the daemon to stop", func() bool { return notRunning(t.Context(), m) })
}

// TestMountForegroundRefusals checks every reason mount -f gives up, and
// that it leaves no server running.
func TestMountForegroundRefusals(t *testing.T) {
	cases := slices.Concat(startRefusals, []refusal{
		{"server not answering", func(t *testing.T, e *testEnv) string {
			t.Helper()
			serveControl(t, e.mount(t, "k").SocketPath(), brokenReply)

			return "the server for k does not answer"
		}},
		{"server already running", func(t *testing.T, e *testEnv) string {
			t.Helper()
			serveControl(t, e.mount(t, "k").SocketPath(), statusReply(server.Status{PID: 4242}))

			return "k already has a server running (pid 4242); stop it with: davdrive unmount k"
		}},
		{"stale mount in the way", func(t *testing.T, e *testEnv) string {
			t.Helper()
			e.mounts.set(e.mount(t, "k").Mountpoint)

			return "is mounted but no server is running for k"
		}},
		{"mounting fails", func(t *testing.T, e *testEnv) string {
			t.Helper()
			e.mounts.queue(&e.mounts.mountErrs, errors.New("mount_nfs: permission denied"))

			return "mount_nfs: permission denied"
		}},
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			captureLogs(t)
			want := tc.setup(t, e)
			m := e.mount(t, "k")
			err := execute(t.Context(), "mount", "-f")
			checkErr(t, err, want)
			if st, err := server.Query(t.Context(), m.SocketPath()); err == nil && st.PID == os.Getpid() {
				t.Error("mount -f left its server running")
			}
		})
	}
}

// TestMountForeground runs mount -f until Ctrl-C, which must unmount the
// share and stop the server.
func TestMountForeground(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t)
	m := e.mount(t, "k")

	done := make(chan error, 1)
	go func() { done <- execute(t.Context(), "mount", "--foreground") }()
	waitFor(t, "the share to be mounted", func() bool { return e.mounts.isMountedAt(m.Mountpoint) })
	st, err := server.Query(t.Context(), m.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	mounts, _ := e.mounts.calls()
	checkMounts(t, mounts, m.Mountpoint, []int{st.Port})
	if st.PID != os.Getpid() {
		t.Errorf("server pid %d, want this process", st.PID)
	}

	if err := interruptUntilReturns(t, "mount -f returns", done); err != nil {
		t.Errorf("mount -f = %v, want nil", err)
	}
	_, unmounts := e.mounts.calls()
	checkUnmounts(t, unmounts, m.Mountpoint, []bool{false})
	if !notRunning(t.Context(), m) {
		t.Error("the server still answers after mount -f returned")
	}
	if !logs.contains("press Ctrl-C to unmount") {
		t.Errorf("log = %q, want the Ctrl-C hint", logs)
	}
}

// withStdin makes os.Stdin read content for the rest of the test.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

// TestRunDaemon runs the daemon in this process: it reports its port on
// the ready pipe, serves, and stops on SIGINT once the share is unmounted.
func TestRunDaemon(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t)
	m := e.mount(t, "k")
	withStdin(t, davPassword+"\n")
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readyR.Close() })

	done := make(chan error, 1)
	a := &app{configPath: e.cfg}
	go func() { done <- a.runDaemon(ownConfig(t.Context()), []string{"k"}, readyW) }()
	ready, err := io.ReadAll(readyR)
	if err != nil {
		t.Fatal(err)
	}
	port, err := parseReady(ready, nil, m.LogPath())
	if err != nil {
		t.Fatalf("ready = %q: %v", ready, err)
	}
	st, err := server.Query(t.Context(), m.SocketPath())
	if err != nil || st.Port != port {
		t.Fatalf("status = %+v, %v; want port %d", st, err, port)
	}

	if err := interruptUntilReturns(t, "the daemon returns", done); err != nil {
		t.Errorf("runDaemon = %v, want nil", err)
	}
	if !notRunning(t.Context(), m) {
		t.Error("the server still answers after the daemon returned")
	}
	for _, want := range []string{"Serving " + e.dav + " for " + m.Mountpoint, "Stopped"} {
		if !logs.contains(want) {
			t.Errorf("log = %q, want %q", logs, want)
		}
	}
}

// TestRunDaemonStartFailure checks the daemon reports a failed start on
// the ready pipe, where mount reads it, as well as returning it.
func TestRunDaemonStartFailure(t *testing.T) {
	for _, tc := range []struct {
		name, mount, stdin, want string
		noUser                   bool
	}{
		{"unknown share", "x", davPassword + "\n", `no mount "x"`, false},
		{"no password on stdin", "k", "", "reading the password from stdin: EOF", false},
		{"wrong password", "k", "wrong\n", "listing ", false},
		// Without a user the daemon reads no password and connects
		// anonymously, which this server refuses.
		{"no user", "k", "", "listing ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.noUser {
				s := e.share("k")
				s.user = ""
				e.writeConfig(t, s)
			}
			captureLogs(t)
			withStdin(t, tc.stdin)
			readyR, readyW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = readyR.Close() })

			a := &app{configPath: e.cfg}
			runErr := a.runDaemon(ownConfig(t.Context()), []string{tc.mount}, readyW)
			checkErr(t, runErr, tc.want)
			ready, err := io.ReadAll(readyR)
			if err != nil {
				t.Fatal(err)
			}
			if want := "error: " + runErr.Error() + "\n"; string(ready) != want {
				t.Errorf("ready = %q, want %q", ready, want)
			}
		})
	}
}

// TestSpawnDaemonResourceFailures checks spawnDaemon reports what kept it
// from starting the daemon, and leaves none running.
func TestSpawnDaemonResourceFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"state directory cannot be created", func(t *testing.T) {
			t.Helper()
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_STATE_HOME", file)
		}, "not a directory"},
		// The log file takes the only free descriptor, so the ready pipe
		// cannot be made.
		{"no descriptor for the ready pipe", func(t *testing.T) {
			t.Helper()
			exhaustFDs(t, 1)
		}, "too many open files"},
		// The log file and the ready pipe take all three, so the daemon's
		// stdin cannot be made.
		{"no descriptor to start the daemon", func(t *testing.T) {
			t.Helper()
			exhaustFDs(t, 3)
		}, "too many open files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			m := e.mount(t, "k")
			reapDaemon(t, m)
			a := &app{configPath: e.cfg}
			tc.setup(t)
			_, _, err := a.spawnDaemon(m, davPassword)
			checkErr(t, err, tc.want)
		})
	}
}
