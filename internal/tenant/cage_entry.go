package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// The cloud entry's gateway (design 5.2): the owner's cage entry proxy opens
// wss://vm-<id>.<portal>/_cage/entry with a signed admission, the gateway
// checks it and splices the WebSocket to the workspace's workbench
// (/api/entry/cloud/ssh), where SSH ends. The gateway holds no SSH key and
// never looks into the binary messages it copies: it sees the device, the
// VM, when, and the sizes and timing of encrypted SSH packets.
//
// Idle cooling still works: the splice does not touch() per byte, so Herdr's
// heartbeats and SSH keepalives never keep a workspace awake. The workbench
// sends a text message, cloudentry.ActiveMark, when the session carries the
// owner's activity, and only that touches, at most once per
// cloudentry.MarkInterval. A cooled workspace is woken only by a dial that
// asks for it (cloudentry.HeaderWake, sent by an interactive ssh), never by
// Herdr's re-dials.
//
// Nothing of this runs while no cloud entry is connected: no goroutine, no
// timer. The nonce cache is pruned when an admission adds to it.

// cageEntryState is the entry's state in the server. Guarded by s.mu.
type cageEntryState struct {
	// nonces are the admissions seen in the last cloudentry.NonceWindow,
	// by device and nonce.
	nonces map[string]time.Time
	pruned time.Time
	// devices are the live states of the devices that connected
	// (devices.go).
	devices map[string]*deviceLive
}

const (
	maxEntryNonces = 1 << 16
	// entryRelayBuffer is each socket's read and write buffer; an SSH
	// packet larger than it goes through in more than one frame.
	entryRelayBuffer = 16 << 10
	// entryMessageLimit bounds one message in either direction (the
	// workbench and the proxy write SSH packets, at most 256 KiB).
	entryMessageLimit = 1 << 20
	// entryCloseLinger is how long one side's close waits for the other
	// side to answer it, so the last bytes are read, not reset.
	entryCloseLinger = time.Second
)

// entryNonceLocked records a nonce and reports whether it was unseen. The
// cache is pruned here, when it is used, never on a timer. Caller holds s.mu.
func (s *Server) entryNonceLocked(key string, now time.Time) bool {
	state := &s.entryState
	if state.nonces == nil {
		state.nonces = make(map[string]time.Time)
	}
	if seen, ok := state.nonces[key]; ok && now.Sub(seen) < cloudentry.NonceWindow {
		return false
	}
	if len(state.nonces) >= maxEntryNonces/2 || now.Sub(state.pruned) >= cloudentry.NonceWindow {
		for other, seen := range state.nonces {
			if now.Sub(seen) >= cloudentry.NonceWindow {
				delete(state.nonces, other)
			}
		}
		state.pruned = now
	}
	if len(state.nonces) >= maxEntryNonces {
		return false
	}
	state.nonces[key] = now
	return true
}

// vmHost is the workspace's host as the owner dials it, the one a device
// signs.
func (s site) vmHost(id string) string {
	return strings.TrimPrefix(s.vmOrigin(id), s.scheme+"://")
}

