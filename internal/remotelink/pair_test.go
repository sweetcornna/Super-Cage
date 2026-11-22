package remotelink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func pairKeyForTest(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// pairSessionForTest returns the joiner's and the host's ends of one
// cage-pair/1 session through the ALPN dispatcher.
func pairSessionForTest(t *testing.T, ctx context.Context, joiner, host *Identity) (*tls.Conn, *tls.Conn) {
	t.Helper()
	legacy := identityForTest(t)
	joinerWS, hostWS := socketsForTest(t)
	result := make(chan handshakeResult, 1)
	go func() {
		connection, err := AcceptAfterJoin(ctx, hostWS, host, legacy.Pin, true)
		result <- handshakeResult{connection, err}
	}()
	joined, err := PairClient(ctx, joinerWS, joiner)
	if err != nil {
		t.Fatal(err)
	}
	accepted := <-result
	if accepted.err != nil {
		t.Fatal(accepted.err)
	}
	for _, connection := range []*tls.Conn{joined, accepted.connection} {
		if state := connection.ConnectionState(); state.NegotiatedProtocol != PairProtocol || state.Version != tls.VersionTLS13 {
			t.Fatalf("pairing session negotiated %q/%x", state.NegotiatedProtocol, state.Version)
		}
	}
	return joined, accepted.connection
}

func TestPairConfirmsCodeAndAgreesOnCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, host, key := identityForTest(t), identityForTest(t), pairKeyForTest(t)
	joined, accepted := pairSessionForTest(t, ctx, joiner, host)
	record := []byte(`{"version":1,"relay":"https://relay.example","host_pin":"x","lookup_token":"y<&>"}`)
	pending := make(chan *HostPairing, 1)
	hostErr := make(chan error, 1)
	go func() {
		p, err := ConfirmJoiner(ctx, accepted, host, key)
		if err != nil {
			hostErr <- err
			return
		}
		pending <- p
		if err := p.Approve(ctx, record); err != nil {
			hostErr <- err
			return
		}
		hostErr <- p.AwaitConfirm(ctx)
	}()
	j, err := JoinHost(ctx, joined, joiner, key, "MacBook Air", "darwin")
	if err != nil {
		t.Fatal(err)
	}
	p := <-pending
	if p.Request != (JoinRequest{Name: "MacBook Air", Platform: "darwin", Pin: joiner.Pin, Check: j.Check}) {
		t.Fatalf("host saw %+v, joiner check %s", p.Request, j.Check)
	}
	if len(j.Check) != 4 || strings.Trim(j.Check, "0123456789") != "" || j.HostPin != host.Pin {
		t.Fatalf("joiner state: check %q host %q", j.Check, j.HostPin)
	}
	got, err := j.Await(ctx)
	if err != nil || !bytes.Equal(got, record) {
		t.Fatalf("record %q, %v", got, err)
	}
	// The host saves only after this side's person confirmed the number.
	select {
	case err := <-hostErr:
		t.Fatalf("host finished before the joiner confirmed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := j.Confirm(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-hostErr; err != nil {
		t.Fatal(err)
	}
}

// A peer without the code (a relay that terminates TLS) gets no valid host
// proof past the joiner: the joiner says nothing, not even its name.
func TestPairJoinerSaysNothingWithoutTheHostsProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, relay := identityForTest(t), identityForTest(t)
	joined, accepted := pairSessionForTest(t, ctx, joiner, relay)
	binding, err := bindPair(accepted, relay, true)
	must(t, err)
	must(t, writePairMessage(accepted, pairMessage{MAC: encode32(binding.hostProof(pairKeyForTest(t)))}))
	if _, err := JoinHost(ctx, joined, joiner, pairKeyForTest(t), "MacBook Air", "darwin"); !errors.Is(err, ErrPairFailed) {
		t.Fatalf("joiner after a wrong host proof: %v", err)
	}
	must(t, accepted.SetReadDeadline(time.Now().Add(2*time.Second)))
	if m, err := readPairMessage(accepted); err == nil {
		t.Fatalf("the joiner spoke after a wrong host proof: %+v", m)
	}
}

