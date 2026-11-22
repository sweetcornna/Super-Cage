package tenant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sylvinus/agent-vm/internal/vsockrelay"
)

// The direct path.
//
// By default every connection to a workbench starts "incus exec ... socat" and
// relays its bytes through incusd and the guest agent. With IncusConfig.Vsock
// the gateway starts one long-lived relay per workspace through that same
// exec ("cage vsock-relay", internal/vsockrelay). It listens on a random vsock
// port of the VM and forwards each connection to the workbench's loopback
// port, and the gateway dials the VM's vsock address directly: a new
// connection costs a socket connect and a handshake instead of a process
// start, and the bytes bypass incusd.
//
// Trust: the gateway hands each relay a secret of its own on the exec's stdin
// (never argv or the environment), and every connection, pooled ones too,
// proves that secret both ways before any HTTP byte. So the gateway sends a
// request or the launcher token only to a relay it started in this process,
// the relay serves only the gateway (and, on Linux, only the host's vsock
// address), and nothing in the VM, its sandbox or a nested guest can use the
// relay or stand in for it. A listener that answers without the secret puts
// the instance on exec. The relay ends with its stdin, so with the gateway.
//
// The path is an optimisation with three safeguards:
//
//   - the workspace's handshake runs over it, so it only stays in use after it
//     carried a real workbench exchange; a handshake that fails over vsock is
//     repeated over exec, and only if that works is the path blamed;
//   - a failed direct dial falls back to exec inside the same dial, so no
//     request or stream sees the failure;
//   - a path that cannot work (the guest's cage has no relay, nothing ever
//     listens, several dials in a row fail) is dropped, and the instance is
//     not tried again for directRetry, so a guest that cannot do vsock costs
//     one failed attempt rather than one per connection. A workbench that is
//     merely not answering yet is not a reason to drop it: the guest side
//     comes up before the workbench does.
//
// Containers share the host's kernel and its vsock namespace, so a vsock
// listener in a container would be reachable from every other container:
// they always use exec.

// directRetry is how long an instance whose direct path failed stays on exec.
const directRetry = 5 * time.Minute

// directWait bounds how long a fresh bridge may take to start listening.
var directWait = 5 * time.Second

// directDialFailures is how many dials in a row may fail before a workspace
// drops the vsock path. Each failed dial still falls back to exec.
const directDialFailures = 3

// relayCommand is the guest's relay; workspace templates carry cage there.
const relayCommand = "/usr/local/bin/cage"

var (
	// errDirectUnsupported marks a failure that will repeat: the instance stays
	// on exec for directRetry.
	errDirectUnsupported = errors.New("vsock cannot be used with this instance")
	// errDirectNotReady means the listener answered but the workbench behind it
	// did not, as happens while the guest boots. Nothing is held against the path.
	errDirectNotReady = errors.New("the workbench does not answer on the vsock bridge yet")
)

// vsockStream opens a connection to a port of a VM's vsock address.
type vsockStream func(ctx context.Context, cid, port uint32) (net.Conn, error)

func (r *IncusRuntime) dialVsock(ctx context.Context, cid, port uint32) (net.Conn, error) {
	if r.vsockDial != nil {
		return r.vsockDial(ctx, cid, port)
	}
	return dialVsockHost(ctx, cid, port)
}

func (r *IncusRuntime) dialExec(ctx context.Context, instance string, port int) (net.Conn, error) {
	if r.execDial != nil {
		return r.execDial(ctx, instance, port)
	}
	return r.dial(ctx, instance, port)
}

// directAllowed reports whether new workspaces of this instance should try
// the direct path.
func (r *IncusRuntime) directAllowed(instance string) bool {
	if !r.cfg.Vsock || r.instanceType() != InstanceVM {
		return false
	}
	until, skipped := r.directSkip.Load(instance)
	return !skipped || !time.Now().Before(until.(time.Time))
}

func (r *IncusRuntime) skipDirect(instance string) {
	r.directSkip.Store(instance, time.Now().Add(directRetry))
}

// guestDialer opens the connections of one workspace: over the direct path
// while it works, with exec as the fallback.
type guestDialer struct {
	runtime  *IncusRuntime
	instance string
	port     int
	direct   atomic.Pointer[directPath]
}

