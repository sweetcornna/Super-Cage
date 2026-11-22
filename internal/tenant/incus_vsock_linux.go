//go:build linux

package tenant

import (
	"context"
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// dialVsockHost connects to a port of a VM's vsock address from the host.
// net.FileConn does not know AF_VSOCK, so the connection is an os.File on a
// non-blocking socket, which the runtime poller serves with deadlines like
// the exec pipes of agentConn.
func dialVsockHost(ctx context.Context, cid, port uint32) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	// The kernel bounds a connect to a VM that does not answer, so a blocking
	// connect in its own goroutine cannot hang for long even when ctx ends.
	result := make(chan error, 1)
	go func() {
		for {
			err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
			if !errors.Is(err, unix.EINTR) {
				result <- err
				return
			}
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			unix.Close(fd)
			return nil, err
		}
	case <-ctx.Done():
		go func() {
			<-result
			unix.Close(fd)
		}()
		return nil, ctx.Err()
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &vsockConn{file: os.NewFile(uintptr(fd), "vsock"), remote: vsockAddr{cid: cid, port: port}}, nil
}
