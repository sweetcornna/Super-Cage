//go:build unix

package tenant

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// vsockPair returns both ends of a stream socket pair wrapped like a vsock
// connection. The wrapper only needs a pollable stream socket, so the pair
// stands in for a connection to a guest.
func vsockPair(t *testing.T) (*vsockConn, *vsockConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]*vsockConn, 2)
	for index, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
		ends[index] = &vsockConn{file: os.NewFile(uintptr(fd), "pair"), remote: vsockAddr{cid: 42, port: 45001}}
		t.Cleanup(func() { ends[index].Close() })
	}
	return ends[0], ends[1]
}

func TestVsockConnCarriesDataDeadlinesAndHalfClose(t *testing.T) {
	var _ net.Conn = (*vsockConn)(nil)
	host, guest := vsockPair(t)
	if host.RemoteAddr().String() != "42:45001" || host.RemoteAddr().Network() != "vsock" {
		t.Fatalf("remote address %v", host.RemoteAddr())
	}
	if _, err := host.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	count, err := guest.Read(buffer)
	if err != nil || string(buffer[:count]) != "ping" {
		t.Fatalf("read %q, %v", buffer[:count], err)
	}
	// A read deadline ends a blocked read with a timeout, which the gateway
	// treats as inconclusive rather than as a broken connection.
	_ = host.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := host.Read(buffer); !isTimeout(err) {
		t.Fatalf("read past the deadline returned %v, want a timeout", err)
	}
	_ = host.SetReadDeadline(time.Time{})
	// CloseWrite ends only the sending direction: the peer reads EOF and can
	// still answer.
	if err := host.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("after CloseWrite the peer read %v, want EOF", err)
	}
	if _, err := guest.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	count, err = host.Read(buffer)
	if err != nil || string(buffer[:count]) != "pong" {
		t.Fatalf("read after CloseWrite: %q, %v", buffer[:count], err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Write([]byte("x")); err == nil {
		t.Fatal("write on a closed connection succeeded")
	}
}
