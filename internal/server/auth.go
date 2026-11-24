package server

import (
	"encoding/binary"
	"net"

	"github.com/rclone/rclone/fs"
)

const (
	// rpcProcNull is the NULL procedure: a ping that reads and changes
	// nothing, sent by clients with any credential before mounting.
	rpcProcNull = 0
	// authSys is the AUTH_SYS (AUTH_UNIX) credential flavor, RFC 5531
	// appendix A.
	authSys = 1
	// maxMachineName is the bound RFC 5531 puts on AUTH_SYS machinename.
	maxMachineName = 255
	// reservedPorts is the end of the port range only root may bind on
	// Linux (net.ipv4.ip_unprivileged_port_start defaults to 1024).
	reservedPorts = 1024
)

// ownerOnly returns the go-nfs Authorize hook that admits calls from the
// mounting user only.
//
// The NFS server listens on a loopback TCP port that every local account
// can reach, and go-nfs authenticates nothing, so without this another
// user could mount the port and read and write this user's share. The
// kernel NFS client sends AUTH_SYS with the uid of the process making each
// access, and root for some housekeeping calls, so uid and 0 are admitted
// and every other uid and flavor is refused.
//
// AUTH_SYS is a claim, not a proof: a local program speaking RPC itself
// can put any uid in it. On Linux the reserved-port rule
// (reservedPortListener) stops unprivileged programs from connecting at
// all; macOS lets any user bind 0.0.0.0 below 1024, so there this check is
// the only barrier and a forged credential passes it.
func ownerOnly(uid uint32) func(proc, flavor uint32, body []byte) bool {
	return func(proc, flavor uint32, body []byte) bool {
		if proc == rpcProcNull {
			return true
		}
		if flavor != authSys {
			return false
		}
		caller, ok := authSysUID(body)

		return ok && (caller == uid || caller == 0)
	}
}

// authSysUID reads the uid from an AUTH_SYS credential body: stamp,
// machinename<255> (length-prefixed, padded to 4 bytes), uid, gid,
// gids<16>.
func authSysUID(body []byte) (uint32, bool) {
	if len(body) < 8 {
		return 0, false
	}
	nameLen := binary.BigEndian.Uint32(body[4:8])
	if nameLen > maxMachineName {
		return 0, false
	}
	off := 8 + int((nameLen+3)&^3)
	if len(body) < off+4 {
		return 0, false
	}

	return binary.BigEndian.Uint32(body[off : off+4]), true
}

// reservedPortListener closes every connection whose source port is not
// reserved, the classic NFS "secure" export rule. Kernel NFS clients bind
// a reserved port, and on Linux only root can, so unprivileged local
// programs cannot reach the server at all. Not used on macOS, where any
// user may bind 0.0.0.0 below 1024.
type reservedPortListener struct {
	net.Listener
}

// Accept returns the next connection from a reserved port, closing the
// others.
func (l reservedPortListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.Port < reservedPorts {
			return c, nil
		}
		fs.Errorf(
			nil,
			"Refused NFS connection from %v: source port is not reserved, so it is not a kernel NFS client",
			c.RemoteAddr(),
		)
		_ = c.Close()
	}
}
