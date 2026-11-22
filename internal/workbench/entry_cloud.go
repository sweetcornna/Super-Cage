package workbench

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// The cloud entry's endpoint (design 5.2 steps 3-6): in a cloud workspace's
// workbench, behind the portal's gateway, the same SSH server as the local
// entry (entryServe) on a WebSocket the gateway opens after it admitted a
// device. It exists only in a workbench started as a cloud workspace's
// (cage serve --tenant-workspace), never on a computer's own controller,
// and only in a build with the cloud entry (cloudentry): it trusts the two
// headers the gateway sets, Cage-Entry-Device and Cage-Entry-Key, which the
// gateway strips from everything it proxies and never forwards
// /api/entry/ from a browser.
//
// The device must be in this workspace's own list (POST
// /api/entry/cloud/devices, from the gateway when the owner turns the entry
// on from the desktop), its key's fingerprint must be the one the gateway
// admitted, and the SSH user key must be that key: a gateway that is
// passive, or turns hostile after enrolment, cannot bring another key.
//
// The gateway does not touch() its workspace per byte; this side sends it a
// text message, cloudentry.ActiveMark, at most once per
// cloudentry.MarkInterval while the session carries the owner's activity:
// typing in an ssh terminal, an exec, scp or sftp moving data, a shell or
// command started, a Herdr frame of the owner's other than heartbeats and
// flow control (herdrlink.Options.OwnerActive). Keepalives never do.

// entryCloudSSHPath ends like /api/entry/ssh, through entryServe
// (server.go).
const entryCloudSSHPath = cloudentry.WorkbenchPath

// entryCloudServed: this workbench offers the cloud entry.
func (s *Server) entryCloudServed() bool {
	return s.tenantWorkspace && !s.desktopEntry && !s.clientOnly && cloudentry.Open()
}

// entryCloudRoutes registers the cloud endpoint and its device route, and
// nothing in any other workbench.
func (s *Server) entryCloudRoutes(mux routeRegistrar) {
	if !s.entryCloudServed() {
		return
	}
	// cloudentry.WorkbenchPath and DevicesPath, as literals for the route
	// classification (agent_access_test.go); a test keeps them equal.
	mux.HandleFunc("GET /api/entry/cloud/ssh", s.entryCloudSSH)
	mux.HandleFunc("POST /api/entry/cloud/devices", s.entryCloudDevice)
}

