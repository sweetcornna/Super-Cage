//go:build unix

package tenant

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type vsockAddr struct{ cid, port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("%d:%d", a.cid, a.port) }

type vsockConn struct {
	file   *os.File
	remote vsockAddr
}

func (c *vsockConn) Read(data []byte) (int, error)  { return c.file.Read(data) }
func (c *vsockConn) Write(data []byte) (int, error) { return c.file.Write(data) }
func (c *vsockConn) Close() error                   { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr            { return vsockAddr{cid: 2} } // VMADDR_CID_HOST
func (c *vsockConn) RemoteAddr() net.Addr           { return c.remote }
func (c *vsockConn) SetDeadline(deadline time.Time) error {
	return c.file.SetDeadline(deadline)
}
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
