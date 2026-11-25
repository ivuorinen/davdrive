package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rclone/rclone/fs"
	rlog "github.com/rclone/rclone/fs/log"
	"github.com/spf13/cobra"
	nfs "github.com/willscott/go-nfs"

	"github.com/ivuorinen/davdrive/internal/config"
	"github.com/ivuorinen/davdrive/internal/server"
)

// daemonEnv makes this test binary act as davdrive itself: mount spawns
// os.Executable(), which under test is this binary, as "daemon ...".
const daemonEnv = "DAVDRIVE_TEST_DAEMON"

func TestMain(m *testing.M) {
	if os.Getenv(daemonEnv) != "" {
		if err := newRootCmd().Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestVersion checks what --version prints for a GoReleaser build and for
// a build from source.
func TestVersion(t *testing.T) {
	fallback := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		fallback = bi.Main.Version
	}
	for _, tc := range []struct {
		name, stamped, want string
	}{
		{"stamped by GoReleaser", "v1.2.3", "v1.2.3"},
		{"built from source", "", fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := version
			version = tc.stamped
			t.Cleanup(func() { version = old })
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs([]string{"--version"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if got, want := out.String(), "davdrive version "+tc.want+"\n"; got != want {
				t.Errorf("--version printed %q, want %q", got, want)
			}
		})
	}
}

// TestCheckDirs refuses to run when the state directory would be relative.
func TestCheckDirs(t *testing.T) {
	newEnv(t)
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	err := execute(t.Context(), "status")
	if err == nil || !strings.Contains(err.Error(), "cannot determine the home directory") {
		t.Fatalf("err = %v, want the home directory error", err)
	}
}

func TestCompleteNames(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(t, e.share("k"), e.share("a"))
	a := &app{configPath: e.cfg}
	names, dir := a.completeNames(nil, nil, "")
	if !slices.Equal(names, []string{"a", "k"}) || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completeNames = %v, %v; want [a k], no file completion", names, dir)
	}

	a.configPath = filepath.Join(t.TempDir(), "missing.toml")
	names, dir = a.completeNames(nil, nil, "")
	if names != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completeNames without config = %v, %v; want nothing, no file completion", names, dir)
	}
}

// TestCompletionThroughCobra checks the names reach the shell completion
// protocol of every command that takes one.
func TestCompletionThroughCobra(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(t, e.share("k"), e.share("a"))
	for _, cmd := range nameCommands {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{cobra.ShellCompRequestCmd, cmd, ""})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); !strings.HasPrefix(got, "a\nk\n:4\n") {
			t.Errorf("%s completion = %q, want a, k and no file completion", cmd, got)
		}
	}
}

// TestSetupLogging checks the rclone and go-nfs levels each -v level sets,
// and which messages rclone's log handler then lets through.
func TestSetupLogging(t *testing.T) {
	for _, tc := range []struct {
		verbose   int
		passed    []fs.LogLevel
		dropped   []fs.LogLevel
		nfsLevel  nfs.LogLevel
		rcloneLvl fs.LogLevel
	}{
		{
			0,
			[]fs.LogLevel{fs.LogLevelError, fs.LogLevelNotice},
			[]fs.LogLevel{fs.LogLevelInfo, fs.LogLevelDebug},
			nfs.WarnLevel, fs.LogLevelNotice,
		},
		{
			1,
			[]fs.LogLevel{fs.LogLevelNotice, fs.LogLevelInfo},
			[]fs.LogLevel{fs.LogLevelDebug},
			nfs.WarnLevel, fs.LogLevelInfo,
		},
		{2, []fs.LogLevel{fs.LogLevelInfo, fs.LogLevelDebug}, nil, nfs.DebugLevel, fs.LogLevelDebug},
	} {
		t.Run("v="+strconv.Itoa(tc.verbose), func(t *testing.T) {
			logs := captureLogs(t)
			ctx, ci := fs.AddConfig(t.Context())
			setupLogging(ctx, "", tc.verbose)
			if ci.LogLevel != tc.rcloneLvl {
				t.Errorf("rclone level = %v, want %v", ci.LogLevel, tc.rcloneLvl)
			}
			if got := nfs.Log.GetLevel(); got != tc.nfsLevel {
				t.Errorf("go-nfs level = %v, want %v", got, tc.nfsLevel)
			}
			for _, l := range tc.passed {
				fs.LogPrintf(l, nil, "message at %v", l)
				if !logs.contains("message at " + l.String()) {
					t.Errorf("a %v message was dropped", l)
				}
			}
			for _, l := range tc.dropped {
				fs.LogPrintf(l, nil, "message at %v", l)
				if logs.contains("message at " + l.String()) {
					t.Errorf("a %v message was logged", l)
				}
			}
		})
	}
}

