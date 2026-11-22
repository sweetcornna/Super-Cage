//go:build linux

package vsockrelay

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ListenVsock listens on a vsock port of every address of this machine. The
// file descriptors are non-blocking, so the runtime poller serves accept,
// reads, writes and deadlines, and Close ends a waiting Accept.
func ListenVsock(port uint32) (Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "vsock-listener")
	raw, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &vsockListener{file: file, raw: raw}, nil
}

type vsockListener struct {
	file *os.File
	raw  syscall.RawConn
}

func (l *vsockListener) Close() error { return l.file.Close() }

// Accept returns the next connection and its peer's CID; a peer address that
// is not a vsock one reports a CID no host has.
func (l *vsockListener) Accept() (net.Conn, uint32, error) {
	for {
		var fd int
		var peer unix.Sockaddr
		var acceptErr error
		err := l.raw.Read(func(listener uintptr) bool {
			fd, peer, acceptErr = unix.Accept4(int(listener), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
			return !errors.Is(acceptErr, unix.EAGAIN)
		})
		if err != nil {
			return nil, 0, err
		}
		if errors.Is(acceptErr, unix.EINTR) || errors.Is(acceptErr, unix.ECONNABORTED) {
			continue
		}
		if acceptErr != nil {
			return nil, 0, acceptErr
		}
		cid := uint32(unix.VMADDR_CID_ANY)
		if address, ok := peer.(*unix.SockaddrVM); ok {
			cid = address.CID
		}
		return &vsockConn{file: os.NewFile(uintptr(fd), "vsock"), peer: vsockAddr{cid: cid}}, cid, nil
	}
}

type vsockAddr struct{ cid uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return "vsock:" + strconv.FormatUint(uint64(a.cid), 10) }

type vsockConn struct {
	file *os.File
	peer vsockAddr
}

func (c *vsockConn) Read(data []byte) (int, error)        { return c.file.Read(data) }
func (c *vsockConn) Write(data []byte) (int, error)       { return c.file.Write(data) }
func (c *vsockConn) Close() error                         { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr                  { return vsockAddr{cid: unix.VMADDR_CID_ANY} }
func (c *vsockConn) RemoteAddr() net.Addr                 { return c.peer }
func (c *vsockConn) SetDeadline(deadline time.Time) error { return c.file.SetDeadline(deadline) }
func (c *vsockConn) SetReadDeadline(deadline time.Time) error {
	return c.file.SetReadDeadline(deadline)
}
func (c *vsockConn) SetWriteDeadline(deadline time.Time) error {
	return c.file.SetWriteDeadline(deadline)
}

// CloseWrite ends the sending direction, as a half-close does on TCP.
func (c *vsockConn) CloseWrite() error {
	raw, err := c.file.SyscallConn()
	if err != nil {
		return err
	}
	var shutdown error
	if err := raw.Control(func(fd uintptr) { shutdown = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return shutdown
}
