package mobilelink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// hostileRelay is a working relay (the real relay.Server behind it) that also
// reads everything it carries, knows every room's tokens, and can replace the
// carrying of a joiner's connection (client) or delay it (hold).
type hostileRelay struct {
	inner    *relay.Server
	innerURL string
	URL      string

	mu         sync.Mutex
	seen       [][]byte
	fromJoiner [][]byte
	rooms      map[string]relay.Invitation
	hold       time.Time // joiner frames from index holdFrom wait until hold
	holdFrom   int
	client     func(http.ResponseWriter, *http.Request)
}

func newHostileRelay(t *testing.T, pairTimeout time.Duration) *hostileRelay {
	t.Helper()
	inner, err := relay.New(relay.Config{PublicRooms: true, AdminToken: "hostile-relay-admin-token-0123456789abcdef", PairTimeout: pairTimeout})
	if err != nil {
		t.Fatal(err)
	}
	innerEdge := httptest.NewServer(inner)
	h := &hostileRelay{inner: inner, innerURL: innerEdge.URL, rooms: map[string]relay.Invitation{}}
	edge := httptest.NewServer(h)
	h.URL = edge.URL
	t.Cleanup(func() {
		edge.Close()
		_ = inner.Close()
		innerEdge.Close()
	})
	return h
}

func (h *hostileRelay) record(data []byte, joiner bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, bytes.Clone(data))
	if joiner {
		h.fromJoiner = append(h.fromJoiner, bytes.Clone(data))
	}
}

func (h *hostileRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/rooms":
		body, _ := io.ReadAll(r.Body)
		h.record(body, false)
		forward := httptest.NewRequest(http.MethodPost, "/v1/rooms", bytes.NewReader(body))
		forward.Header = r.Header.Clone()
		reply := httptest.NewRecorder()
		h.inner.ServeHTTP(reply, forward)
		var room relay.Invitation
		if json.Unmarshal(reply.Body.Bytes(), &room) == nil && room.ID != "" {
			h.mu.Lock()
			h.rooms[room.ID] = room
			h.mu.Unlock()
		}
		for key, values := range reply.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(reply.Code)
		_, _ = w.Write(reply.Body.Bytes())
	case strings.HasSuffix(r.URL.Path, "/client"):
		h.mu.Lock()
		client := h.client
		h.mu.Unlock()
		if client != nil {
			client(w, r)
			return
		}
		h.carry(w, r)
	default:
		h.record([]byte(r.Header.Get("Authorization")), false)
		h.inner.ServeHTTP(w, r)
	}
}

// dial takes the room's client seat on the real relay behind this one.
func (h *hostileRelay) dial(path, authorization string) (*websocket.Conn, int, error) {
	ws, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.innerURL, "http")+path, http.Header{"Authorization": {authorization}})
	status := http.StatusBadGateway
	if res != nil {
		status = res.StatusCode
		res.Body.Close()
	}
	return ws, status, err
}

func (h *hostileRelay) carry(w http.ResponseWriter, r *http.Request) {
	upstream, status, err := h.dial(r.URL.Path, r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "connection unavailable", status)
		return
	}
	joiner, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		upstream.Close()
		return
	}
	go h.pump(upstream, joiner, false)
	h.pump(joiner, upstream, true)
}

func (h *hostileRelay) pump(from, to *websocket.Conn, joiner bool) {
	defer from.Close()
	defer to.Close()
	for frame := 0; ; frame++ {
		kind, data, err := from.ReadMessage()
		if err != nil {
			return
		}
		h.record(data, joiner)
		h.mu.Lock()
		hold, holdFrom := h.hold, h.holdFrom
		h.mu.Unlock()
		if joiner && frame >= holdFrom {
			time.Sleep(time.Until(hold))
		}
		if to.WriteMessage(kind, data) != nil {
			return
		}
	}
}

func (h *hostileRelay) room(t *testing.T, id string) relay.Invitation {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[id]
	if !ok {
		t.Fatal("relay did not see the room")
	}
	return room
}

