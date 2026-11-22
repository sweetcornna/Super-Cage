package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

var errController = errors.New("workspace controller is unavailable")
var errControllerSession = errors.New("workspace controller session has expired")

// idleConnTimeout is how long the pool keeps an idle guest connection. The
// workbench closes a connection that stays idle for 60 seconds, so the pool
// lets go of its own earlier and a reused connection is rarely one the guest
// is closing.
const idleConnTimeout = 45 * time.Second

// Each workspace has two budgets of simultaneous proxied requests. Streams
// (WebSockets and server-sent events) live for as long as a browser tab does,
// so they must not use up the places ordinary requests need.
const (
	maxRequestSlots = 64
	maxStreamSlots  = 128
)

type Workspace struct {
	origin          *url.URL
	cookie          *http.Cookie
	transport       *http.Transport
	control         *http.Transport
	ctx             context.Context
	cancel          context.CancelFunc
	slots           chan struct{}
	streamSlots     chan struct{} // the budget of streams; slots is the one of ordinary requests
	checkedAt       time.Time
	nextProbe       time.Time
	nextFaultProbe  time.Time
	faults          uint64
	verify          func(context.Context) error
	checkingSession atomic.Bool
	// lastIO is the Unix time in nanoseconds of the last proxied request or
	// stream byte. It feeds idle cooling and is updated without the server lock.
	lastIO atomic.Int64
	// awake ends with every freeze, closing the requests and streams that were
	// proxied before it. Guarded by the server lock.
	awake context.Context
	doze  context.CancelFunc
}

func newWorkspace(port int, dial func(context.Context) (net.Conn, error)) *Workspace {
	ctx, cancel := context.WithCancel(context.Background())
	origin := &url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", strconv.Itoa(port))}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != origin.Host {
				return nil, errController
			}
			return dial(ctx)
		},
		// Every new guest connection starts a transport process in the VM.
		// Keep enough warm connections for the workbench's polling and its
		// reconnecting streams instead of paying that start on each request.
		MaxConnsPerHost: maxRequestSlots + maxStreamSlots, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		IdleConnTimeout: idleConnTimeout, ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: 64 << 10,
	}
	// Reserve one connection for rechecks and one for fault confirmation.
	// Long-lived HTTP and WebSocket traffic cannot occupy either slot.
	control := transport.Clone()
	control.MaxConnsPerHost = 2
	control.MaxIdleConns = 1
	control.MaxIdleConnsPerHost = 1
	awake, doze := context.WithCancel(ctx)
	workspace := &Workspace{origin: origin, transport: transport, control: control, ctx: ctx, cancel: cancel, slots: make(chan struct{}, maxRequestSlots), streamSlots: make(chan struct{}, maxStreamSlots), awake: awake, doze: doze}
	workspace.touch()
	return workspace
}

func (w *Workspace) touch() { w.lastIO.Store(time.Now().UnixNano()) }

func (w *Workspace) lastActive() time.Time { return time.Unix(0, w.lastIO.Load()) }

// sleep ends every proxied request and stream of a workspace that is about to
// freeze and drops its idle guest connections. A browser reconnects and its
// next request wakes the workspace. Caller holds the server lock.
func (w *Workspace) sleep() {
	doze := w.doze
	w.awake, w.doze = context.WithCancel(w.ctx)
	doze()
	w.transport.CloseIdleConnections()
	w.control.CloseIdleConnections()
}

func (w *Workspace) close() {
	w.cancel()
	w.transport.CloseIdleConnections()
	w.control.CloseIdleConnections()
}

func (w *Workspace) request(ctx context.Context, method, path, token string) (*http.Response, error) {
	var body io.Reader
	if token != "" {
		data, _ := json.Marshal(map[string]string{"token": token})
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, w.origin.String()+path, body)
	if err != nil {
		return nil, errController
	}
	request.Header.Set("Origin", w.origin.String())
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if w.cookie != nil {
		request.AddCookie(w.cookie)
	}
	response, err := w.control.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			return nil, errControllerSession
		}
		return nil, errController
	}
	return response, nil
}

func (w *Workspace) bootstrap(ctx context.Context, launcherToken string) error {
	if !secretPattern.MatchString(launcherToken) {
		return errController
	}
	response, err := w.request(ctx, http.MethodPost, "/api/launch", launcherToken)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	response.Body.Close()
	var launch struct {
		URL            string `json:"url"`
		ClientOnly     bool   `json:"client_only"`
		DesktopGateway bool   `json:"desktop_gateway"`
	}
	if err != nil || len(body) > 8192 || json.Unmarshal(body, &launch) != nil || launch.ClientOnly || launch.DesktopGateway {
		return errController
	}
	entry, err := url.Parse(launch.URL)
	if err != nil || entry.Scheme != w.origin.Scheme || entry.Host != w.origin.Host || entry.User != nil || entry.Path != "/" || !secretPattern.MatchString(entry.Fragment) {
		return errController
	}
	session, err := w.request(ctx, http.MethodPost, "/api/session", entry.Fragment)
	if err != nil {
		return err
	}
	defer session.Body.Close()
	if size, err := io.Copy(io.Discard, io.LimitReader(session.Body, 8193)); err != nil || size > 8192 {
		return errController
	}
	name := "cage_session_" + w.origin.Port()
	var cookie *http.Cookie
	for _, candidate := range session.Cookies() {
		if candidate.Name == name {
			if cookie != nil || !secretPattern.MatchString(candidate.Value) || candidate.Domain != "" || candidate.Path != "/" {
				return errController
			}
			cookie = &http.Cookie{Name: name, Value: candidate.Value}
		}
	}
	if cookie == nil {
		return errController
	}
	w.cookie = cookie
	return w.check(ctx)
}

func (w *Workspace) check(ctx context.Context) error {
	response, err := w.request(ctx, http.MethodGet, "/api/client/about", "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	size, err := io.Copy(io.Discard, io.LimitReader(response.Body, 8193))
	if err != nil {
		return err
	}
	if size > 8192 {
		return errController
	}
	return nil
}

func (w *Workspace) recheck(ctx context.Context) error {
	if w.verify != nil {
		if err := w.verify(ctx); err != nil {
			return err
		}
	}
	return w.check(ctx)
}
