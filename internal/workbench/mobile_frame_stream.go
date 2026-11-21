package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

const (
	// A held read ends before the device link's 35 s response-header timeout
	// even when its last look at the guest runs into the guest's own 15 s limit.
	phoneFrameMaxHold = 15 * time.Second
	// How long a frame read may rely on the gate and configuration it saw.
	phoneGateTTL = time.Second
)

var phoneFrameTag = regexp.MustCompile(`^[0-9a-f]{32}$`)

// phoneCall is one validated request of the phone to a graphical surface.
type phoneCall struct {
	surface     phoneSurface
	op          string
	input       phoneSurfaceInput
	room, epoch context.Context
	state       *phoneSurfaceState
	defaultHome bool
	frame       mobilelink.FrameQuery // zero for the original reads
	// deferGate leaves the check that guards pixels to the caller.
	deferGate bool
	// Set by phoneSurfaceExecute while it holds the surface lock.
	session *phoneSurfaceSession
	seq     uint64
}

// phoneBuffer captures the reply of one look at the guest so a held read can
// decide whether it carries anything new before any byte reaches the phone.
type phoneBuffer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newPhoneBuffer() *phoneBuffer { return &phoneBuffer{header: http.Header{}} }

func (b *phoneBuffer) Header() http.Header { return b.header }

func (b *phoneBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *phoneBuffer) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

func (b *phoneBuffer) copyTo(w http.ResponseWriter) {
	for name, values := range b.header {
		w.Header()[name] = values
	}
	status := b.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}

// phoneHolder allows one held frame read per surface. A newer read replaces
// the older one: a phone that asks again no longer wants the previous answer,
// and the device link only has a few streams to share with everything else.
type phoneHolder struct {
	mu  sync.Mutex
	cur chan struct{}
}

func (h *phoneHolder) begin() (superseded <-chan struct{}, end func()) {
	mine := make(chan struct{})
	h.mu.Lock()
	if h.cur != nil {
		close(h.cur)
	}
	h.cur = mine
	h.mu.Unlock()
	return mine, func() {
		h.mu.Lock()
		if h.cur == mine {
			h.cur = nil
		}
		h.mu.Unlock()
	}
}

// phoneGateCache keeps the last gate status and configuration fingerprint
// that authorized the phone. Only a grant is stored, never a refusal or an
// error, and the grant is judged again against the current time on every use.
type phoneGateCache struct {
	mu          sync.Mutex
	at          time.Time
	fingerprint string
	gate        cage.GateStatus
	ttl         time.Duration                                                   // zero means phoneGateTTL
	fetch       func(*Server, context.Context) (string, cage.GateStatus, error) // replaces the real lookup in tests
}

func (s *Server) phoneGateLookup(ctx context.Context) (string, cage.GateStatus, error) {
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return "", cage.GateStatus{}, err
	}
	client, err := s.gateClient()
	if err != nil {
		return "", cage.GateStatus{}, err
	}
	gate, err := client.Status(ctx)
	return cfg.Fingerprint(), gate, err
}

// phoneNetwork reports whether the phone may use the guest now. The server
// flags are always read live. With cached set, a grant seen within the last
// phoneGateTTL stands in for the configuration read and the gate query.
func (s *Server) phoneNetwork(ctx context.Context, cache *phoneGateCache, cached bool) bool {
	s.mu.Lock()
	blocked := !s.ready || s.closing || s.networkPaused || s.proxyNetworkLost
	s.mu.Unlock()
	if blocked {
		return false
	}
	var fingerprint string
	var gate cage.GateStatus
	hit := false
	cache.mu.Lock()
	ttl, fetch := cache.ttl, cache.fetch
	if ttl == 0 {
		ttl = phoneGateTTL
	}
	if cached && !cache.at.IsZero() && time.Since(cache.at) < ttl {
		fingerprint, gate, hit = cache.fingerprint, cache.gate, true
	}
	cache.mu.Unlock()
	if !hit {
		lookup := s.phoneGateLookup
		if fetch != nil {
			lookup = func(ctx context.Context) (string, cage.GateStatus, error) { return fetch(s, ctx) }
		}
		var err error
		if fingerprint, gate, err = lookup(ctx); err != nil {
			return false
		}
	}
	now := time.Now()
	ok := gate.Configuration == fingerprint && gate.Mode == cage.GateOpen && gate.PolicyVerified &&
		!gate.Fault && gate.Deadline().After(now) && gate.Report != nil && gate.Report.Authorizes(gate.RunID, gate.Configuration, now) == nil
	cache.mu.Lock()
	switch {
	case ok && !hit:
		cache.at, cache.fingerprint, cache.gate = now, fingerprint, gate
	case !ok:
		cache.at = time.Time{}
	}
	cache.mu.Unlock()
	return ok
}

// phoneFrame is a guest frame object seen as fields plus its identity.
type phoneFrame struct {
	fields    map[string]json.RawMessage
	tag       string
	unchanged bool
}