type deviceFixture struct {
	t      *testing.T
	relay  *hostileRelay
	host   *remotelink.Identity
	joiner *remotelink.Identity

	mu    sync.Mutex
	asked []DeviceRequest
	saved []DeviceRequest
}

// newDeviceFixture lowers Argon2id for protocol tests; the designed
// parameters are covered by TestDeviceCodeKeysKnownAnswer and the benchmark.
func newDeviceFixture(t *testing.T, pairTimeout time.Duration) *deviceFixture {
	t.Helper()
	saved := deviceCodeKDF
	deviceCodeKDF.time, deviceCodeKDF.memory = 1, 64
	t.Cleanup(func() { deviceCodeKDF = saved })
	host, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	joiner, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return &deviceFixture{t: t, relay: newHostileRelay(t, pairTimeout), host: host, joiner: joiner}
}

func freshLookupForTest() string {
	var token [32]byte
	_, _ = rand.Read(token[:])
	return base64.RawURLEncoding.EncodeToString(token[:])
}

// serve shows a new code and runs the host until one peer is done. decide
// plays the person at the host.
func (f *deviceFixture) serve(ctx context.Context, decide func(context.Context, DeviceRequest) bool) (*DeviceInvite, <-chan error) {
	f.t.Helper()
	inv, err := NewDeviceInvite(ctx, f.relay.URL, "", f.host)
	if err != nil {
		f.t.Fatal(err)
	}
	code := inv.Code
	done := make(chan error, 1)
	go func() {
		done <- AcceptDevice(ctx, inv, f.host, func(ctx context.Context, request DeviceRequest) bool {
			f.mu.Lock()
			f.asked = append(f.asked, request)
			f.mu.Unlock()
			return decide(ctx, request)
		}, func(request DeviceRequest) (PairedHost, error) {
			f.mu.Lock()
			f.saved = append(f.saved, request)
			f.mu.Unlock()
			return PairedHost{Version: 1, Relay: f.relay.URL, HostPin: f.host.Pin, Lookup: freshLookupForTest()}, nil
		})
	}()
	// The room is found only once the host's socket has joined it.
	lookup, _, err := deviceCodeKeys(ctx, code, f.relay.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	for start := time.Now(); ; time.Sleep(5 * time.Millisecond) {
		if _, err := LookupRoom(ctx, f.relay.URL, lookup); err == nil {
			break
		} else if time.Since(start) > 3*time.Second {
			f.t.Fatal("host never joined its room:", err)
		}
	}
	return inv, done
}

func (f *deviceFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked), len(f.saved)
}

func (f *deviceFixture) redeem(ctx context.Context, code string, check func(string)) (PairedHost, error) {
	return RedeemDeviceCode(ctx, code, f.relay.URL, f.joiner, DeviceHello{Name: "MacBook Air", Platform: "darwin"}, check)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

func TestDeviceCodeJoinsThroughOneRoomOnce(t *testing.T) {
	f := newDeviceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shown := make(chan string, 1)
	inv, done := f.serve(ctx, func(ctx context.Context, request DeviceRequest) bool {
		select {
		case check := <-shown:
			return check == request.Check
		case <-ctx.Done():
			return false
		}
	})
	record, err := f.redeem(ctx, FormatDeviceCode(inv.Code), func(check string) { shown <- check })
	if err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if record.HostPin != f.host.Pin || record.Relay != f.relay.URL || record.Version != 1 {
		t.Fatalf("joiner saved %+v", record)
	}
	f.mu.Lock()
	saved := f.saved
	f.mu.Unlock()
	if len(saved) != 1 || !saved[0].ShortCode || saved[0].Pin != f.joiner.Pin || saved[0].Name != "MacBook Air" || saved[0].Platform != "darwin" || len(saved[0].Check) != 4 {
		t.Fatalf("host saved %+v", saved)
	}
	// One use: the code, a retyped code and the QR of the same room are dead.
	if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
		t.Fatalf("second use of a code: %v", err)
	}
	phone, _ := remotelink.NewIdentity()
	if _, err := RedeemPair(ctx, inv.Raw, f.relay.URL, phone.Pin); err == nil {
		t.Fatal("QR of a used room still pairs")
	}
	// What the relay read: hashes, bearers, room tokens and TLS records. None
	// of the code, the names, the keys or the saved lookup token.
	f.relay.mu.Lock()
	seen := bytes.Join(f.relay.seen, nil)
	f.relay.mu.Unlock()
	for _, secret := range [][]byte{
		[]byte(inv.Code), []byte(FormatDeviceCode(inv.Code)), []byte("MacBook Air"), []byte(f.joiner.Pin), []byte(f.host.Pin), []byte(record.Lookup),
		f.joiner.Certificate.Leaf.RawSubjectPublicKeyInfo[12:], f.host.Certificate.Leaf.RawSubjectPublicKeyInfo[12:],
	} {
		if bytes.Contains(seen, secret) {
			t.Fatalf("relay read %q", secret)
		}
	}
}