func headerToken(headers http.Header, name, token string) bool {
	for _, value := range headers.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// cageEntry answers /_cage/entry on a workspace's host.
func (s *Server) cageEntry(w http.ResponseWriter, r *http.Request, id string) {
	// Closed in every build until the cloud entry is verified: not found,
	// before any header is read.
	if !cloudentry.Open() {
		notFound(w)
		return
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Upgrade")) != 1 ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerToken(r.Header, "Connection", "upgrade") {
		entryRefusal(w, http.StatusBadRequest, "invalid", "本机终端的入口只接受 cage entry proxy 的连接")
		return
	}
	// Only cage entry proxy dials this; a browser always sends Origin with
	// a WebSocket, and a page must never reach it.
	if len(r.Header.Values("Origin")) != 0 || len(r.Header.Values("Sec-Fetch-Site")) != 0 {
		entryRefusal(w, http.StatusForbidden, "origin_refused", "本机终端的入口不接受浏览器的连接")
		return
	}
	values := r.Header.Values("Authorization")
	var auth cloudentry.Authorization
	ok := len(values) == 1
	if ok {
		auth, ok = cloudentry.Parse(values[0])
	}
	unknown := func() {
		entryRefusal(w, http.StatusUnauthorized, "unauthenticated", "门户没有认出这台电脑：在 Super Cage 里重新开启这个工作区的本机终端")
	}
	if !ok {
		unknown()
		return
	}
	wakeValues := r.Header.Values(cloudentry.HeaderWake)
	wake := len(wakeValues) == 1 && wakeValues[0] == "1"
	now := s.now()
	if !auth.Fresh(now) {
		entryRefusal(w, http.StatusUnauthorized, "clock", "这台电脑的时间和门户相差超过 1 分钟：校准时间后再连接")
		return
	}
	s.mu.Lock()
	if !s.allowRate("cage-entry", s.clientIP(r), 120, now) {
		s.mu.Unlock()
		entryRefusal(w, http.StatusTooManyRequests, "rate_limited", "连接过于频繁，稍后再试")
		return
	}
	device, known := s.store.Devices[auth.Device]
	s.mu.Unlock()
	key, blob, keyOK := cloudentry.ParseAuthorizedKey(device.PublicKey)
	if !known || !keyOK || !auth.Verify(key, s.site.vmHost(id), id) {
		unknown()
		return
	}
	s.mu.Lock()
	if refusal := s.admitLocked(auth, id, now); refusal != nil {
		s.mu.Unlock()
		refusal(w)
		return
	}
	if s.coolingLocked(id) {
		if !wake {
			s.mu.Unlock()
			entryRefusal(w, http.StatusConflict, "asleep", "工作区已休眠：在终端里直接用 ssh 连接会唤醒它，Herdr 会在它醒来后自己连上")
			return
		}
		s.mu.Unlock()
		if s.wake(r.Context(), id) != nil {
			unavailable(w)
			return
		}
		s.mu.Lock()
		// The device may have been revoked while the workspace resumed.
		if refusal := s.admittedLocked(auth.Device, id); refusal != nil {
			s.mu.Unlock()
			refusal(w)
			return
		}
	}
	workspace := s.workspaces[id]
	if workspace == nil || workspace.checkedAt.IsZero() {
		if !wake {
			s.mu.Unlock()
			if workspace == nil {
				entryRefusal(w, http.StatusConflict, "stopped", "工作区已停止：在终端里直接用 ssh 连接或在门户打开它，都会启动它")
			} else {
				entryRefusal(w, http.StatusServiceUnavailable, "unavailable", "工作区暂时不可用，稍后再连接")
			}
			return
		}
		// As a portal open does: start (or recheck) it; the proxy retries.
		if !s.store.blocked {
			s.noteActivityLocked(id, now)
			s.startVMLocked(s.store.VMs[id], now)
		}
		s.mu.Unlock()
		entryRefusal(w, http.StatusServiceUnavailable, "starting", "工作区正在启动，稍后再连接")
		return
	}
	live := s.deviceLiveLocked(auth.Device)
	live.lastUsed = now
	awake, cookie := workspace.awake, workspace.cookie
	s.mu.Unlock()
	if cookie == nil {
		unavailable(w)
		return
	}
	release, admitted := workspace.acquire(r)
	if !admitted {
		entryRefusal(w, http.StatusTooManyRequests, "rate_limited", "这个工作区的连接已满，稍后再试")
		return
	}
	defer release()
	// The relay hangs on the device (revoked), the workspace (stopped or
	// gone) and its awake context (frozen): any of them ends both sockets.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	for _, parent := range []context.Context{live.ctx, workspace.ctx, awake} {
		stop := context.AfterFunc(parent, cancel)
		defer stop()
	}
	if ctx.Err() != nil {
		unavailable(w)
		return
	}
	bench, hostKey, refusal := workspace.dialEntry(ctx, cookie, auth.Device, cloudentry.Fingerprint(blob))
	if refusal != nil {
		refusal(w)
		return
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize: entryRelayBuffer, WriteBufferSize: entryRelayBuffer,
		// Origin was refused above.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	client, err := upgrader.Upgrade(w, r, http.Header{cloudentry.HostKeyHeader: {hostKey}})
	if err != nil {
		_ = bench.Close()
		return
	}
	relayEntry(ctx, cancel, client, bench, workspace.touch)
}

// admitLocked checks an admission whose signature verified: the device is
// not revoked, its account exists and owns the VM, the device is within its
// rate and the nonce is new. Caller holds s.mu.
func (s *Server) admitLocked(auth cloudentry.Authorization, id string, now time.Time) func(http.ResponseWriter) {
	if refusal := s.admittedLocked(auth.Device, id); refusal != nil {
		return refusal
	}
	if !s.allowRate("cage-entry-device", auth.Device, 60, now) {
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusTooManyRequests, "rate_limited", "连接过于频繁，稍后再试")
		}
	}
	if !s.entryNonceLocked(auth.Device+"/"+auth.Nonce, now) {
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusUnauthorized, "replay", "这次连接的签名已经用过：重新连接即可")
		}
	}
	return nil
}

// admittedLocked: the device is still enrolled and not revoked, its account
// is active and owns the VM (the owner check of validGrant). Caller holds s.mu.
func (s *Server) admittedLocked(deviceID, id string) func(http.ResponseWriter) {
	device, known := s.store.Devices[deviceID]
	vm, exists := s.store.VMs[id]
	switch {
	case !known:
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusUnauthorized, "unauthenticated", "门户没有认出这台电脑：在 Super Cage 里重新开启这个工作区的本机终端")
		}
	case device.Revoked != nil:
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusForbidden, "revoked", "这台电脑的钥匙已在门户撤销：在 Super Cage 里重新开启本机终端")
		}
	case !s.ownerActiveLocked(device.Owner):
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusForbidden, "account", "这个门户账户已停用")
		}
	case !exists || vm.Owner != device.Owner:
		// The same answer whether the VM exists under another account or
		// not at all.
		return func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusForbidden, "not_owner", "这台电脑登记的账户没有这个工作区")
		}
	}
	return nil
}