func (g *guestDialer) dial(ctx context.Context) (net.Conn, error) {
	if path := g.direct.Load(); path != nil && !path.dead.Load() {
		conn, err := path.dial(ctx)
		if err == nil {
			path.failures.Store(0)
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A listener that does not prove the secret is not the gateway's relay.
		if errors.Is(err, vsockrelay.ErrUnauthenticated) {
			g.runtime.skipDirect(g.instance)
			path.fail(err)
			return g.runtime.dialExec(ctx, g.instance, g.port)
		}
		// One failed dial proves little (a burst of connections can overrun the
		// listener's backlog); this one is served over exec.
		if path.failures.Add(1) >= directDialFailures {
			g.runtime.skipDirect(g.instance)
			path.fail(fmt.Errorf("%d dials in a row failed, the last with: %w", directDialFailures, err))
		}
	}
	return g.runtime.dialExec(ctx, g.instance, g.port)
}

// directPath is the vsock endpoint of one workspace's workbench.
type directPath struct {
	runtime   *IncusRuntime
	instance  string
	cid, port uint32
	secret    []byte // this bridge's, known only to this process and its relay
	dead      atomic.Bool
	// failures counts the dials that failed since the last one that worked.
	failures atomic.Int32
	reported sync.Once
}

// dial connects to the relay and authenticates it, and it the gateway; only
// then is the connection used.
func (p *directPath) dial(ctx context.Context) (net.Conn, error) {
	conn, err := p.runtime.dialVsock(ctx, p.cid, p.port)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	err = vsockrelay.Authenticate(conn, p.secret)
	if !stop() && err == nil {
		err = ctx.Err() // the caller left; its deadline may be on the connection
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// fail retires the path and says so once.
func (p *directPath) fail(reason error) {
	p.dead.Store(true)
	p.reported.Do(func() {
		log.Printf("cage-tenant: workspace %s: the vsock path ended (%v); new connections use incus exec", p.instance, reason)
	})
}

// openDirect prepares the vsock path of a workspace: it finds the VM's vsock
// address, starts the bridge with a configuration of its own, and proves with
// an authenticated plain HTTP exchange that the relay reaches the workbench.
// A listener the gateway did not start is never used. The bridge ends with
// the workspace. The error says whether the path may be tried again:
// errDirectUnsupported does not, errDirectNotReady and any other error do not
// count against it.
func (r *IncusRuntime) openDirect(ctx context.Context, workspace *Workspace, instance string, port int) (path *directPath, err error) {
	id := strings.TrimPrefix(instance, "local:cage-")
	cid, err := r.vsockID(ctx, id)
	if err != nil {
		return nil, err
	}
	config, err := vsockrelay.NewConfig(port)
	if err != nil {
		return nil, err
	}
	path = &directPath{runtime: r, instance: instance, cid: cid, port: config.Listen, secret: config.Secret}
	// A bridge that did not become usable ends at once, not with the workspace.
	bridge, stopBridge := context.WithCancel(workspace.ctx)
	defer func() {
		if err != nil {
			stopBridge()
		}
	}()
	exited, err := r.startBridge(bridge, instance, config)
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, directWait)
	defer cancel()
	for {
		connected, probeErr := path.probe(wait, port)
		switch {
		case probeErr == nil:
			go func() {
				<-exited
				if workspace.ctx.Err() == nil {
					path.fail(errors.New("the bridge process ended"))
				} else {
					path.dead.Store(true)
				}
			}()
			return path, nil
		case errors.Is(probeErr, vsockrelay.ErrUnauthenticated):
			return nil, fmt.Errorf("%w: a listener on the bridge's port is not the bridge (%v)", errDirectUnsupported, probeErr)
		case connected:
			return nil, errDirectNotReady
		}
		select {
		case <-exited:
			return nil, fmt.Errorf("%w: the bridge in the guest ended at once, its cage may have no vsock relay", errDirectUnsupported)
		case <-wait.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: nothing answered as the bridge on the vsock port within %s", errDirectUnsupported, directWait)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// probe sends one request through the authenticated path and accepts any
// HTTP answer: that proves the bytes reach an HTTP server on the workbench's
// port. connected tells whether the relay answered at all, which separates a
// relay that is not listening yet from a workbench that does not answer.
func (p *directPath) probe(ctx context.Context, port int) (connected bool, err error) {
	conn, err := p.dial(ctx)
	if errors.Is(err, vsockrelay.ErrNotReady) {
		return true, err
	}
	if err != nil {
		return false, err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	request, err := http.NewRequest(http.MethodGet, "http://localhost:"+strconv.Itoa(port)+"/api/client/about", nil)
	if err != nil {
		return true, err
	}
	request.Close = true
	if err := request.Write(conn); err != nil {
		return true, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return true, err
	}
	return true, response.Body.Close()
}

// startBridge runs the guest's relay through exec and hands it its
// configuration on stdin, which stays open while the bridge runs: the relay
// ends when it closes. The returned channel closes when the process ends;
// cancelling ctx stops it.
func (r *IncusRuntime) startBridge(ctx context.Context, instance string, config vsockrelay.Config) (<-chan struct{}, error) {
	command := r.command(ctx, "exec", instance, "--mode=non-interactive", "--", relayCommand, "vsock-relay")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("the vsock bridge could not be started")
	}
	// The incus client forwards SIGTERM to the process in the guest, which ends
	// the listener; the client is killed if it does not leave in time.
	command.Cancel = func() error {
		_ = stdin.Close()
		return command.Process.Signal(syscall.SIGTERM)
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("the vsock bridge could not be started")
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	// A failed write means the process already ended; its exit says so.
	_, _ = io.WriteString(stdin, config.Line())
	return exited, nil
}

// vsockID returns the VM's vsock context ID, which Incus keeps in the
// instance's volatile configuration.
func (r *IncusRuntime) vsockID(ctx context.Context, id string) (uint32, error) {
	body, err := captureIncus(r.localCommand(ctx, "query", "local:/1.0/instances/cage-"+id+"?project="+r.cfg.Project), 1<<20)
	if err != nil {
		return 0, err
	}
	var record instance
	if err := json.Unmarshal(body, &record); err != nil || record.Name != "cage-"+id || record.Type != InstanceVM || record.Config[instanceLabel] != id {
		return 0, errors.New("Incus instance ownership does not match the workspace")
	}
	// 0 to 2 are the hypervisor, local and host addresses.
	cid, err := strconv.ParseUint(record.Config["volatile.vsock_id"], 10, 32)
	if err != nil || cid < 3 {
		return 0, fmt.Errorf("%w: the VM has no vsock address", errDirectUnsupported)
	}
	return uint32(cid), nil
}
