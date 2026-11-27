package workbench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	defaultOrcaPort       = 16768
	orcaTunnelPortRangeLo = 16768
	orcaTunnelPortRangeHi = 16868
)

// orcaTunnel is a dedicated, epoch-gated, persistent loopback TCP forwarder for
// native Orca Desktop connections (`ws://127.0.0.1:<hostPort>` -> guest
// `127.0.0.1:<guestPort>`).
//
// Unlike t3CodeProxy (which gates browser HTTP tabs with cookies on an ephemeral
// port), Orca Desktop's Electron WebSocket client persists the remote server
// port in runtime-environments.json and authenticates with E2EE v2 cryptographic
// handshakes without browser cookies.
//
// Security invariants:
//   - Listens strictly on 127.0.0.1:<hostPort> and refuses non-loopback peers.
//   - Bound to the active localAccount unlock epoch: locking Cage cancels the
//     epoch and immediately closes the listener, the SSH local forward, and
//     every live relayed connection.
//   - Transparently reconnects the underlying SSH local forward on drop while
//     keeping the persisted 127.0.0.1:<hostPort> endpoint stable.
type orcaTunnel struct {
	server        *Server
	hostPort      int
	guestPort     int
	targetKey     string
	listener      net.Listener
	authority     string // "127.0.0.1:<hostPort>"
	epoch         context.Context
	stopEpochHook func() bool

	mu           sync.Mutex
	activeConns  map[net.Conn]struct{}
	tunnelCancel context.CancelFunc
	tunnelDone   chan struct{}
	tunnelAddr   string
	tunnelMu     sync.Mutex
	closed       bool
}

func (t *orcaTunnel) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *orcaTunnel) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	conns := make([]net.Conn, 0, len(t.activeConns))
	for c := range t.activeConns {
		conns = append(conns, c)
	}
	clear(t.activeConns)
	stopHook := t.stopEpochHook
	t.stopEpochHook = nil
	t.mu.Unlock()

	if stopHook != nil {
		stopHook()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	t.stopSSHForward()
	if t.listener != nil {
		_ = t.listener.Close()
	}
}