// TestSetupLoggingFile checks that the daemon's log goes to its log file.
func TestSetupLoggingFile(t *testing.T) {
	isolateLogging(t)
	path := filepath.Join(t.TempDir(), "k.log")
	setupLogging(ownConfig(t.Context()), path, 0)
	if rlog.Opt.MaxBackups != 3 || rlog.Opt.MaxSize != 10*fs.Mebi {
		t.Errorf("rotation = %d backups of %v, want 3 of 10Mi", rlog.Opt.MaxBackups, rlog.Opt.MaxSize)
	}
	fs.Logf(nil, "into the log file")
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is in t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "into the log file") {
		t.Errorf("log file = %q, want the message", data)
	}
}

// TestUnmountIfMounted covers each way the unmount before a server stops
// can go, and the verdict on whether stopping is safe.
func TestUnmountIfMounted(t *testing.T) {
	errBusy := errors.New("resource busy")
	errTable := errors.New("getfsstat failed")
	for _, tc := range []struct {
		name        string
		mounted     bool
		tableErrs   []error
		unmountErrs []error
		want        bool
		forces      []bool // force flag of each unmount call
		log         string
	}{
		{"nothing mounted", false, nil, nil, true, nil, ""},
		{"unmounts", true, nil, nil, true, []bool{false}, ""},
		{"busy, forced", true, nil, []error{errBusy}, true, []bool{false, true}, "resource busy; forcing"},
		{"forcing fails", true, nil, []error{errBusy, errBusy}, false, []bool{false, true}, "Forced unmount of"},
		{"mount table unreadable", true, []error{errTable}, nil, false, nil, "Reading the mount table"},
		{"mount table unreadable after", true, []error{nil, errTable}, nil, false, []bool{false}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			f := installFakeMounts(t)
			mp := filepath.Join(t.TempDir(), "mnt")
			if tc.mounted {
				f.set(mp)
			}
			f.queue(&f.tableErrs, tc.tableErrs...)
			f.queue(&f.unmountErrs, tc.unmountErrs...)
			m := &config.Mount{Name: "k", Mountpoint: mp}
			if got := unmountIfMounted(m, true); got != tc.want {
				t.Errorf("unmountIfMounted = %v, want %v", got, tc.want)
			}
			_, unmounts := f.calls()
			checkUnmounts(t, unmounts, mp, tc.forces)
			if tc.log != "" && !logs.contains(tc.log) {
				t.Errorf("log = %q, want %q", logs, tc.log)
			}
		})
	}
}

// startServer starts a real server for m and stops it at the end of the
// test unless the test already did.
func startServer(t *testing.T, m *config.Mount) *server.Server {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	srv, err := server.Start(ctx, m, davPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !notRunning(ctx, m) {
			srv.Stop(ctx)
		}
	})

	return srv
}

// TestServeUntilStoppedWhileMounted checks that a stop request the unmount
// cannot honor keeps the server running, and that it stops once the share
// is unmounted.
func TestServeUntilStoppedWhileMounted(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t)
	m := e.mount(t, "k")
	srv := startServer(t, m)
	e.mounts.set(m.Mountpoint)
	errSudo := errors.New("sudo: a password is required")
	e.mounts.queue(&e.mounts.unmountErrs, errSudo, errSudo)

	done := make(chan struct{})
	go func() {
		serveUntilStopped(t.Context(), srv, m, false)
		close(done)
	}()
	stopped := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	interruptUntil(t, "the server refuses to stop", func() bool { return logs.contains("keeps running") })
	if stopped() {
		t.Fatal("serveUntilStopped returned while the share was still mounted")
	}
	if notRunning(t.Context(), m) {
		t.Fatal("the server stopped while the share was still mounted")
	}
	if !logs.contains("davdrive unmount --force k") {
		t.Errorf("log = %q, want the unmount --force hint", logs)
	}

	interruptUntil(t, "serveUntilStopped returns", stopped)
	if !notRunning(t.Context(), m) {
		t.Error("the server still answers after serveUntilStopped returned")
	}
	if e.mounts.isMountedAt(m.Mountpoint) {
		t.Error("the share is still mounted")
	}
}

