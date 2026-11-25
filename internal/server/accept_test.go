package server

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	nfs "github.com/willscott/go-nfs"
)

// failingListener fails Accept with errs in turn, then with net.ErrClosed.
type failingListener struct {
	net.Listener
	errs  []error
	calls int
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.calls++
	if len(l.errs) == 0 {
		return nil, net.ErrClosed
	}
	err := l.errs[0]
	l.errs = l.errs[1:]

	return nil, err
}

func (l *failingListener) Close() error { return nil }

func acceptError(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

// TestNFSAcceptRetries guards accept-retry.patch: running out of file
// descriptors is waited out, as net/http does, instead of ending the NFS
// server for good. Any other Accept error still ends it.
func TestNFSAcceptRetries(t *testing.T) {
	exhausted := &failingListener{errs: []error{
		acceptError(syscall.EMFILE), acceptError(syscall.ENFILE), acceptError(syscall.EMFILE),
	}}
	if err := (&nfs.Server{}).Serve(exhausted); !errors.Is(err, net.ErrClosed) || exhausted.calls != 4 {
		t.Errorf("Serve = %v after %d accepts, want net.ErrClosed after 4", err, exhausted.calls)
	}

	broken := errors.New("listener broken")
	other := &failingListener{errs: []error{broken}}
	if err := (&nfs.Server{}).Serve(other); !errors.Is(err, broken) || other.calls != 1 {
		t.Errorf("Serve = %v after %d accepts, want %v after 1", err, other.calls, broken)
	}
}
