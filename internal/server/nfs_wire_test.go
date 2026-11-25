package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"
)

const (
	rpcCall       = 0
	rpcReply      = 1
	msgAccepted   = 0
	msgDenied     = 1
	authError     = 1
	authTooWeak   = 5
	nfsProgram    = 100003
	procCompound  = 1
	opPutRootFH   = 24
	opCreate      = 6
	nf4Dir        = 2
	nfs4OK        = 0
	nfs4ErrExist  = 17
	rpcReadWindow = 10 * time.Second
)

// compoundPutRootFH is a v4.0 COMPOUND with no tag holding one PUTROOTFH.
var compoundPutRootFH = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, opPutRootFH}

// rpcFrame encodes a record-marked ONC RPC call to NFSv4 (RFC 5531)
// carrying an AUTH_SYS credential for uid.
func rpcFrame(xid, proc, uid uint32, args []byte) []byte {
	cred := authSysBody("test", uid, uid)
	credLen := uint32(len(cred)) //nolint:gosec // G115: a few bytes
	b := make([]byte, 4, 64)     // record mark, filled in last
	for _, v := range []uint32{xid, rpcCall, 2, nfsProgram, 4, proc, authSys, credLen} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	b = append(b, cred...)
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0) // verifier: AUTH_NONE, empty
	b = append(b, args...)
	binary.BigEndian.PutUint32(b, 1<<31|uint32(len(b)-4)) //nolint:gosec // G115: a few bytes

	return b
}

// rpcExchange sends one call and returns the reply as XDR words.
func rpcExchange(t *testing.T, c net.Conn, frame []byte) []uint32 {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(rpcReadWindow))
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("sending RPC: %v", err)
	}
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		t.Fatalf("reading RPC reply: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(mark[:])&^(1<<31))
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("reading RPC reply: %v", err)
	}
	words := make([]uint32, 0, len(body)/4)
	for i := 0; i+4 <= len(body); i += 4 {
		words = append(words, binary.BigEndian.Uint32(body[i:]))
	}

	return words
}

func dialNFS(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// TestNFSServesOwnerOnly speaks RPC to a running server: other users'
// calls are refused before they reach the share, the owner's are served.
// On Linux the reserved-port rule refuses this unprivileged test program
// outright.
func TestNFSServesOwnerOnly(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	s, _ := startServer(t, newMount("wire", dav.URL), "")
	c := dialNFS(t, s.Port())

	if runtime.GOOS == "linux" {
		_ = c.SetReadDeadline(time.Now().Add(rpcReadWindow))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("read from an unreserved port = %v, want EOF", err)
		}

		return
	}

	owner := uint32(os.Getuid()) //nolint:gosec // G115: uids are 32-bit on darwin and linux
	accepted := []uint32{rpcReply, msgAccepted, 0, 0, 0}
	for i, tc := range []struct {
		name       string
		proc, uid  uint32
		args       []byte
		wantAfterX []uint32
	}{
		{"ping from another user", rpcProcNull, owner + 1, nil, accepted},
		{"compound from another user", procCompound, owner + 1, compoundPutRootFH, []uint32{
			rpcReply, msgDenied, authError, authTooWeak,
		}},
		{"compound from the owner", procCompound, owner, compoundPutRootFH, append(slices.Clone(accepted),
			0, 0, 1, opPutRootFH, 0, // NFS4_OK, empty tag, one result: PUTROOTFH OK
		)},
	} {
		xid := uint32(i + 1)
		got := rpcExchange(t, c, rpcFrame(xid, tc.proc, tc.uid, tc.args))
		if want := append([]uint32{xid}, tc.wantAfterX...); !slices.Equal(got, want) {
			t.Errorf("%s: reply = %v, want %v", tc.name, got, want)
		}
	}
}

// plainNFSPort serves s's NFS handler and access check on a second loopback
// listener without the reserved-port rule, so a test can speak NFS to the
// share on Linux too, and returns its port.
func plainNFSPort(t *testing.T, s *Server) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain := &nfs.Server{Handler: s.nfs.Handler, EnabledNFSVersions: []uint32{4}, Authorize: s.nfs.Authorize}
	go func() { _ = plain.Serve(ln) }()
	// Close only closes listeners Serve has taken up; closing ln covers
	// a Serve that has not started yet.
	t.Cleanup(func() {
		_ = plain.Close()
		_ = ln.Close()
	})
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", ln.Addr())
	}

	return addr.Port
}

// TestStopWithExpiredContext stops a server holding an open NFS
// connection with no time left: Stop must still return and drop it.
func TestStopWithExpiredContext(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("the reserved-port rule refuses this test's NFS connection on Linux")
	}
	isolate(t)
	dav := newDAV(t, davOptions{})
	m := newMount("expired", dav.URL)
	s, stop := startServer(t, m, "")
	c := dialNFS(t, s.Port())
	rpcExchange(t, c, rpcFrame(1, rpcProcNull, 0, nil)) // the server now tracks c

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stop(ctx)

	_ = c.SetReadDeadline(time.Now().Add(rpcReadWindow))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("NFS connection after Stop: read err = %v, want it closed", err)
	}
	assertReleased(t, m)
}

// compoundCreateDir is a v4.0 COMPOUND of PUTROOTFH and CREATE of the
// directory name, with no attributes.
func compoundCreateDir(name string) []byte {
	b := binary.BigEndian.AppendUint32(nil, 0) // tag: empty
	b = binary.BigEndian.AppendUint32(b, 0)    // minor version
	b = binary.BigEndian.AppendUint32(b, 2)    // operations
	b = binary.BigEndian.AppendUint32(b, opPutRootFH)
	b = binary.BigEndian.AppendUint32(b, opCreate)
	b = binary.BigEndian.AppendUint32(b, nf4Dir)            // object type
	b = binary.BigEndian.AppendUint32(b, uint32(len(name))) //nolint:gosec // G115: a short test name
	b = append(b, name...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	b = binary.BigEndian.AppendUint32(b, 0) // attribute mask: no words

	return binary.BigEndian.AppendUint32(b, 0) // attribute values: empty
}

// TestNFSCreateRefusesTakenName creates a directory over NFS twice: the
// second CREATE must answer NFS4ERR_EXIST. On a Linux mount, mkdir of a
// directory the client has not seen since mounting reaches this CREATE,
// and an NFS4_OK makes it report success, breaking every program that uses
// mkdir as a lock.
func TestNFSCreateRefusesTakenName(t *testing.T) {
	isolate(t)
	dav := newDAV(t, davOptions{})
	s, _ := startServer(t, newMount("create", dav.URL), "")
	c := dialNFS(t, plainNFSPort(t, s))
	owner := uint32(os.Getuid()) //nolint:gosec // G115: uids are 32-bit on darwin and linux

	// After the RPC header: COMPOUND status, empty tag, two results,
	// PUTROOTFH OK, then CREATE and its status.
	for i, want := range []uint32{nfs4OK, nfs4ErrExist} {
		xid := uint32(i + 1)
		got := rpcExchange(t, c, rpcFrame(xid, procCompound, owner, compoundCreateDir("taken")))
		wantPrefix := []uint32{xid, rpcReply, msgAccepted, 0, 0, 0, want, 0, 2, opPutRootFH, nfs4OK, opCreate, want}
		if len(got) < len(wantPrefix) || !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
			t.Errorf("CREATE %d: reply = %v, want it to start %v", i+1, got, wantPrefix)
		}
	}
}