// After 允许 the host saves only on the joiner's confirmation of this session.
func TestPairHostWaitsForTheJoinersConfirmation(t *testing.T) {
	joiner, host, key := identityForTest(t), identityForTest(t), pairKeyForTest(t)
	for _, test := range []struct {
		name string
		end  func(context.Context, *JoinerPairing) error
		want error
	}{
		{"numbers differ", func(ctx context.Context, j *JoinerPairing) error { return j.Refuse(ctx, ErrPairRejected) }, ErrPairRejected},
		{"no click", func(ctx context.Context, j *JoinerPairing) error { return nil }, context.DeadlineExceeded},
		{"forged confirmation", func(ctx context.Context, j *JoinerPairing) error {
			j.key = make([]byte, 32)
			return j.Confirm(ctx)
		}, ErrPairFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			joined, accepted := pairSessionForTest(t, ctx, joiner, host)
			hostErr := make(chan error, 1)
			go func() {
				p, err := ConfirmJoiner(ctx, accepted, host, key)
				if err == nil {
					err = p.Approve(ctx, []byte(`{"version":1}`))
				}
				if err == nil {
					wait, stop := context.WithTimeout(ctx, 300*time.Millisecond)
					defer stop()
					err = p.AwaitConfirm(wait)
				}
				hostErr <- err
			}()
			j, err := JoinHost(ctx, joined, joiner, key, "Pixel", "android")
			must(t, err)
			_, err = j.Await(ctx)
			must(t, err)
			must(t, test.end(ctx, j))
			if err := <-hostErr; !errors.Is(err, test.want) {
				t.Fatalf("host: %v, want %v", err, test.want)
			}
		})
	}
}

// A legacy client is today's phone: it offers only cage-remote/1 and is pinned
// to the QR's temporary key by the unchanged peerVerifier.
func TestPairDispatcherKeepsLegacyClients(t *testing.T) {
	host, temp, other := identityForTest(t), identityForTest(t), identityForTest(t)
	accept := func(t *testing.T, ctx context.Context, ws *websocket.Conn, pairing bool) chan handshakeResult {
		result := make(chan handshakeResult, 1)
		go func() {
			connection, err := AcceptAfterJoin(ctx, ws, host, temp.Pin, pairing)
			result <- handshakeResult{connection, err}
		}()
		return result
	}
	custom := func(ctx context.Context, ws *websocket.Conn, identity *Identity, protos []string) (*tls.Conn, error) {
		config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: protos,
			Certificates: []tls.Certificate{identity.Certificate}, InsecureSkipVerify: true}
		connection := tls.Client(newWebSocketConn(ws), config)
		return connection, connection.HandshakeContext(ctx)
	}
	for _, pairing := range []bool{true, false} {
		t.Run("legacy client, pairing offered "+map[bool]string{true: "yes", false: "no"}[pairing], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			clientWS, serverWS := socketsForTest(t)
			result := accept(t, ctx, serverWS, pairing)
			client, err := Client(ctx, clientWS, temp, host.Pin)
			if err != nil {
				t.Fatal(err)
			}
			server := <-result
			if server.err != nil || server.connection.ConnectionState().NegotiatedProtocol != Protocol {
				t.Fatalf("legacy pairing through the dispatcher: %v", server.err)
			}
			go func() { _, _ = client.Write([]byte("sha256:phone")) }()
			buffer := make([]byte, 12)
			if _, err := io.ReadFull(server.connection, buffer); err != nil || string(buffer) != "sha256:phone" {
				t.Fatalf("legacy stream: %q %v", buffer, err)
			}
		})
	}
	t.Run("legacy client with a different key", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		clientWS, serverWS := socketsForTest(t)
		result := accept(t, ctx, serverWS, true)
		_, _ = Client(ctx, clientWS, other, host.Pin)
		if server := <-result; server.err == nil || !strings.Contains(server.err.Error(), "paired public key") {
			t.Fatalf("unpinned legacy client accepted: %v", server.err)
		}
	})
	t.Run("pairing client while no code is showing", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		clientWS, serverWS := socketsForTest(t)
		result := accept(t, ctx, serverWS, false)
		if _, err := PairClient(ctx, clientWS, other); err == nil {
			t.Fatal("pairing client accepted while pairing is off")
		}
		if server := <-result; server.err == nil {
			t.Fatal("host accepted a pairing client while pairing is off")
		}
	})
	t.Run("client without ALPN", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		clientWS, serverWS := socketsForTest(t)
		result := accept(t, ctx, serverWS, true)
		_, _ = custom(ctx, clientWS, temp, nil)
		if server := <-result; server.err == nil || !strings.Contains(server.err.Error(), "application protocol") {
			t.Fatalf("client without ALPN accepted: %v", server.err)
		}
	})
	t.Run("client offering both", func(t *testing.T) {
		for _, pairing := range []bool{true, false} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			clientWS, serverWS := socketsForTest(t)
			result := accept(t, ctx, serverWS, pairing)
			// The pinned temporary key decides only on the legacy branch.
			identity := other
			if !pairing {
				identity = temp
			}
			client, err := custom(ctx, clientWS, identity, []string{Protocol, PairProtocol})
			server := <-result
			want := map[bool]string{true: PairProtocol, false: Protocol}[pairing]
			if err != nil || server.err != nil || server.connection.ConnectionState().NegotiatedProtocol != want || client.ConnectionState().NegotiatedProtocol != want {
				t.Fatalf("pairing=%v: client %v server %v", pairing, err, server.err)
			}
			cancel()
		}
	})
}