func TestDeviceInviteKeepsTodaysQRAndLongCode(t *testing.T) {
	f := newDeviceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, manual := range []bool{false, true} {
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { t.Error("QR pairing asked for approval"); return false })
		// Today's CAGE1 shape, so today's phone apps scan it.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(inv.Raw, "CAGE1:")), &fields); err != nil || !strings.HasPrefix(inv.Raw, "CAGE1:") {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if fmt.Sprint(keys) != "[e h i r s t]" {
			t.Fatalf("QR fields changed: %v", keys)
		}
		code := inv.Raw
		if manual {
			code = inv.Manual
		}
		phone, _ := remotelink.NewIdentity()
		got, err := RedeemPair(ctx, code, f.relay.URL, phone.Pin)
		if err != nil {
			t.Fatal(err)
		}
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
		var saved PairedHost
		if json.Unmarshal([]byte(got), &saved) != nil || saved.HostPin != f.host.Pin {
			t.Fatalf("phone saved %s", got)
		}
		f.mu.Lock()
		last := f.saved[len(f.saved)-1]
		f.mu.Unlock()
		if last != (DeviceRequest{Pin: phone.Pin}) {
			t.Fatalf("host saved %+v", last)
		}
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("short code after the QR won the seat: %v", err)
		}
	}
	if asked, _ := f.counts(); asked != 0 {
		t.Fatal("QR pairing asked for approval")
	}
}

func TestDeviceCodeApprovalOutcomes(t *testing.T) {
	saved := DeviceApprovalTimeout
	DeviceApprovalTimeout = 300 * time.Millisecond
	defer func() { DeviceApprovalTimeout = saved }()
	for _, test := range []struct {
		name   string
		decide func(context.Context, DeviceRequest) bool
		want   error
	}{
		{"rejected", func(context.Context, DeviceRequest) bool { return false }, ErrDeviceRejected},
		{"timeout", func(ctx context.Context, _ DeviceRequest) bool { <-ctx.Done(); return true }, ErrDeviceApprovalTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDeviceFixture(t, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			inv, done := f.serve(ctx, test.decide)
			if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, test.want) {
				t.Fatalf("joiner: %v", err)
			}
			if err := receive(t, done); !errors.Is(err, test.want) {
				t.Fatalf("host: %v", err)
			}
			if asked, saved := f.counts(); asked != 1 || saved != 0 {
				t.Fatalf("asked %d saved %d", asked, saved)
			}
		})
	}
}

