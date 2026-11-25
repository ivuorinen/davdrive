package main

import (
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/ivuorinen/davdrive/internal/server"
)

// TestUnmount covers unmount on shares without a live server to stop.
func TestUnmount(t *testing.T) {
	errBusy := errors.New("umount: resource busy")
	notAnswering := "k is not mounted, but its server does not answer (status request: 500 Internal Server Error) " +
		"and may still be running; find it with: pgrep -fl 'davdrive daemon'"
	for _, tc := range []struct {
		name       string
		args       []string
		mounted    bool
		broken     bool // a control socket that answers with errors
		unmountErr error
		wantOut    string
		wantErr    string
		wantForce  []bool // force flag of each unmount call
	}{
		{name: "nothing to do", wantOut: "k is not mounted\n"},
		{name: "stale mount cleared", mounted: true, wantOut: unmountedK, wantForce: []bool{false}},
		{name: "stale mount cleared by force", args: []string{"--force"}, mounted: true, wantOut: unmountedK,
			wantForce: []bool{true}},
		{name: "busy", mounted: true, unmountErr: errBusy, wantForce: []bool{false},
			wantErr: errBusy.Error() + "\nclose the files open under "},
		{name: "busy, forced", args: []string{"--force"}, mounted: true, unmountErr: errBusy,
			wantForce: []bool{true}, wantErr: errBusy.Error()},
		{name: "server not answering", broken: true, wantErr: notAnswering},
		{name: "server does not answer, mounted", mounted: true, broken: true, wantErr: notAnswering,
			wantForce: []bool{false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			m := e.mount(t, "k")
			if tc.mounted {
				e.mounts.set(m.Mountpoint)
			}
			if tc.broken {
				serveControl(t, m.SocketPath(), brokenReply)
			}
			if tc.unmountErr != nil {
				e.mounts.queue(&e.mounts.unmountErrs, tc.unmountErr)
			}
			out, err := e.run(t, append([]string{"umount"}, tc.args...)...)
			checkErr(t, err, tc.wantErr)
			if out != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out, tc.wantOut)
			}
			_, unmounts := e.mounts.calls()
			checkUnmounts(t, unmounts, m.Mountpoint, tc.wantForce)
			if e.mounts.isMountedAt(m.Mountpoint) != (tc.unmountErr != nil) {
				t.Errorf("mounted = %v after unmount", e.mounts.isMountedAt(m.Mountpoint))
			}
		})
	}
}

// fakeDaemon is a stand-in process for a daemon: unmount signals its pid.
// It reports whether the process ended on SIGINT.
func fakeDaemon(t *testing.T) (pid int, interrupted func() bool) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var sigint bool
	wait := func() bool {
		once.Do(func() {
			_ = cmd.Wait()
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			sigint = ok && ws.Signaled() && ws.Signal() == syscall.SIGINT
		})

		return sigint
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		wait()
	})

	return cmd.Process.Pid, wait
}

// scriptedControl answers the control socket with replies[i] for the i-th
// request. The request after the last reply fails and makes the daemon go
// away, so the last reply is delivered in full.
func scriptedControl(t *testing.T, socket string, replies []http.HandlerFunc) {
	t.Helper()
	var mu sync.Mutex
	var n int
	var stop func()
	mu.Lock()
	defer mu.Unlock()
	stop = serveControl(t, socket, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if n < len(replies) {
			replies[n](w, r)
			n++

			return
		}
		brokenReply(w, r)
		go stop()
	})
}

// TestUnmountWaitsForUploads stops a daemon that is still uploading and
// expects unmount to report the pending uploads until the daemon is gone.
func TestUnmountWaitsForUploads(t *testing.T) {
	e := newEnv(t)
	m := e.mount(t, "k")
	e.mounts.set(m.Mountpoint)
	pid, interrupted := fakeDaemon(t)
	st := func(inProgress, queued int) http.HandlerFunc {
		return statusReply(server.Status{PID: pid, UploadsInProgress: inProgress, UploadsQueued: queued})
	}
	scriptedControl(t, m.SocketPath(), []http.HandlerFunc{
		st(1, 1), // unmount's own status query
		st(0, 0),
		st(1, 1),
		st(2, 0), // unchanged count: not printed again
		brokenReply,
		st(0, 1),
	})

	out, err := e.run(t, "unmount")
	if err != nil {
		t.Fatal(err)
	}
	want := "Waiting for 2 upload(s) to finish...\nWaiting for 1 upload(s) to finish...\nUnmounted k\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if !interrupted() {
		t.Error("the daemon was not sent SIGINT")
	}
	if e.mounts.isMountedAt(m.Mountpoint) {
		t.Error("the share is still mounted")
	}
}

// TestUnmountCtrlC checks Ctrl-C while unmount waits stops only the
// waiting, and says where to follow the uploads.
func TestUnmountCtrlC(t *testing.T) {
	e := newEnv(t)
	m := e.mount(t, "k")
	pid, interrupted := fakeDaemon(t)
	serveControl(t, m.SocketPath(), statusReply(server.Status{PID: pid, UploadsQueued: 3}))

	var err error
	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() { done <- execute(t.Context(), "unmount") }()
		err = interruptUntilReturns(t, "unmount returns", done)
	})
	if err != nil {
		t.Errorf("unmount = %v, want nil", err)
	}
	const want = "\nThe server finishes its uploads in the background; see: davdrive status k\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("stdout = %q, want it to end with %q", out, want)
	}
	if !interrupted() {
		t.Error("the daemon was not sent SIGINT")
	}
}

// TestUnmountServerStuck gives up waiting once the daemon fails
// queryFailureLimit status queries in a row.
func TestUnmountServerStuck(t *testing.T) {
	e := newEnv(t)
	m := e.mount(t, "k")
	pid, _ := fakeDaemon(t)
	replies := make([]http.HandlerFunc, 1, 1+queryFailureLimit)
	replies[0] = statusReply(server.Status{PID: pid})
	for range queryFailureLimit {
		replies = append(replies, brokenReply)
	}
	scriptedControl(t, m.SocketPath(), replies)

	_, err := e.run(t, "unmount")
	checkErr(t, err, "k is not mounted, but its server does not answer (status request: 500")
}

// TestUnmountSignalFails reports a daemon pid that cannot be signaled: a
// daemon that has exited, and a reply with no pid, where kill(2) would
// reach unmount's own process group instead.
func TestUnmountSignalFails(t *testing.T) {
	gone := exec.CommandContext(t.Context(), "true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{gone.ProcessState.Pid(), 0} {
		t.Run("pid "+strconv.Itoa(pid), func(t *testing.T) {
			e := newEnv(t)
			m := e.mount(t, "k")
			serveControl(t, m.SocketPath(), statusReply(server.Status{PID: pid}))

			_, err := e.run(t, "unmount")
			checkErr(t, err, "stopping the server (pid "+strconv.Itoa(pid)+")")
		})
	}
}