func TestPairWrongKeyFailsBeforeAnyNonce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, host := identityForTest(t), identityForTest(t)
	joined, accepted := pairSessionForTest(t, ctx, joiner, host)
	hostErr := make(chan error, 1)
	go func() { _, err := ConfirmJoiner(ctx, accepted, host, pairKeyForTest(t)); hostErr <- err }()
	if _, err := JoinHost(ctx, joined, joiner, pairKeyForTest(t), "Guess", "linux"); !errors.Is(err, ErrPairFailed) {
		t.Fatalf("joiner with a wrong code: %v", err)
	}
	if err := <-hostErr; !errors.Is(err, ErrPairFailed) {
		t.Fatalf("host accepted a wrong code: %v", err)
	}
}

func TestPairRevealMustMatchCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, host, key := identityForTest(t), identityForTest(t), pairKeyForTest(t)
	joined, accepted := pairSessionForTest(t, ctx, joiner, host)
	hostErr := make(chan error, 1)
	go func() { _, err := ConfirmJoiner(ctx, accepted, host, key); hostErr <- err }()
	binding, err := bindPair(joined, joiner, false)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := readPairMessage(joined); err != nil {
		t.Fatal(err)
	} else if proof, err := m.proof(); err != nil || encode32(proof[:]) != encode32(binding.hostProof(key)) {
		t.Fatal("no valid host proof first:", m)
	}
	committed, revealed := make([]byte, 32), make([]byte, 32)
	revealed[0] = 1
	commit := pairCommit(committed)
	if err := writePairMessage(joined, pairMessage{Version: 1, MAC: encode32(binding.joinerMAC(key)), Commit: encode32(commit[:]), Name: "Swap", Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if m, err := readPairMessage(joined); err != nil {
		t.Fatal(err)
	} else if _, err := m.nonce(); err != nil {
		t.Fatal("host refused a valid proof:", m)
	}
	if err := writePairMessage(joined, pairMessage{Nonce: encode32(revealed)}); err != nil {
		t.Fatal(err)
	}
	if err := <-hostErr; !errors.Is(err, ErrPairFailed) {
		t.Fatalf("host accepted a reveal that does not match the commit: %v", err)
	}
}

func TestPairJoinerRequiresHostProofAndReportsRefusals(t *testing.T) {
	joiner, host, key := identityForTest(t), identityForTest(t), pairKeyForTest(t)
	for _, test := range []struct {
		name string
		end  func(context.Context, *HostPairing) error
		want error
	}{
		{"rejected", func(ctx context.Context, p *HostPairing) error { return p.Refuse(ctx, ErrPairRejected) }, ErrPairRejected},
		{"timeout", func(ctx context.Context, p *HostPairing) error { return p.Refuse(ctx, ErrPairTimedOut) }, ErrPairTimedOut},
		{"forged approval", func(ctx context.Context, p *HostPairing) error {
			// A host that never knew the code cannot make the approval MAC.
			p.key = make([]byte, 32)
			return p.Approve(ctx, []byte(`{"version":1}`))
		}, ErrPairFailed},
		{"silent host", func(ctx context.Context, p *HostPairing) error { return nil }, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			joined, accepted := pairSessionForTest(t, ctx, joiner, host)
			hostErr := make(chan error, 1)
			go func() {
				p, err := ConfirmJoiner(ctx, accepted, host, key)
				if err == nil {
					err = test.end(ctx, p)
				}
				hostErr <- err
			}()
			j, err := JoinHost(ctx, joined, joiner, key, "Pixel", "android")
			if err != nil {
				t.Fatal(err)
			}
			wait, stop := context.WithTimeout(ctx, 300*time.Millisecond)
			defer stop()
			if record, err := j.Await(wait); !errors.Is(err, test.want) || record != nil {
				t.Fatalf("joiner got %q, %v; want %v", record, err, test.want)
			}
			if err := <-hostErr; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestPairRelayInTheMiddleWithTheCode is the strongest relay: it found the
// code, terminates both TLS sessions, passes both MACs and grinds every value
// it controls. The check numbers still differ, because each value it controls
// is fixed before the value it would need to match is revealed.
func TestPairRelayInTheMiddleWithTheCode(t *testing.T) {
	for attempt := 1; ; attempt++ {
		hostCheck, joinerCheck := relayInTheMiddleForTest(t)
		if hostCheck != joinerCheck {
			t.Logf("host shows %s, joiner shows %s", hostCheck, joinerCheck)
			return
		}
		// Equal four-digit numbers happen by chance with probability 1/10⁴ per
		// attempt; each attempt costs the relay a new code in practice.
		if attempt == 3 {
			t.Fatal("check numbers matched three times in a row")
		}
	}
}

func relayInTheMiddleForTest(t *testing.T) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, host, relay, key := identityForTest(t), identityForTest(t), identityForTest(t), pairKeyForTest(t)
	// Session A: joiner to relay (the relay plays the host). Session B: relay
	// to host (the relay plays the joiner).
	joinedA, relayA := pairSessionForTest(t, ctx, joiner, relay)
	relayB, acceptedB := pairSessionForTest(t, ctx, relay, host)
	hostSide := make(chan *HostPairing, 1)
	go func() {
		p, err := ConfirmJoiner(ctx, acceptedB, host, key)
		if err != nil {
			t.Error("host refused the relay's valid proof:", err)
		}
		hostSide <- p
	}()
	joinerSide := make(chan *JoinerPairing, 1)
	go func() {
		j, err := JoinHost(ctx, joinedA, joiner, key, "MacBook Air", "darwin")
		if err != nil {
			t.Error("joiner refused the relay's valid proof:", err)
		}
		joinerSide <- j
	}()
	bindingA, err := bindPair(relayA, relay, true)
	if err != nil {
		t.Fatal(err)
	}
	bindingB, err := bindPair(relayB, relay, false)
	if err != nil {
		t.Fatal(err)
	}
	// Session B first: the host proves the code to the relay (which can check
	// it), the relay commits to its joiner nonce and learns the host's number X.
	if m, err := readPairMessage(relayB); err != nil {
		t.Fatal(err)
	} else if proof, err := m.proof(); err != nil || encode32(proof[:]) != encode32(bindingB.hostProof(key)) {
		t.Fatal("host proof missing on session B")
	}
	relayNonce := make([]byte, 32)
	commit := pairCommit(relayNonce)
	must(t, writePairMessage(relayB, pairMessage{Version: 1, MAC: encode32(bindingB.joinerMAC(key)), Commit: encode32(commit[:]), Name: "MacBook Air", Platform: "darwin"}))
	hostNonce := readNonceForTest(t, relayB)
	must(t, writePairMessage(relayB, pairMessage{Nonce: encode32(relayNonce)}))
	hostDigest := bindingB.checkDigest(hostNonce[:], relayNonce)
	p := <-hostSide
	if p == nil || p.Request != (JoinRequest{Name: "MacBook Air", Platform: "darwin", Pin: relay.Pin, Check: checkNumber(hostDigest)}) {
		t.Fatal("host state does not match the relay's view")
	}
	// Session A: the relay proves the code to the joiner, which then commits to
	// its nonce. The relay grinds its own nonce toward X under its best guess
	// of the hidden joiner nonce.
	must(t, writePairMessage(relayA, pairMessage{MAC: encode32(bindingA.hostProof(key))}))
	m, err := readPairMessage(relayA)
	must(t, err)
	_, joinerCommit, err := m.hello()
	must(t, err)
	guess := make([]byte, 32)
	ground, tries, start := grindForTest(bindingA, guess, p.Request.Check)
	t.Logf("relay ground its nonce toward %s in %d tries (%v) against a guessed joiner nonce", p.Request.Check, tries, time.Since(start))
	must(t, writePairMessage(relayA, pairMessage{Nonce: encode32(ground)}))
	m, err = readPairMessage(relayA)
	must(t, err)
	joinerNonce, err := m.nonce()
	must(t, err)
	if c := pairCommit(joinerNonce[:]); c != joinerCommit {
		t.Fatal("joiner reveal does not match its commit")
	}
	joinerDigest := bindingA.checkDigest(ground, joinerNonce[:])
	j := <-joinerSide
	if j == nil || j.Check != checkNumber(joinerDigest) || j.HostPin != relay.Pin {
		t.Fatal("joiner state does not match the relay's view")
	}
	if joinerDigest == hostDigest {
		t.Fatal("full check digests matched across two sessions")
	}
	// Counterfactual: had the joiner revealed its nonce before the relay chose
	// one, the same grinding would have matched the host's number.
	_, tries, start = grindForTest(bindingA, joinerNonce[:], p.Request.Check)
	t.Logf("with the joiner nonce known first, the relay would match %s after %d tries (%v)", p.Request.Check, tries, time.Since(start))
	// The person sees two different numbers and refuses on the host. The relay
	// gets a refusal, never the host's record.
	must(t, p.Refuse(ctx, ErrPairRejected))
	m, err = readPairMessage(relayB)
	if err != nil || !errors.Is(m.refusal(), ErrPairRejected) {
		t.Fatalf("relay received %+v, %v from the host", m, err)
	}
	// The relay forges an approval toward the joiner (it has the code). The
	// joiner's person sees its own number differ from the host's and refuses:
	// nothing is confirmed, so nothing is saved on either side.
	forged := []byte(`{"version":1,"host_pin":"relay"}`)
	must(t, writePairMessage(relayA, pairMessage{MAC: encode32(bindingA.hostMAC(key, forged)), Record: string(forged)}))
	if record, err := j.Await(ctx); err != nil || !bytes.Equal(record, forged) {
		t.Fatalf("joiner and a forged approval: %q %v", record, err)
	}
	if j.Check != p.Request.Check {
		must(t, j.Refuse(ctx, ErrPairRejected))
		m, err = readPairMessage(relayA)
		if err != nil || !errors.Is(m.refusal(), ErrPairRejected) {
			t.Fatalf("relay received %+v, %v from the joiner", m, err)
		}
	}
	return p.Request.Check, j.Check
}

func grindForTest(binding pairBinding, joinerNonce []byte, target string) ([]byte, int, time.Time) {
	start := time.Now()
	nonce := make([]byte, 32)
	for tries := 1; ; tries++ {
		binary.BigEndian.PutUint64(nonce, uint64(tries))
		if checkNumber(binding.checkDigest(nonce, joinerNonce)) == target {
			return nonce, tries, start
		}
	}
}

func readNonceForTest(t *testing.T, r io.Reader) [32]byte {
	t.Helper()
	m, err := readPairMessage(r)
	must(t, err)
	nonce, err := m.nonce()
	must(t, err)
	return nonce
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidPairName(t *testing.T) {
	for name, want := range map[string]bool{
		"MacBook Air": true, "小明的 Windows 电脑": true, "": false, " padded": false, "line\nbreak": false,
		"evil\u202Eexe.app": false, "zero\u200Bwidth": false, "sep\u2028": false, strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false, string([]byte{0xff}): false,
		// At most 160 bytes as well, what the device list keeps: 53 CJK
		// characters fit, 61 (183 bytes) or 64 (192 bytes) do not.
		strings.Repeat("长", 53): true, strings.Repeat("电", 61): false, strings.Repeat("长", 64): false,
	} {
		if ValidPairName(name) != want {
			t.Errorf("ValidPairName(%q) = %v", name, !want)
		}
	}
}

// A joiner that proves the code but names itself with more than the device
// list keeps (61 CJK characters, 183 bytes) is refused before the host asks
// its person: no request, ErrPairFailed. The joiner's own side refuses to
// send such a name at all.
func TestPairHostRefusesANameTheListCannotKeep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joiner, host, key := identityForTest(t), identityForTest(t), pairKeyForTest(t)
	long := strings.Repeat("电", 61)
	joined, accepted := pairSessionForTest(t, ctx, joiner, host)
	if _, err := JoinHost(ctx, joined, joiner, key, long, "darwin"); err == nil || !strings.Contains(err.Error(), "invalid device name") {
		t.Fatalf("joiner and a name of %d bytes: %v", len(long), err)
	}
	hostErr := make(chan error, 1)
	go func() {
		pending, err := ConfirmJoiner(ctx, accepted, host, key)
		if err == nil {
			err = errors.New("the host asks about " + pending.Request.Name)
		}
		hostErr <- err
	}()
	binding, err := bindPair(joined, joiner, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readPairMessage(joined); err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	commit := pairCommit(nonce[:])
	must(t, writePairMessage(joined, pairMessage{Version: 1, MAC: encode32(binding.joinerMAC(key)), Commit: encode32(commit[:]), Name: long, Platform: "darwin"}))
	if err := <-hostErr; !errors.Is(err, ErrPairFailed) {
		t.Fatalf("host and a %d-byte name: %v", len(long), err)
	}
}

func TestPairFramesAreStrict(t *testing.T) {
	frame := func(body string) []byte {
		return append(binary.BigEndian.AppendUint16(nil, uint16(len(body))), body...)
	}
	for _, raw := range [][]byte{
		frame(`{"nonce":"x"} {}`), frame(`{"nonce":"x","extra":1}`), frame(`[]`), {0, 0}, {0xff, 0xff},
		frame(`{"nonce":"x"}`)[:5],
	} {
		m, err := readPairMessage(bytes.NewReader(raw))
		if err == nil {
			if _, err := m.nonce(); err == nil {
				t.Errorf("accepted %q", raw)
			}
		}
	}
	// A frame never consumes bytes of the next one.
	var stream bytes.Buffer
	must(t, writePairMessage(&stream, pairMessage{Error: "rejected"}))
	must(t, writePairMessage(&stream, pairMessage{Error: "timeout"}))
	for _, want := range []error{ErrPairRejected, ErrPairTimedOut} {
		m, err := readPairMessage(&stream)
		if err != nil || !errors.Is(m.refusal(), want) {
			t.Fatalf("framed refusal %+v %v", m, err)
		}
	}
	if (pairMessage{Error: "rejected", Nonce: "x"}).refusal() != nil || (pairMessage{Error: "other"}).refusal() == nil {
		t.Fatal("refusal accepted extra fields or an unknown reason silently")
	}
}

// FuzzPairMessage drives the frame parser and both state machines with
// arbitrary peer bytes. Nothing may panic, and no input passes either MAC.
func FuzzPairMessage(f *testing.F) {
	key := bytes.Repeat([]byte{7}, 32)
	binding := pairBinding{exporter: [32]byte{1}, joiner: [32]byte{2}, host: [32]byte{3}}
	seed := func(messages ...pairMessage) []byte {
		var out bytes.Buffer
		for _, m := range messages {
			_ = writePairMessage(&out, m)
		}
		return out.Bytes()
	}
	nonce := encode32(make([]byte, 32))
	f.Add(seed(pairMessage{Version: 1, MAC: encode32(binding.joinerMAC(key)), Commit: nonce, Name: "A", Platform: "darwin"}, pairMessage{Nonce: nonce}))
	f.Add(seed(pairMessage{MAC: encode32(binding.hostProof(key))}, pairMessage{Nonce: nonce}, pairMessage{MAC: nonce, Record: `{"version":1}`}))
	f.Add(seed(pairMessage{MAC: nonce}))
	f.Add(seed(pairMessage{Error: "rejected"}))
	f.Add([]byte{0, 2, '{', '}'})
	f.Fuzz(func(t *testing.T, data []byte) {
		if m, err := readPairMessage(bytes.NewReader(data)); err == nil {
			_, _ = m.proof()
			if (&HostPairing{key: key, binding: binding, record: []byte(`{"version":1}`)}).confirmed(m) == nil {
				t.Fatalf("host accepted fuzzed bytes as the joiner's confirmation: %+v", m)
			}
			_, _, _ = m.hello()
			_, _ = m.nonce()
			_, _, _ = m.result()
			_ = m.refusal()
			var again bytes.Buffer
			if writePairMessage(&again, m) == nil {
				if back, err := readPairMessage(&again); err != nil || back != m {
					t.Fatalf("round trip changed %+v into %+v (%v)", m, back, err)
				}
			}
		}
		peer := struct {
			io.Reader
			io.Writer
		}{bytes.NewReader(data), io.Discard}
		if _, err := confirmJoiner(peer, key, binding); err == nil {
			t.Fatal("host accepted fuzzed bytes as a proof of the code")
		}
		peer.Reader = bytes.NewReader(data)
		if _, err := joinHost(peer, key, binding, "A", "darwin"); err == nil {
			j := &JoinerPairing{key: key, binding: binding}
			if m, err := readPairMessage(peer); err == nil {
				if record, err := j.result(m); err == nil {
					t.Fatalf("joiner accepted fuzzed approval %q", record)
				}
			}
		}
	})
}