// entryCloudDevice adds or updates a device in this workspace's list and
// answers the endpoint's host key. The gateway calls it on its control
// connection when the owner turns the entry on from the desktop.
func (s *Server) entryCloudDevice(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(agentRequestHeader) != "" {
		entryRefuse(w, errEntryWindowOnly)
		return
	}
	var input struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
	if err != nil || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 || !cloudentry.ValidDevice(input.ID) || !validEntryDeviceName(input.Name) {
		http.Error(w, "设备无效", http.StatusBadRequest)
		return
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	var signer ssh.Signer
	err = func() error {
		s.entry.mu.Lock()
		defer s.entry.mu.Unlock()
		st, err := entryLoadState(s.profile)
		if err != nil {
			st = entryState{Version: entryStateVersion}
		}
		if record := st.device(input.ID); record != nil {
			// The gateway's ids follow keys: a known id keeps its key.
			if record.PublicKey != authorized {
				return errors.New("这个设备已登记了另一把钥匙")
			}
			record.Name = input.Name
		} else {
			st.Devices = append(st.Devices, entryDevice{ID: input.ID, Name: input.Name, PublicKey: authorized, Created: time.Now().UTC()})
		}
		st.Enabled = true
		if signer, err = entryHostSigner(s.profile, true); err != nil {
			return err
		}
		return entrySaveState(s.profile, st)
	}()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready && s.vmRunning() {
		s.entryStartWrapper()
	}
	jsonReply(w, map[string]string{"host_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))})
}

// entryCloudSSH is GET /api/entry/cloud/ssh: the gateway's splice of an
// admitted device's WebSocket. It decides like entrySSH and serves the
// same SSH endpoint.
func (s *Server) entryCloudSSH(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(agentRequestHeader) != "" {
		entryRefuse(w, errEntryWindowOnly)
		return
	}
	devices, keys := r.Header.Values(cloudentry.HeaderDevice), r.Header.Values(cloudentry.HeaderKey)
	if len(devices) != 1 || len(keys) != 1 || !cloudentry.ValidDevice(devices[0]) || !strings.HasPrefix(keys[0], "SHA256:") {
		entryRefuse(w, errEntryUnknown)
		return
	}
	conn := &entryConn{device: devices[0], cloudKey: keys[0]}
	conn.epoch, _ = r.Context().Value(accountEpochKey{}).(context.Context)
	if conn.epoch == nil {
		entryRefuse(w, errEntryLocked)
		return
	}
	conn.ctx, conn.cancel = context.WithCancelCause(r.Context())
	defer conn.cancel(nil)
	stopLock := context.AfterFunc(conn.epoch, func() { conn.cancel(errEntryLocked) })
	defer stopLock()
	s.entry.registry.add(conn)
	defer s.entry.registry.remove(conn)
	record, refusal := s.entryCheck(conn.device)
	if refusal == nil && !entryCloudKeyMatches(record, conn.cloudKey) {
		refusal = errEntryUnknown
	}
	if refusal == nil && conn.ctx.Err() != nil {
		refusal = errEntryClosing
		if cause, ok := context.Cause(conn.ctx).(*entryRefusal); ok {
			refusal = cause
		}
	}
	if refusal != nil {
		entryRefuse(w, refusal)
		return
	}
	signer, err := entryHostSigner(s.profile, false)
	if err != nil {
		entryRefuse(w, errEntryOff)
		return
	}
	header := http.Header{cloudentry.HostKeyHeader: {strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.origin }, ReadBufferSize: 16 << 10, WriteBufferSize: 16 << 10}
	ws, err := upgrader.Upgrade(w, r, header)
	if err != nil {
		return
	}
	defer ws.Close()
	transport := newEntryWSConn(ws)
	transport.stalled = func() {
		if !conn.stalled.Swap(true) {
			diagnostics.Log("info", "entry", "a cloud ssh connection ended: the client took no output for "+entryWriteTimeout.String()+"; its terminals are kept")
		}
		conn.cancel(errEntryStalled)
	}
	conn.active = transport.markActive
	s.entryServe(conn, transport, record, signer)
}

// entryCloudKeyMatches: the enrolled key is the one the gateway admitted.
func entryCloudKeyMatches(record entryDevice, fingerprint string) bool {
	enrolled, _, _, _, err := ssh.ParseAuthorizedKey([]byte(record.PublicKey))
	return err == nil && cloudentry.Fingerprint(enrolled.Marshal()) == fingerprint
}

// entryKeyAllowed is entryServe's key check: for the cloud entry, exactly
// the enrolled key the gateway admitted (no renewal of this computer's key,
// which is the local entry's); for the local entry, entryKeyAccepted.
func (s *Server) entryKeyAllowed(conn *entryConn, record entryDevice, key ssh.PublicKey) bool {
	if conn.cloudKey == "" {
		return s.entryKeyAccepted(record, key)
	}
	enrolled, _, _, _, err := ssh.ParseAuthorizedKey([]byte(record.PublicKey))
	return err == nil && entryKeysEqual(enrolled, key) && cloudentry.Fingerprint(key.Marshal()) == conn.cloudKey
}

// noteActive marks the owner's activity on a cloud connection; nothing on a
// local one.
func (c *entryConn) noteActive() {
	if c.active != nil {
		c.active()
	}
}

// activeReader is r, marking the owner's activity on each read that
// carried bytes (a cloud connection only).
func (c *entryConn) activeReader(r io.Reader) io.Reader {
	if c.active == nil {
		return r
	}
	return entryActiveReader{r: r, conn: c}
}

type entryActiveReader struct {
	r    io.Reader
	conn *entryConn
}

func (a entryActiveReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.conn.noteActive()
	}
	return n, err
}

// markActive sends the activity mark, at most once per
// cloudentry.MarkInterval; a call within it costs one atomic load.
func (c *entryWSConn) markActive() {
	now := time.Now().UnixNano()
	last := c.marked.Load()
	if last != 0 && time.Duration(now-last) < cloudentry.MarkInterval || !c.marked.CompareAndSwap(last, now) {
		return
	}
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		_ = c.ws.SetWriteDeadline(time.Now().Add(entryWriteTimeout))
		_ = c.ws.WriteMessage(websocket.TextMessage, []byte(cloudentry.ActiveMark))
	}()
}
