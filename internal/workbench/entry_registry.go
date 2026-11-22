package workbench

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylvinus/agent-vm/internal/entryproxy"
)

var (
	errEntryLocalOnly = entryproxy.ErrLocalOnly
	errEntryOff       = &entryRefusal{Status: http.StatusConflict, Code: "off", Message: "本机终端未在这个环境里开启"}
	errEntryUnknown   = &entryRefusal{Status: http.StatusForbidden, Code: "device_unknown", Message: "这台电脑还没有加入这个环境的本机终端：在 Super Cage 里重新开启"}
	errEntryRevoked   = &entryRefusal{Status: http.StatusForbidden, Code: "device_revoked", Message: "这台电脑的钥匙已撤销：在 Super Cage 里重新开启本机终端即可生成新钥匙"}
	// errEntryRevocationsUnreadable: revoked.json exists but cannot be
	// read, so every key is refused (N4 of the second review).
	errEntryRevocationsUnreadable = &entryRefusal{Status: http.StatusForbidden, Code: "revocations_unreadable", Message: "这台电脑的撤销记录（Super Cage 的 entry/revoked.json）无法读取：在修好或删除它之前，本机终端拒绝所有钥匙"}
	errEntryLocked                = entryproxy.ErrLocked
	errEntryUnsupported           = &entryRefusal{Status: http.StatusConflict, Code: "unsupported", Message: "这个环境暂不支持本机终端"}
	errEntryStopped               = &entryRefusal{Status: http.StatusConflict, Code: "stopped", Message: "环境已停止：先在 Super Cage 里启动环境"}
	errEntryStarting              = &entryRefusal{Status: http.StatusConflict, Code: "starting", Message: "环境正在启动或停止：稍后再连接"}
	errEntryPaused                = &entryRefusal{Status: http.StatusConflict, Code: "paused", Message: "网络已暂停：本机终端会在恢复网络后重新可用"}
	errEntryClosing               = entryproxy.ErrClosing
	errEntryWindowOnly            = &entryRefusal{Status: http.StatusForbidden, Code: "window_only", Message: "本机终端只能在 Super Cage 窗口或 cage entry 里管理"}
)

// entryConn is one live connection of the entry: one WebSocket carrying one
// SSH connection. Cancelling its context ends everything it started.
type entryConn struct {
	device string
	ctx    context.Context
	cancel context.CancelCauseFunc
	// epoch is the account unlock the dial was admitted under; it ends when
	// Super Cage locks.
	epoch context.Context
	// flush holds a cut back until terminal sessions wrote their reset.
	flush entryFlush
	// What the connection runs now, for the status line.
	shells, execs, sftps atomic.Int32
	channels             atomic.Int32
	// refused counts each request or channel type the endpoint refused, so
	// tests can tell "the client never asked" from "asked and refused".
	refusedMu sync.Mutex
	refused   map[string]int
}

func (c *entryConn) refuse(kind string) {
	c.refusedMu.Lock()
	defer c.refusedMu.Unlock()
	if c.refused == nil {
		c.refused = map[string]int{}
	}
	c.refused[kind]++
}

func (c *entryConn) refusals() map[string]int {
	c.refusedMu.Lock()
	defer c.refusedMu.Unlock()
	out := make(map[string]int, len(c.refused))
	for k, v := range c.refused {
		out[k] = v
	}
	return out
}

// entryFlush counts the terminal sessions of a connection that still write
// their last bytes (the reset of design 5.7) before a cut closes it.
type entryFlush struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to 0
}

func (f *entryFlush) begin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

func (f *entryFlush) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n--; f.n == 0 {
		close(f.idle)
	}
}

// wait returns when no session is writing any more, or after limit.
func (f *entryFlush) wait(limit time.Duration) {
	f.mu.Lock()
	idle, n := f.idle, f.n
	f.mu.Unlock()
	if n == 0 {
		return
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	}
}

// entryRegistry holds every live connection of this environment's entry.
// cut only cancels contexts: it takes no other lock and does no I/O, so it
// may run where the caller holds s.mu (the network pause path does).
type entryRegistry struct {
	mu    sync.Mutex
	conns map[*entryConn]struct{}
	// observe, when set, sees every connection that ends (tests).
	observe func(*entryConn)
}

func (r *entryRegistry) add(c *entryConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = map[*entryConn]struct{}{}
	}
	r.conns[c] = struct{}{}
}

func (r *entryRegistry) remove(c *entryConn) {
	r.mu.Lock()
	_, present := r.conns[c]
	delete(r.conns, c)
	observe := r.observe
	r.mu.Unlock()
	if present && observe != nil {
		observe(c)
	}
}

// cut cancels the connections match selects (all when match is nil) with
// the reason the next dial will be refused for.
func (r *entryRegistry) cut(match func(*entryConn) bool, cause *entryRefusal) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for c := range r.conns {
		if match == nil || match(c) {
			c.cancel(cause)
			n++
		}
	}
	return n
}

func (r *entryRegistry) cutAll(cause *entryRefusal) int { return r.cut(nil, cause) }

func (r *entryRegistry) cutDevice(device string, cause *entryRefusal) int {
	return r.cut(func(c *entryConn) bool { return c.device == device }, cause)
}

type entryConnections struct {
	Total int `json:"total"`
	Shell int `json:"shell"`
	Exec  int `json:"exec"`
	SFTP  int `json:"sftp"`
}

func (r *entryRegistry) counts() entryConnections {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out entryConnections
	for c := range r.conns {
		if c.ctx.Err() != nil {
			continue
		}
		out.Total++
		out.Shell += int(c.shells.Load())
		out.Exec += int(c.execs.Load())
		out.SFTP += int(c.sftps.Load())
	}
	return out
}