func (t *orcaTunnel) stopSSHForward() {
	t.tunnelMu.Lock()
	cancel := t.tunnelCancel
	done := t.tunnelDone
	t.tunnelCancel = nil
	t.tunnelDone = nil
	t.tunnelAddr = ""
	t.tunnelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (t *orcaTunnel) ensureSSHForward(ctx context.Context) (string, error) {
	if hook := t.server.orca.tunnelDialHook; hook != nil {
		return hook(ctx, t.guestPort)
	}
	t.tunnelMu.Lock()
	defer t.tunnelMu.Unlock()
	if t.isClosed() {
		return "", errors.New("Orca 本地隧道已关闭")
	}
	if t.tunnelAddr != "" && t.tunnelDone != nil {
		select {
		case <-t.tunnelDone:
			if t.tunnelCancel != nil {
				t.tunnelCancel()
			}
			t.tunnelCancel = nil
			t.tunnelDone = nil
			t.tunnelAddr = ""
		default:
			return t.tunnelAddr, nil
		}
	}
	addr, cancel, done, err := t.server.startGuestLocalForward(ctx, t.guestPort)
	if err != nil {
		return "", err
	}
	t.tunnelAddr, t.tunnelCancel, t.tunnelDone = addr, cancel, done
	return addr, nil
}

func (t *orcaTunnel) dialGuest(ctx context.Context) (net.Conn, error) {
	addr, err := t.ensureSSHForward(ctx)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if t.server.orca.tunnelDialHook == nil {
			t.stopSSHForward()
			if retryAddr, rErr := t.ensureSSHForward(ctx); rErr == nil {
				conn, err = dialer.DialContext(ctx, "tcp", retryAddr)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return conn, nil
}

func (t *orcaTunnel) trackConn(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (t.epoch != nil && t.epoch.Err() != nil) {
		return false
	}
	if t.activeConns == nil {
		t.activeConns = map[net.Conn]struct{}{}
	}
	t.activeConns[c] = struct{}{}
	return true
}

func (t *orcaTunnel) untrackConn(c net.Conn) {
	t.mu.Lock()
	delete(t.activeConns, c)
	t.mu.Unlock()
}

func (t *orcaTunnel) serve() {
	for {
		clientConn, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.handleConn(clientConn)
	}
}

func (t *orcaTunnel) handleConn(clientConn net.Conn) {
	defer func() { _ = clientConn.Close() }()
	if t.isClosed() || (t.epoch != nil && t.epoch.Err() != nil) {
		return
	}
	if t.server.account != nil {
		if epoch, unlocked := t.server.account.access(); !unlocked || (t.epoch != nil && epoch != t.epoch) {
			t.close()
			return
		}
	}
	if remoteAddr, ok := clientConn.RemoteAddr().(*net.TCPAddr); ok && !remoteAddr.IP.IsLoopback() {
		return
	}
	if tc, ok := clientConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	dialCtx, dialCancel := context.WithTimeout(t.server.lifetimeContext(), 8*time.Second)
	if t.epoch != nil {
		stop := context.AfterFunc(t.epoch, dialCancel)
		defer stop()
	}
	upstream, err := t.dialGuest(dialCtx)
	dialCancel()
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	if !t.trackConn(clientConn) || !t.trackConn(upstream) {
		t.untrackConn(clientConn)
		t.untrackConn(upstream)
		return
	}
	defer func() {
		t.untrackConn(clientConn)
		t.untrackConn(upstream)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, clientConn)
		closeWriteConn(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, upstream)
		closeWriteConn(clientConn)
	}()
	wg.Wait()
}

func (s *Server) closeOrcaTunnel() {
	s.orca.mu.Lock()
	tun := s.orca.tunnel
	s.orca.tunnel = nil
	s.orca.mu.Unlock()
	if tun != nil {
		tun.close()
	}
}

func (s *Server) orcaTunnelSnapshot() (active bool, port int) {
	s.orca.mu.Lock()
	tun := s.orca.tunnel
	s.orca.mu.Unlock()
	if tun == nil || tun.isClosed() {
		return false, 0
	}
	if tun.epoch != nil && tun.epoch.Err() != nil {
		return false, 0
	}
	return true, tun.hostPort
}

func isLocalLoopbackPortFree(port int) bool {
	if port < 1024 || port > 65535 {
		return false
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// pickFreeOrcaTunnelPort chooses a deterministic, collision-free loopback port
// in [16768..16868], skipping any port currently bound on 127.0.0.1 OR already
// registered by another remote server in host Orca Desktop.
func pickFreeOrcaTunnelPort(preferred int, reserved map[int]bool) int {
	if preferred >= 1024 && preferred <= 65535 && !reserved[preferred] && isLocalLoopbackPortFree(preferred) {
		return preferred
	}
	for p := orcaTunnelPortRangeLo; p <= orcaTunnelPortRangeHi; p++ {
		if reserved[p] {
			continue
		}
		if isLocalLoopbackPortFree(p) {
			return p
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return defaultOrcaPort
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func (s *Server) ensureOrcaTunnel(ctx context.Context, hostPort, guestPort int) (*orcaTunnel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.windowsGuest != nil {
		return nil, errors.New("Windows 原生客机暂不支持托管 Orca Server 本地隧道")
	}
	if hostPort < 1024 || hostPort > 65535 || guestPort < 1024 || guestPort > 65535 {
		return nil, errors.New("Orca 隧道端口须在 1024 到 65535 之间")
	}
	var epoch context.Context
	if s.account != nil {
		var unlocked bool
		epoch, unlocked = s.account.access()
		if !unlocked {
			return nil, errors.New("Cage 已锁定，请先解锁工作台")
		}
		if requested, _ := ctx.Value(accountEpochKey{}).(context.Context); requested != nil && requested != epoch {
			return nil, errClaudeAccessLost
		}
	}
	targetKey := fmt.Sprintf("%s:%d->%d", s.sshTarget(), hostPort, guestPort)

	s.orca.mu.Lock()
	if cur := s.orca.tunnel; cur != nil && cur.targetKey == targetKey && !cur.isClosed() && (cur.epoch == nil || cur.epoch == epoch) {
		s.orca.mu.Unlock()
		if _, err := cur.ensureSSHForward(ctx); err != nil {
			return nil, err
		}
		return cur, nil
	}
	old := s.orca.tunnel
	s.orca.tunnel = nil
	s.orca.mu.Unlock()
	if old != nil {
		old.close()
	}

	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))
	ln, err := net.Listen("tcp", authority)
	if err != nil {
		return nil, fmt.Errorf("本机回环端口 %d 已被占用，请在 Orca 设置中更换本机隧道端口: %w", hostPort, err)
	}

	tun := &orcaTunnel{
		server:      s,
		hostPort:    hostPort,
		guestPort:   guestPort,
		targetKey:   targetKey,
		listener:    ln,
		authority:   authority,
		epoch:       epoch,
		activeConns: map[net.Conn]struct{}{},
	}
	if _, err := tun.ensureSSHForward(ctx); err != nil {
		tun.close()
		return nil, err
	}

	if epoch != nil {
		stop := context.AfterFunc(epoch, func() {
			tun.close()
			s.orca.mu.Lock()
			if s.orca.tunnel == tun {
				s.orca.tunnel = nil
			}
			s.orca.mu.Unlock()
		})
		tun.mu.Lock()
		if tun.closed {
			tun.mu.Unlock()
			stop()
		} else {
			tun.stopEpochHook = stop
			tun.mu.Unlock()
		}
	}

	go tun.serve()

	var displaced *orcaTunnel
	s.orca.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.orca.mu.Unlock()
		tun.close()
		return nil, err
	}
	if tun.isClosed() || (epoch != nil && epoch.Err() != nil) {
		s.orca.mu.Unlock()
		tun.close()
		return nil, errors.New("Cage 已锁定，请先解锁工作台")
	}
	if existing := s.orca.tunnel; existing != nil && !existing.isClosed() {
		if existing.targetKey == targetKey && (existing.epoch == nil || existing.epoch == epoch) {
			s.orca.mu.Unlock()
			tun.close()
			return existing, nil
		}
		displaced = existing
	}
	s.orca.tunnel = tun
	s.orca.mu.Unlock()
	if displaced != nil {
		displaced.close()
	}
	return tun, nil
}