// failedServer is a server that has already failed: Done delivers err.
type failedServer struct {
	done    chan error
	stopped chan struct{}
}

func newFailedServer(err error) *failedServer {
	s := &failedServer{done: make(chan error, 1), stopped: make(chan struct{})}
	s.done <- err

	return s
}

func (s *failedServer) Done() <-chan error { return s.done }

func (s *failedServer) Stop(context.Context) { close(s.stopped) }

// TestServeUntilStoppedServerFailure expects a failed server to be
// stopped even though the share cannot be unmounted: it can no longer
// serve the mount either way.
func TestServeUntilStoppedServerFailure(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t)
	m := e.mount(t, "k")
	e.mounts.set(m.Mountpoint)
	errBusy := errors.New("resource busy")
	e.mounts.queue(&e.mounts.unmountErrs, errBusy, errBusy)
	srv := newFailedServer(errors.New("NFS server: listener gone"))

	serveUntilStopped(t.Context(), srv, m, true)

	select {
	case <-srv.stopped:
	default:
		t.Fatal("the failed server was not stopped")
	}
	if want := "Server failed: NFS server: listener gone; unmounting and stopping"; !logs.contains(want) {
		t.Errorf("log = %q, want %q", logs, want)
	}
	_, unmounts := e.mounts.calls()
	checkUnmounts(t, unmounts, m.Mountpoint, []bool{false, true})
	if !e.mounts.isMountedAt(m.Mountpoint) {
		t.Error("the fake mount table lost a mount whose unmount failed")
	}
}

// exhaustFDs lowers the open file limit and fills it, leaving free
// descriptors. The returned func undoes both; so does the test's end.
func exhaustFDs(t *testing.T, free int) (release func()) {
	t.Helper()
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	limit := orig
	limit.Cur = uint64(probe.Fd()) + 32
	_ = probe.Close()
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	release = func() {
		for _, f := range files {
			_ = f.Close()
		}
		files = nil
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(release)
	files = openUntilFull(t)
	if len(files) < free {
		t.Fatal("too few descriptors to free")
	}
	for _, f := range files[len(files)-free:] {
		_ = f.Close()
	}
	files = files[:len(files)-free]

	return release
}

// openUntilFull opens files until the open file limit is reached.
func openUntilFull(t *testing.T) []*os.File {
	t.Helper()
	var files []*os.File
	for {
		f, err := os.Open(os.DevNull)
		if errors.Is(err, syscall.EMFILE) {
			return files
		}
		if err != nil {
			for _, f := range files {
				_ = f.Close()
			}
			t.Fatal(err)
		}
		files = append(files, f)
	}
}

// TestMountTableError checks every command reports an unreadable mount
// table instead of guessing.
func TestMountTableError(t *testing.T) {
	e := newEnv(t)
	cases := make([][]string, 0, 1+len(nameCommands))
	cases = append(cases, []string{"mount", "-f"})
	for _, cmd := range nameCommands {
		cases = append(cases, []string{cmd})
	}
	for _, args := range cases {
		e.mounts.queue(&e.mounts.tableErrs, errors.New("getfsstat failed"))
		if _, err := e.run(t, args...); err == nil || err.Error() != "getfsstat failed" {
			t.Errorf("%v: err = %v, want the mount table error", args, err)
		}
	}
}

// TestUnknownMount checks every command names the shares it knows.
func TestUnknownMount(t *testing.T) {
	e := newEnv(t)
	for _, cmd := range nameCommands {
		_, err := e.run(t, cmd, "x")
		if err == nil || !strings.Contains(err.Error(), `no mount "x"`) || !strings.Contains(err.Error(), "k") {
			t.Errorf("%s x: err = %v, want the unknown mount error listing k", cmd, err)
		}
	}
}

// TestConfigError checks every command reports a missing config file.
func TestConfigError(t *testing.T) {
	e := newEnv(t)
	missing := filepath.Join(t.TempDir(), "missing.toml")
	for _, cmd := range nameCommands {
		_, err := e.run(t, cmd, "--config", missing)
		checkErr(t, err, "config file "+missing+" does not exist")
	}
}