func TestDeviceCodeJoinerLeavesDuringApproval(t *testing.T) {
	f := newDeviceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	asked, ended := make(chan struct{}), make(chan struct{})
	inv, done := f.serve(ctx, func(ctx context.Context, _ DeviceRequest) bool {
		close(asked)
		<-ctx.Done()
		close(ended)
		return true
	})
	leave, stop := context.WithCancel(ctx)
	joined := make(chan error, 1)
	go func() { _, err := f.redeem(leave, inv.Code, nil); joined <- err }()
	receive(t, asked)
	stop()
	if err := receive(t, joined); !errors.Is(err, context.Canceled) {
		t.Fatalf("joiner: %v", err)
	}
	receive(t, ended)
	if err := receive(t, done); !errors.Is(err, ErrDeviceJoinerLeft) {
		t.Fatalf("host: %v", err)
	}
	if _, saved := f.counts(); saved != 0 {
		t.Fatal("host enrolled a joiner that had left")
	}
}

func TestDeviceCodeSecondJoinerExpiryAndCancel(t *testing.T) {
	t.Run("second joiner", func(t *testing.T) {
		f := newDeviceFixture(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		asked, release := make(chan struct{}), make(chan struct{})
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { close(asked); <-release; return true })
		first := make(chan error, 1)
		go func() { _, err := f.redeem(ctx, inv.Code, nil); first <- err }()
		receive(t, asked)
		other, _ := remotelink.NewIdentity()
		if _, err := RedeemDeviceCode(ctx, inv.Code, f.relay.URL, other, DeviceHello{"Second", "linux"}, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("second joiner while the first waits: %v", err)
		}
		close(release)
		if err := receive(t, first); err != nil {
			t.Fatal(err)
		}
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		f := newDeviceFixture(t, 2*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
		time.Sleep(time.Until(inv.Expires) + 100*time.Millisecond)
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("expired code: %v", err)
		}
		if err := receive(t, done); !errors.Is(err, ErrDeviceCodeExpired) {
			t.Fatalf("host after expiry: %v", err)
		}
	})
	t.Run("dialog closed", func(t *testing.T) {
		f := newDeviceFixture(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		show, close := context.WithCancel(ctx)
		inv, done := f.serve(show, func(context.Context, DeviceRequest) bool { return true })
		close()
		if err := receive(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("closed dialog: %v", err)
		}
		// The relay drops the room when it reads the host's close.
		lookup, _, err := deviceCodeKeys(ctx, inv.Code, f.relay.URL)
		if err != nil {
			t.Fatal(err)
		}
		for start := time.Now(); ; time.Sleep(5 * time.Millisecond) {
			if _, err := LookupRoom(ctx, f.relay.URL, lookup); errors.Is(err, ErrNoInvitation) {
				break
			} else if time.Since(start) > 2*time.Second {
				t.Fatal("room outlived the closed dialog:", err)
			}
		}
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("code after the dialog closed: %v", err)
		}
	})
}

