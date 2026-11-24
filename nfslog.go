package main

import (
	"fmt"

	"github.com/rclone/rclone/fs"
	nfs "github.com/willscott/go-nfs"
)

// nfsLogger sends go-nfs's messages through rclone's logger at their own
// level. go-nfs's default logger writes through the standard log package,
// which rclone's logging setup files at NOTICE, so go-nfs errors would never
// show up as ERROR and its debug traces would flood NOTICE. Panic and Fatal
// only log, as go-nfs's default logger does; the mapping follows rclone's
// cmd/serve/nfs.
type nfsLogger struct {
	level nfs.LogLevel
}

var _ nfs.Logger = (*nfsLogger)(nil)

// SetLevel sets the most verbose go-nfs level that is passed on.
func (l *nfsLogger) SetLevel(level nfs.LogLevel) { l.level = level }

// GetLevel returns the most verbose go-nfs level that is passed on.
func (l *nfsLogger) GetLevel() nfs.LogLevel { return l.level }

// ParseLevel parses a go-nfs level name.
func (l *nfsLogger) ParseLevel(level string) (nfs.LogLevel, error) {
	return (&nfs.DefaultLogger{}).ParseLevel(level)
}

// Panic logs at ERROR.
func (l *nfsLogger) Panic(args ...any) {
	l.log(nfs.PanicLevel, fs.LogLevelError, "%s", fmt.Sprint(args...))
}

// Fatal logs at ERROR.
func (l *nfsLogger) Fatal(args ...any) {
	l.log(nfs.FatalLevel, fs.LogLevelError, "%s", fmt.Sprint(args...))
}

// Error logs at ERROR.
func (l *nfsLogger) Error(args ...any) {
	l.log(nfs.ErrorLevel, fs.LogLevelError, "%s", fmt.Sprint(args...))
}

// Warn logs at NOTICE, rclone's warning level.
func (l *nfsLogger) Warn(args ...any) {
	l.log(nfs.WarnLevel, fs.LogLevelNotice, "%s", fmt.Sprint(args...))
}

// Info logs at INFO.
func (l *nfsLogger) Info(args ...any) {
	l.log(nfs.InfoLevel, fs.LogLevelInfo, "%s", fmt.Sprint(args...))
}

// Debug logs at DEBUG.
func (l *nfsLogger) Debug(args ...any) {
	l.log(nfs.DebugLevel, fs.LogLevelDebug, "%s", fmt.Sprint(args...))
}

// Trace logs at DEBUG.
func (l *nfsLogger) Trace(args ...any) {
	l.log(nfs.TraceLevel, fs.LogLevelDebug, "%s", fmt.Sprint(args...))
}

// Print logs at INFO.
func (l *nfsLogger) Print(args ...any) {
	l.log(nfs.InfoLevel, fs.LogLevelInfo, "%s", fmt.Sprint(args...))
}

// Panicf logs at ERROR.
func (l *nfsLogger) Panicf(format string, args ...any) {
	l.log(nfs.PanicLevel, fs.LogLevelError, format, args...)
}

// Fatalf logs at ERROR.
func (l *nfsLogger) Fatalf(format string, args ...any) {
	l.log(nfs.FatalLevel, fs.LogLevelError, format, args...)
}

// Errorf logs at ERROR.
func (l *nfsLogger) Errorf(format string, args ...any) {
	l.log(nfs.ErrorLevel, fs.LogLevelError, format, args...)
}

// Warnf logs at NOTICE, rclone's warning level.
func (l *nfsLogger) Warnf(format string, args ...any) {
	l.log(nfs.WarnLevel, fs.LogLevelNotice, format, args...)
}

// Infof logs at INFO.
func (l *nfsLogger) Infof(format string, args ...any) {
	l.log(nfs.InfoLevel, fs.LogLevelInfo, format, args...)
}

// Debugf logs at DEBUG.
func (l *nfsLogger) Debugf(format string, args ...any) {
	l.log(nfs.DebugLevel, fs.LogLevelDebug, format, args...)
}

// Tracef logs at DEBUG.
func (l *nfsLogger) Tracef(format string, args ...any) {
	l.log(nfs.TraceLevel, fs.LogLevelDebug, format, args...)
}

// Printf logs at INFO.
func (l *nfsLogger) Printf(format string, args ...any) {
	l.log(nfs.InfoLevel, fs.LogLevelInfo, format, args...)
}

func (l *nfsLogger) log(at nfs.LogLevel, to fs.LogLevel, format string, args ...any) {
	if l.level >= at {
		fs.LogPrintf(to, "nfs", format, args...)
	}
}