// dialEntry opens the workbench's cloud endpoint with the two headers only
// the gateway sets, and returns its socket and host key. A refusal from the
// workbench reaches the owner in its own words.
func (w *Workspace) dialEntry(ctx context.Context, cookie *http.Cookie, device, fingerprint string) (*websocket.Conn, string, func(http.ResponseWriter)) {
	dialer := websocket.Dialer{
		NetDialContext:   w.transport.DialContext,
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   entryRelayBuffer, WriteBufferSize: entryRelayBuffer,
	}
	header := http.Header{}
	header.Set("Origin", w.origin.String())
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set(cloudentry.HeaderDevice, device)
	header.Set(cloudentry.HeaderKey, fingerprint)
	bench, response, err := dialer.DialContext(ctx, "ws://"+w.origin.Host+cloudentry.WorkbenchPath, header)
	if err != nil {
		if response != nil && response.StatusCode >= 400 && response.StatusCode < 600 {
			var refusal struct{ Code, Message string }
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			if json.Unmarshal(body, &refusal) == nil && validRefusal(refusal.Code, refusal.Message) {
				status := response.StatusCode
				return nil, "", func(w http.ResponseWriter) { entryRefusal(w, status, refusal.Code, refusal.Message) }
			}
		}
		return nil, "", func(w http.ResponseWriter) {
			entryRefusal(w, http.StatusServiceUnavailable, "workspace", "工作区的本机终端没有响应：确认工作区已更新到支持本机终端的版本")
		}
	}
	hostKey := response.Header.Values(cloudentry.HostKeyHeader)
	if len(hostKey) != 1 {
		_ = bench.Close()
		return nil, "", func(w http.ResponseWriter) { unavailable(w) }
	}
	if _, _, ok := cloudentry.ParseAuthorizedKey(hostKey[0]); !ok {
		_ = bench.Close()
		return nil, "", func(w http.ResponseWriter) { unavailable(w) }
	}
	return bench, strings.Join(strings.Fields(hostKey[0])[:2], " "), nil
}

// validRefusal accepts the workbench's refusal as plain words: a short code
// and one line of text.
func validRefusal(code, message string) bool {
	if code == "" || len(code) > 32 || message == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > 200 {
		return false
	}
	for _, c := range code {
		if !('a' <= c && c <= 'z') && c != '_' {
			return false
		}
	}
	return strings.IndexFunc(message, unicode.IsControl) < 0
}

// relayEntry splices the two sockets until either ends or ctx does. Binary
// messages are copied as they are, both ways, and never touch(). The
// workbench's activity marks become touch(), at most once per
// cloudentry.MarkInterval; nothing else from its text messages, and nothing
// of the owner's, is read.
func relayEntry(ctx context.Context, cancel context.CancelFunc, client, bench *websocket.Conn, touch func()) {
	client.SetReadLimit(entryMessageLimit)
	bench.SetReadLimit(entryMessageLimit)
	stop := context.AfterFunc(ctx, func() {
		_ = client.NetConn().Close()
		_ = bench.NetConn().Close()
	})
	defer stop()
	var last atomic.Int64
	mark := func() {
		now := time.Now().UnixNano()
		previous := last.Load()
		if (previous == 0 || time.Duration(now-previous) >= cloudentry.MarkInterval) && last.CompareAndSwap(previous, now) {
			touch()
		}
	}
	up := make(chan struct{})
	go func() {
		defer close(up)
		copyEntry(client, bench, nil)
	}()
	down := make(chan struct{})
	go func() {
		defer close(down)
		copyEntry(bench, client, mark)
	}()
	// When one side ends, the other gets its close and a moment to answer.
	var linger <-chan time.Time
	select {
	case <-up:
		linger = time.After(entryCloseLinger)
		select {
		case <-down:
		case <-linger:
		case <-ctx.Done():
		}
	case <-down:
		linger = time.After(entryCloseLinger)
		select {
		case <-up:
		case <-linger:
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
	cancel()
	<-up
	<-down
}

// copyEntry copies from one socket to the other until reading fails; a
// close is passed on with its code. mark is the workbench side's activity.
func copyEntry(from, to *websocket.Conn, mark func()) {
	for {
		kind, reader, err := from.NextReader()
		if err != nil {
			code := websocket.CloseNormalClosure
			var closed *websocket.CloseError
			if errors.As(err, &closed) && closed.Code != websocket.CloseNoStatusReceived && closed.Code != websocket.CloseAbnormalClosure {
				code = closed.Code
			}
			_ = to.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(entryCloseLinger))
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			writer, err := to.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			if _, err := io.Copy(writer, reader); err != nil {
				return
			}
			if writer.Close() != nil {
				return
			}
		case websocket.TextMessage:
			if mark == nil {
				continue
			}
			text, err := io.ReadAll(io.LimitReader(reader, int64(len(cloudentry.ActiveMark))+1))
			if err == nil && string(text) == cloudentry.ActiveMark {
				mark()
			}
		}
	}
}