func TestDeviceCodeCollisionPicksNewCode(t *testing.T) {
	f := newDeviceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lookup, _, err := deviceCodeKeys(ctx, "11111111", f.relay.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateDiscoverableRoom(ctx, f.relay.URL, "", lookup); err != nil {
		t.Fatal(err)
	}
	defer func(saved func() (string, error)) { newDeviceCode = saved }(newDeviceCode)
	codes := []string{"11111111", "22222222"}
	newDeviceCode = func() (string, error) { code := codes[0]; codes = codes[1:]; return code, nil }
	inv, err := NewDeviceInvite(ctx, f.relay.URL, "", f.host)
	if err != nil || inv.Code != "22222222" {
		t.Fatalf("collision: %+v %v", inv, err)
	}
	newDeviceCode = func() (string, error) { return "11111111", nil }
	if _, err := NewDeviceInvite(ctx, f.relay.URL, "", f.host); err == nil {
		t.Fatal("three collisions accepted")
	}
}

// relayInTheMiddle replaces the carrying of the joiner's connection: the relay
// terminates the joiner's TLS as a host and opens its own to the real host as
// a joiner, proving key to both. With forge it then approves the joiner itself.
func relayInTheMiddle(h *hostileRelay, key []byte, forge bool, hostAnswer chan<- error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client = func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		self, _ := remotelink.NewIdentity()
		temp, _ := remotelink.NewIdentity()
		upstream, status, err := h.dial(r.URL.Path, r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "connection unavailable", status)
			hostAnswer <- err
			return
		}
		defer upstream.Close()
		joinerWS, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			hostAnswer <- err
			return
		}
		defer joinerWS.Close()
		name := "MacBook Air"
		var legA *remotelink.HostPairing
		if connA, err := remotelink.AcceptAfterJoin(ctx, joinerWS, self, temp.Pin, true); err == nil {
			if legA, err = remotelink.ConfirmJoiner(ctx, connA, self, key); err == nil {
				name = legA.Request.Name
			}
		}
		connB, err := remotelink.PairClient(ctx, upstream, self)
		if err == nil {
			var legB *remotelink.JoinerPairing
			if legB, err = remotelink.JoinHost(ctx, connB, self, key, name, "darwin"); err == nil {
				var record []byte
				if record, err = legB.Await(ctx); err == nil {
					err = fmt.Errorf("relay obtained the host's record %s", record)
				}
			}
		}
		hostAnswer <- err
		if legA != nil && forge {
			payload, _ := json.Marshal(PairedHost{Version: 1, Relay: h.URL, HostPin: self.Pin, Lookup: freshLookupForTest()})
			_ = legA.Approve(ctx, payload)
		}
	}
}

