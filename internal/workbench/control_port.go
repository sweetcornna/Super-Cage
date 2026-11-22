package workbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

const (
	controllerFirstPort = 17777
	controllerPortCount = 31
)

type controllerPortRecord struct {
	Port int `json:"port"`
}

// listenController keeps the workbench origin stable, because the origin
// scopes its cookies and browser storage. Windows can reserve whole port
// blocks for Hyper-V, WSL or container networking, which may cover every
// preferred port. The system then assigns a free port. It is remembered, so
// later starts keep that origin while it stays available.
//
// Residual (A3 of the review of controller-dial-hardening, documented, not
// fixed): another local user who holds a preferred port, or its [::1]
// twin, still moves the controller to another port (a new origin, so the
// old one's browser storage is not seen), and one who holds every port a
// start tries keeps it from starting.
func listenController(profile string) (net.Listener, int, error) {
	for port := controllerFirstPort; port < controllerFirstPort+controllerPortCount; port++ {
		listener, err := listenLoopback(port)
		if err == nil {
			return listener, port, nil
		}
		if errors.Is(err, errLoopbackTwinHeld) {
			diagnostics.Log("warn", "controller", fmt.Sprintf("port %d skipped: another program listens on [::1]:%d", port, port))
		}
	}
	path := filepath.Join(profile, "control-port.json")
	var record controllerPortRecord
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &record) == nil && record.Port >= 1024 && record.Port <= 65535 {
		if listener, err := listenLoopback(record.Port); err == nil {
			return listener, record.Port, nil
		}
	}
	listener, err := listenLoopback(0)
	for attempt := 0; errors.Is(err, errLoopbackTwinHeld) && attempt < 8; attempt++ {
		listener, err = listenLoopback(0)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("无法在本机回环地址上监听工作台端口: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if port < 1024 {
		listener.Close()
		return nil, 0, fmt.Errorf("系统分配的工作台端口 %d 无效", port)
	}
	// Losing the record only costs the stable origin, not the start.
	_ = writePrivate(path, controllerPortRecord{Port: port})
	return listener, port, nil
}

// listenLoopbackV6 is replaced in tests to stand for a system without IPv6 loopback.
var listenLoopbackV6 = func(port int) (net.Listener, error) {
	return net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
}

var errLoopbackTwinHeld = errors.New("another program listens on the [::1] twin of this port")

// listenLoopback holds port on 127.0.0.1 and on [::1]. Clients in this
// package dial 127.0.0.1 themselves, but the desktop windows load
// http://localhost:<port>, which their resolvers may send to ::1 first: a
// program of another local user listening there would get the launch page
// and its ticket. Without IPv6 loopback nobody can listen on ::1, so IPv4
// alone is enough. A twin that someone else holds makes the port count as
// taken, like a taken IPv4 port.
func listenLoopback(port int) (net.Listener, error) {
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	v6, err := listenLoopbackV6(v4.Addr().(*net.TCPAddr).Port)
	if err == nil {
		return newLoopbackPair(v4, v6), nil
	}
	probe, probeErr := listenLoopbackV6(0)
	if probeErr != nil {
		return v4, nil
	}
	probe.Close()
	v4.Close()
	return nil, fmt.Errorf("%w: %v", errLoopbackTwinHeld, err)
}

// loopbackPair accepts the connections of both loopback listeners of one
// port. Its address is the IPv4 one.
type loopbackPair struct {
	v4, v6   net.Listener
	accepted chan acceptedConn
	closed   chan struct{}
	once     sync.Once
}

type acceptedConn struct {
	conn net.Conn
	err  error
}

func newLoopbackPair(v4, v6 net.Listener) *loopbackPair {
	p := &loopbackPair{v4: v4, v6: v6, accepted: make(chan acceptedConn), closed: make(chan struct{})}
	go p.forward(v4)
	go p.forward(v6)
	return p
}

// forward passes l's connections to Accept. An error of the IPv4 listener
// is passed on, as net/http expects of its listener. One of the IPv6 twin
// is not: net/http would stop serving IPv4 too, or retry at once. The twin
// backs off instead, as net/http does after a temporary error, up to a
// second, until it accepts again or the pair closes (A4 of the review of
// controller-dial-hardening).
func (p *loopbackPair) forward(l net.Listener) {
	var delay time.Duration
	for {
		conn, err := l.Accept()
		if err != nil && l == p.v6 && !errors.Is(err, net.ErrClosed) {
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(delay):
				continue
			case <-p.closed:
				return
			}
		}
		delay = 0
		select {
		case p.accepted <- acceptedConn{conn, err}:
		case <-p.closed:
			if conn != nil {
				conn.Close()
			}
			return
		}
		if errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

func (p *loopbackPair) Accept() (net.Conn, error) {
	select {
	case a := <-p.accepted:
		return a.conn, a.err
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

func (p *loopbackPair) Close() error {
	err := net.ErrClosed
	p.once.Do(func() {
		close(p.closed)
		err = errors.Join(p.v4.Close(), p.v6.Close())
	})
	return err
}

func (p *loopbackPair) Addr() net.Addr { return p.v4.Addr() }
