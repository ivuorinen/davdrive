package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	rlog "github.com/rclone/rclone/fs/log"
	nfs "github.com/willscott/go-nfs"
)

// TestNFSLogger checks each go-nfs message reaches rclone's log at its
// mapped level when it is within the configured go-nfs level, and is
// dropped otherwise.
func TestNFSLogger(t *testing.T) {
	type call func(l *nfsLogger)
	methods := []struct {
		name string
		call call
		at   nfs.LogLevel
		to   fs.LogLevel
	}{
		{"Panic", func(l *nfsLogger) { l.Panic("Panic ", 1) }, nfs.PanicLevel, fs.LogLevelError},
		{"Fatal", func(l *nfsLogger) { l.Fatal("Fatal ", 1) }, nfs.FatalLevel, fs.LogLevelError},
		{"Error", func(l *nfsLogger) { l.Error("Error ", 1) }, nfs.ErrorLevel, fs.LogLevelError},
		{"Warn", func(l *nfsLogger) { l.Warn("Warn ", 1) }, nfs.WarnLevel, fs.LogLevelNotice},
		{"Info", func(l *nfsLogger) { l.Info("Info ", 1) }, nfs.InfoLevel, fs.LogLevelInfo},
		{"Debug", func(l *nfsLogger) { l.Debug("Debug ", 1) }, nfs.DebugLevel, fs.LogLevelDebug},
		{"Trace", func(l *nfsLogger) { l.Trace("Trace ", 1) }, nfs.TraceLevel, fs.LogLevelDebug},
		{"Print", func(l *nfsLogger) { l.Print("Print ", 1) }, nfs.InfoLevel, fs.LogLevelInfo},
		{"Panicf", func(l *nfsLogger) { l.Panicf("%s %d", "Panicf", 1) }, nfs.PanicLevel, fs.LogLevelError},
		{"Fatalf", func(l *nfsLogger) { l.Fatalf("%s %d", "Fatalf", 1) }, nfs.FatalLevel, fs.LogLevelError},
		{"Errorf", func(l *nfsLogger) { l.Errorf("%s %d", "Errorf", 1) }, nfs.ErrorLevel, fs.LogLevelError},
		{"Warnf", func(l *nfsLogger) { l.Warnf("%s %d", "Warnf", 1) }, nfs.WarnLevel, fs.LogLevelNotice},
		{"Infof", func(l *nfsLogger) { l.Infof("%s %d", "Infof", 1) }, nfs.InfoLevel, fs.LogLevelInfo},
		{"Debugf", func(l *nfsLogger) { l.Debugf("%s %d", "Debugf", 1) }, nfs.DebugLevel, fs.LogLevelDebug},
		{"Tracef", func(l *nfsLogger) { l.Tracef("%s %d", "Tracef", 1) }, nfs.TraceLevel, fs.LogLevelDebug},
		{"Printf", func(l *nfsLogger) { l.Printf("%s %d", "Printf", 1) }, nfs.InfoLevel, fs.LogLevelInfo},
	}
	logs := captureLogs(t)
	rlog.Handler.SetLevel(slog.LevelDebug)
	for _, level := range []nfs.LogLevel{nfs.ErrorLevel, nfs.WarnLevel, nfs.DebugLevel, nfs.TraceLevel} {
		l := &nfsLogger{}
		l.SetLevel(level)
		if l.GetLevel() != level {
			t.Fatalf("GetLevel = %v after SetLevel(%v)", l.GetLevel(), level)
		}
		for _, m := range methods {
			before := len(logs.records())
			m.call(l)
			recs := logs.records()[before:]
			want := m.name + " 1"
			switch {
			case m.at > level && len(recs) != 0:
				t.Errorf("level %v: %s was logged: %q", level, m.name, recs[0].text)
			case m.at > level:
			case len(recs) != 1 || !strings.Contains(recs[0].text, want):
				t.Errorf("level %v: %s logged %+v, want one record with %q", level, m.name, recs, want)
			case recs[0].level != fs.LogLevelToSlog(m.to):
				t.Errorf("level %v: %s logged at %v, want %v", level, m.name, recs[0].level, fs.LogLevelToSlog(m.to))
			}
		}
	}
}

func TestNFSLoggerParseLevel(t *testing.T) {
	l := &nfsLogger{}
	if got, err := l.ParseLevel("debug"); err != nil || got != nfs.DebugLevel {
		t.Errorf("ParseLevel(debug) = %v, %v; want DebugLevel", got, err)
	}
	if _, err := l.ParseLevel("loud"); err == nil {
		t.Error("ParseLevel(loud) succeeded")
	}
}