func TestDeviceCodeHostileRelay(t *testing.T) {
	t.Run("swaps both sessions without the code", func(t *testing.T) {
		f := newDeviceFixture(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
		guess := make([]byte, 32)
		_, _ = rand.Read(guess)
		answer := make(chan error, 1)
		relayInTheMiddle(f.relay, guess, true, answer)
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeFailed) {
			t.Fatalf("joiner through a relay without the code: %v", err)
		}
		if err := receive(t, answer); !errors.Is(err, remotelink.ErrPairFailed) {
			t.Fatalf("relay toward the host: %v", err)
		}
		if err := receive(t, done); !errors.Is(err, ErrDeviceCodeFailed) {
			t.Fatalf("host: %v", err)
		}
		if asked, saved := f.counts(); asked != 0 || saved != 0 {
			t.Fatal("host asked or saved for a relay without the code")
		}
		f.relay.mu.Lock()
		f.relay.client = nil
		f.relay.mu.Unlock()
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("code after a failed proof: %v", err)
		}
	})
	t.Run("swaps both sessions with the code", func(t *testing.T) {
		for attempt := 1; ; attempt++ {
			hostCheck, joinerCheck := relayWithTheCodeForTest(t)
			if hostCheck != joinerCheck {
				t.Logf("host showed %s, joiner showed %s", hostCheck, joinerCheck)
				return
			}
			// Equal numbers by chance: probability 1/10⁴ per code.
			if attempt == 3 {
				t.Fatal("check numbers matched three times")
			}
		}
	})
	t.Run("replays a recorded joiner", func(t *testing.T) {
		f := newDeviceFixture(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
		if _, err := f.redeem(ctx, inv.Code, nil); err != nil {
			t.Fatal(err)
		}
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
		f.relay.mu.Lock()
		recorded := slices.Clone(f.relay.fromJoiner)
		f.relay.mu.Unlock()
		old := f.relay.room(t, inv.Pair.Room)
		if _, _, err := f.relay.dial("/v1/rooms/"+old.ID+"/client", "Bearer "+old.ClientToken); err == nil {
			t.Fatal("relay re-entered a used room")
		}
		next, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
		room := f.relay.room(t, next.Pair.Room)
		replay, _, err := f.relay.dial("/v1/rooms/"+room.ID+"/client", "Bearer "+room.ClientToken)
		if err != nil {
			t.Fatal(err)
		}
		defer replay.Close()
		for _, frame := range recorded {
			if replay.WriteMessage(websocket.BinaryMessage, frame) != nil {
				break
			}
		}
		hostErr := receive(t, done)
		if hostErr == nil || !strings.Contains(hostErr.Error(), "TLS handshake") {
			t.Fatalf("host and a replayed joiner: %v", hostErr)
		}
		t.Logf("host: %v", hostErr)
		if asked, saved := f.counts(); asked != 1 || saved != 1 {
			t.Fatalf("replay reached approval: asked %d saved %d", asked, saved)
		}
	})
	t.Run("grinds codes and proofs", func(t *testing.T) {
		f := newDeviceFixture(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
		// Wrong codes: anonymous misses, never seen by the host.
		misses := 0
		for i := range 50 {
			code := fmt.Sprintf("%08d", (i*7919+1)%100_000_000)
			if code == inv.Code {
				continue
			}
			lookup, _, err := deviceCodeKeys(ctx, code, f.relay.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LookupRoom(ctx, f.relay.URL, lookup); !errors.Is(err, ErrNoInvitation) {
				t.Fatalf("wrong code %s: %v", code, err)
			}
			misses++
		}
		if asked, _ := f.counts(); asked != 0 {
			t.Fatal("host saw lookups")
		}
		// The relay holds the room's client token, so it can skip the lookup and
		// prove a guessed key directly. That is one try per code shown.
		room := f.relay.room(t, inv.Pair.Room)
		for try := 1; try <= 2; try++ {
			ws, _, err := f.relay.dial("/v1/rooms/"+room.ID+"/client", "Bearer "+room.ClientToken)
			if try == 2 {
				if err == nil {
					t.Fatal("relay got a second try at the same code")
				}
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			self, _ := remotelink.NewIdentity()
			conn, err := remotelink.PairClient(ctx, ws, self)
			if err != nil {
				t.Fatal(err)
			}
			guess := make([]byte, 32)
			_, _ = rand.Read(guess)
			if _, err := remotelink.JoinHost(ctx, conn, self, guess, "Guess", "linux"); !errors.Is(err, remotelink.ErrPairFailed) {
				t.Fatalf("guessed proof: %v", err)
			}
			conn.Close()
		}
		if err := receive(t, done); !errors.Is(err, ErrDeviceCodeFailed) {
			t.Fatalf("host after a guessed proof: %v", err)
		}
		if _, err := f.redeem(ctx, inv.Code, nil); !errors.Is(err, ErrDeviceCodeUnknown) {
			t.Fatalf("right code after the relay burned it: %v", err)
		}
		t.Logf("%d wrong-code lookups: uncounted 404s; one guessed proof killed the code", misses)
	})
	// Once both seats are taken the relay stops the room's timer, so only the
	// host's own deadline ends the code. Frame 0 is the ClientHello, frame 1
	// the client's last handshake flight, frame 2 the proof of the code.
	for _, from := range []int{0, 2} {
		t.Run(fmt.Sprintf("stalls joiner frames from %d past the code's life", from), func(t *testing.T) {
			f := newDeviceFixture(t, 2*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { return true })
			f.relay.mu.Lock()
			f.relay.hold, f.relay.holdFrom = inv.Expires.Add(300*time.Millisecond), from
			f.relay.mu.Unlock()
			if _, err := f.redeem(ctx, inv.Code, nil); err == nil {
				t.Fatal("joiner paired through a stalled room")
			}
			hostErr := receive(t, done)
			if !errors.Is(hostErr, context.DeadlineExceeded) {
				t.Fatalf("host after the code expired: %v", hostErr)
			}
			if asked, saved := f.counts(); asked != 0 || saved != 0 {
				t.Fatal("stalled joiner reached approval")
			}
			t.Logf("host: %v", hostErr)
		})
	}
}

// relayWithTheCodeForTest is the relay that found the code (about 10⁵ cores
// for 2 minutes at the designed Argon2id cost). It passes both proofs; the
// person compares the numbers and refuses on the host.
func relayWithTheCodeForTest(t *testing.T) (string, string) {
	f := newDeviceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shown := make(chan string, 1)
	var hostCheck string
	inv, done := f.serve(ctx, func(ctx context.Context, request DeviceRequest) bool {
		hostCheck = request.Check
		select {
		case check := <-shown:
			return check == request.Check
		case <-ctx.Done():
			return false
		}
	})
	_, key, err := deviceCodeKeys(ctx, inv.Code, f.relay.URL)
	if err != nil {
		t.Fatal(err)
	}
	answer := make(chan error, 1)
	relayInTheMiddle(f.relay, key, true, answer)
	var joinerCheck string
	record, err := f.redeem(ctx, inv.Code, func(check string) { joinerCheck = check; shown <- check })
	hostErr := receive(t, done)
	relayErr := receive(t, answer)
	if hostCheck == joinerCheck {
		return hostCheck, joinerCheck
	}
	if !errors.Is(hostErr, ErrDeviceRejected) || !errors.Is(relayErr, remotelink.ErrPairRejected) {
		t.Fatalf("host %v, relay %v", hostErr, relayErr)
	}
	if asked, saved := f.counts(); asked != 1 || saved != 0 {
		t.Fatalf("asked %d saved %d", asked, saved)
	}
	// Residual: the joiner trusts whoever proved the code. Here that is the
	// relay's own key, never the host's, so it has no link to the host.
	if err != nil || record.HostPin == f.host.Pin {
		t.Fatalf("joiner after a forged approval: %+v %v", record, err)
	}
	t.Logf("joiner accepted the relay's forged approval and pinned the relay's key, not the host's")
	return hostCheck, joinerCheck
}

func TestParseDeviceCode(t *testing.T) {
	for input, want := range map[string]string{
		"48219037": "48219037", "4821 9037": "48219037", " 4821-9037 ": "48219037", "４８２１　９０３７": "48219037",
		"4821903": "", "482190370": "", "4821 903a": "", "": "", "٤٨٢١٩٠٣٧": "",
	} {
		got, err := ParseDeviceCode(input)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("ParseDeviceCode(%q) = %q, %v", input, got, err)
		}
	}
	for range 100 {
		code, err := NewDeviceCode()
		if err != nil || len(code) != DeviceCodeDigits || strings.Trim(code, "0123456789") != "" {
			t.Fatalf("NewDeviceCode() = %q, %v", code, err)
		}
	}
	if FormatDeviceCode("48219037") != "4821 9037" {
		t.Fatal("format")
	}
}

// TestDeviceCodeKeysKnownAnswer pins the designed parameters (Argon2id t=3,
// 64 MiB, p=1; HKDF-SHA256 "lookup" and "confirm"), so host and joiner builds
// cannot drift apart silently.
func TestDeviceCodeKeysKnownAnswer(t *testing.T) {
	lookup, confirm, err := deviceCodeKeys(context.Background(), "48219037", OfficialRelay)
	if err != nil {
		t.Fatal(err)
	}
	if lookup != knownLookup || base64.RawURLEncoding.EncodeToString(confirm) != knownConfirm {
		t.Fatalf("lookup %s confirm %s", lookup, base64.RawURLEncoding.EncodeToString(confirm))
	}
	other, _, err := deviceCodeKeys(context.Background(), "48219037", "https://relay.example")
	if err != nil || other == lookup {
		t.Fatal("relay origin is not part of the stretch")
	}
}

const (
	knownLookup  = "sA20H9X2RbQLJ98CICfQhrBw7LRJBENYKtAu3mH8s7A"
	knownConfirm = "kn2qXXpQ_3PtdTYIBEwRHf66hJEuCI6GVvW8KleF3fU"
)

func BenchmarkDeviceCodeKeys(b *testing.B) {
	ctx := context.Background()
	for i := 0; b.Loop(); i++ {
		if _, _, err := deviceCodeKeys(ctx, fmt.Sprintf("%08d", i%100_000_000), OfficialRelay); err != nil {
			b.Fatal(err)
		}
	}
}