// phoneFrameDigest names a frame by its fields in sorted key order, so a guest
// that serializes the same frame with another key order or spacing (a Swift
// dictionary, for one) still gets the same name.
func phoneFrameDigest(fields map[string]json.RawMessage) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write(bytes.TrimSpace(fields[name]))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// parsePhoneFrame names a frame by the guest's tag, or by a digest of its
// fields when the guest has none (the Linux, Windows and macOS desktops).
func parsePhoneFrame(body []byte, after string) (phoneFrame, bool) {
	var f phoneFrame
	if json.Unmarshal(body, &f.fields) != nil || f.fields == nil {
		return f, false
	}
	f.unchanged = string(f.fields["unchanged"]) == "true"
	var tag string
	switch {
	case json.Unmarshal(f.fields["frame_tag"], &tag) == nil && phoneFrameTag.MatchString(tag):
		f.tag = tag
	case f.unchanged:
		f.tag = after
	default:
		f.tag = phoneFrameDigest(f.fields)
	}
	return f, true
}

// phoneFrameJPEG splits the picture off a frame object. ok is false when the
// image is not a JPEG or the remaining fields do not fit one header; the
// caller then answers with the object as JSON.
func phoneFrameJPEG(fields map[string]json.RawMessage) (jpeg []byte, meta string, ok bool) {
	var encoded string
	if json.Unmarshal(fields["image"], &encoded) != nil || encoded == "" {
		return nil, "", false
	}
	jpeg, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(jpeg) < 4 || jpeg[0] != 0xff || jpeg[1] != 0xd8 || jpeg[2] != 0xff {
		return nil, "", false
	}
	rest := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		if name != "image" {
			rest[name] = value
		}
	}
	data, err := json.Marshal(rest)
	if err != nil {
		return nil, "", false
	}
	meta = base64.RawURLEncoding.EncodeToString(data)
	if !mobilelink.ValidFrameMeta(meta) {
		return nil, "", false
	}
	return jpeg, meta, true
}

func phoneFrameDelays(surface phoneSurface) (floor, ceiling time.Duration) {
	if surface == phoneDesktop {
		return 150 * time.Millisecond, 800 * time.Millisecond
	}
	return 100 * time.Millisecond, time.Second
}

// phoneFrameStream answers a negotiated frame read: the next frame after the
// phone's tag, as a JPEG body when asked, held for up to the requested time
// while the picture stays the same. Nothing is held under the surface lock, so
// taps, keys and resizes run while a read waits. The guest cannot push, so a
// waiting read looks at it again after a short pause that grows while the
// picture stays still and starts over with the next read.
func (s *Server) phoneFrameStream(call *phoneCall, w http.ResponseWriter, r *http.Request) {
	q, state := call.frame, call.state
	hold := min(q.Wait, phoneFrameMaxHold)
	var superseded <-chan struct{}
	if hold > 0 {
		var end func()
		superseded, end = state.hold.begin()
		defer end()
	}
	deadline := time.Now().Add(hold)
	delay, ceiling := phoneFrameDelays(call.surface)
	call.deferGate = true
	for {
		buf := newPhoneBuffer()
		s.phoneSurfaceExecute(call, buf, r)
		if r.Context().Err() != nil {
			return // The phone went away; nobody is left to answer.
		}
		if buf.status != http.StatusOK {
			buf.copyTo(w)
			return
		}
		f, ok := parsePhoneFrame(buf.body.Bytes(), q.After)
		if !ok {
			http.Error(w, "客机返回的画面无效", http.StatusBadGateway)
			return
		}
		if !f.unchanged && (q.After == "" || f.tag != q.After) {
			s.releasePhoneFrame(call, f, w, r)
			return
		}
		remaining := time.Until(deadline)
		select {
		case <-superseded:
			remaining = 0
		default:
		}
		if remaining <= 0 {
			phoneUnchanged(w, call.input.ID, q.After)
			return
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-call.session.ctx.Done():
			timer.Stop() // The next look reports the ended session.
		case <-superseded:
			timer.Stop()
		}
		delay = min(delay*3/2, ceiling)
	}
}

func phoneUnchanged(w http.ResponseWriter, id, tag string) {
	body, _ := json.Marshal(map[string]any{"id": id, "frame_tag": tag, "unchanged": true})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(mobilelink.FrameStreamHeader, "1")
	_, _ = w.Write(body)
}

// releasePhoneFrame sends a frame that differs from the phone's. The lock and
// authorization checks run again here, after the capture, exactly where the
// single-shot path runs them.
func (s *Server) releasePhoneFrame(call *phoneCall, f phoneFrame, w http.ResponseWriter, r *http.Request) {
	b, state := call.session, call.state
	label := call.surface.label()
	if b.ctx.Err() != nil || call.epoch.Err() != nil || call.room.Err() != nil {
		http.Error(w, label+"授权已结束", http.StatusGone)
		return
	}
	if !s.phoneSurfaceNetwork(r.Context(), state) {
		state.op.Lock()
		s.retirePhoneSurface(call.surface, b)
		state.op.Unlock()
		http.Error(w, "电脑上的 Super Cage 网络检查已暂停", http.StatusConflict)
		return
	}
	f.fields["frame_tag"] = json.RawMessage(strconv.Quote(f.tag))
	f.fields["frame_seq"] = json.RawMessage(strconv.FormatUint(call.seq, 10))
	defer interruptHTTPOnCancel(w, r)()
	w.Header().Set(mobilelink.FrameStreamHeader, "1")
	if call.frame.Binary {
		if jpeg, meta, ok := phoneFrameJPEG(f.fields); ok {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set(mobilelink.FrameMetaHeader, meta)
			_, _ = w.Write(jpeg)
			return
		}
	}
	body, err := json.Marshal(f.fields)
	if err != nil {
		http.Error(w, "客机返回的画面无效", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
