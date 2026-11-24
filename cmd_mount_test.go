package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
