package server

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// authSysBody encodes an AUTH_SYS credential body as a kernel client
// sends it.
func authSysBody(machine string, uid, gid uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, 12345)             // stamp
	b = binary.BigEndian.AppendUint32(b, uint32(len(machine))) //nolint:gosec // G115: test names are a few bytes
	b = append(b, machine...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	b = binary.BigEndian.AppendUint32(b, uid)
	b = binary.BigEndian.AppendUint32(b, gid)

	return binary.BigEndian.AppendUint32(b, 0) // no supplementary gids
}

func TestOwnerOnly(t *testing.T) {
	const owner = 501
	allow := ownerOnly(owner)
	for _, tc := range []struct {
		name         string
		proc, flavor uint32
		body         []byte
		want         bool
	}{
		{"owner", 1, authSys, authSysBody("mac", owner, 20), true},
		{"root", 1, authSys, authSysBody("mac", 0, 0), true},
		{"other user", 1, authSys, authSysBody("mac", 502, 20), false},
		{"name padding is skipped", 1, authSys, authSysBody("hostname", owner, 20), true},
		{"unpadded name length", 1, authSys, authSysBody("abcde", 502, 20), false},
		{"AUTH_NULL compound", 1, 0, nil, false},
		{"AUTH_NULL ping", rpcProcNull, 0, nil, true},
		{"truncated body", 1, authSys, authSysBody("mac", owner, 20)[:10], false},
		{"credential shorter than its header", 1, authSys, make([]byte, 4), false},
		{"oversized name", 1, authSys, binary.BigEndian.AppendUint32(make([]byte, 4), 4096), false},
	} {
		if got := allow(tc.proc, tc.flavor, tc.body); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestReservedPortListenerRefusesEphemeralPorts connects from an ordinary
// (ephemeral, unprivileged) port, as a non-root program would, and expects
// the connection to be closed without being handed to the server.
func TestReservedPortListenerRefusesEphemeralPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := reservedPortListener{ln}
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read from refused connection = %v, want EOF", err)
	}
	select {
	case <-accepted:
		t.Fatal("connection from an unreserved port was accepted")
	default:
	}
}

// TestReservedPortListenerAcceptsReservedPorts connects from a port below
// 1024, as a kernel NFS client does, and expects the connection handed on.
// macOS lets any user bind the wildcard address there; on Linux only root
// can, so elsewhere the test skips when no reserved port can be bound.
func TestReservedPortListenerAcceptsReservedPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := reservedPortListener{ln}
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
		}
	}()

	c := dialFromReservedPort(t, ln.Addr().String())
	defer func() { _ = c.Close() }()
	select {
	case got := <-accepted:
		defer func() { _ = got.Close() }()
		if got.RemoteAddr().String() != c.LocalAddr().String() {
			t.Fatalf("accepted %v, want the connection from %v", got.RemoteAddr(), c.LocalAddr())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("connection from a reserved port was not accepted")
	}
}

// dialFromReservedPort connects to addr from the first free port below
// 1024, skipping the test when this user may bind none.
func dialFromReservedPort(t *testing.T, addr string) net.Conn {
	t.Helper()
	for port := reservedPorts - 1; port >= 600; port-- {
		d := net.Dialer{LocalAddr: &net.TCPAddr{Port: port}}
		c, err := d.DialContext(t.Context(), "tcp", addr)
		if err == nil {
			return c
		}
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			t.Skipf("binding reserved port %d: %v", port, err)
		}
	}
	t.Skip("no free reserved port to dial from")

	return nil
}
