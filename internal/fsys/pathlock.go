package fsys

import (
	"sync"

	billy "github.com/go-git/go-billy/v5"
)

// pathLocks serializes file handles per path, from open to close.
//
// go-nfs opens and closes the file for every READ and WRITE call. When
// those open-write-close cycles overlap on one file, rclone's VFS cache
// loses writes: the uploaded copy has zero-filled holes where whole WRITE
// calls were (TestConcurrentWrites). One NFS connection is served one call
// at a time, but a client may open several (Linux nconnect) or overlap a
// reconnect, so the cycles are serialized here, per file.
type pathLocks struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

type pathLock struct {
	sync.Mutex
	refs int // holders and waiters; the entry is dropped at zero
}

// lock blocks until no other handle on name is open, and returns the
// function that releases name.
func (p *pathLocks) lock(name string) (unlock func()) {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = make(map[string]*pathLock)
	}
	l := p.locks[name]
	if l == nil {
		l = &pathLock{}
		p.locks[name] = l
	}
	l.refs++
	p.mu.Unlock()

	l.Lock()

	return func() {
		l.Unlock()
		p.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(p.locks, name)
		}
		p.mu.Unlock()
	}
}

// lockedFile releases its path lock when closed.
type lockedFile struct {
	billy.File
	unlock func()
	once   sync.Once
}

func (f *lockedFile) Close() error {
	err := f.File.Close()
	f.once.Do(f.unlock)

	return err
}
