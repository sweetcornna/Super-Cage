package remotelink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// PairProtocol is the TLS application protocol of a short-code pairing. Its
// handshake pins no key. Each side proves the code inside the session, bound
// to the session and to both keys (JoinHost, ConfirmJoiner); only then does
// either side trust the other's key.
const PairProtocol = "cage-pair/1"

const (
	pairExporterLabel = "EXPORTER-super-cage-device-code"
	maxPairFrame      = 4 << 10
	maxPairRecord     = 2 << 10
	maxPairName       = 64
)

// A refused short-code pairing ends with one of these. ErrPairFailed covers a
// wrong code, a tampered session and a malformed message alike.
var (
	ErrPairFailed   = errors.New("pairing confirmation failed: wrong code or tampered connection")
	ErrPairRejected = errors.New("pairing rejected by the host")
	ErrPairTimedOut = errors.New("pairing was not approved in time")
)

// ErrNoPeer means the wait for a device in the room ended with no device:
// the code expired, the relay ended the waiting room, or ctx ended.
var ErrNoPeer = errors.New("no device joined the room")

var pairRefusals = map[string]error{"failed": ErrPairFailed, "rejected": ErrPairRejected, "timeout": ErrPairTimedOut}

// pairRand supplies the commit-then-reveal nonces. Tests replace it.
var pairRand io.Reader = rand.Reader

// PairPlatforms are the platforms a joining device may report, as Go names
// them (runtime.GOOS), like the device report on a link.
var PairPlatforms = []string{"darwin", "windows", "linux", "android", "ios"}

// AcceptAfterJoin is ServerAfterJoin for a room that both today's QR or long
// code and a short code reach. The client's ALPN chooses the configuration:
//   - Protocol (cage-remote/1): exactly ServerAfterJoin's, pinned to legacyPin
//     through the unchanged peerVerifier.
//   - PairProtocol, only when pairing is true: TLS 1.3 with a required client
//     certificate and no pin. The caller must run ConfirmJoiner next.
//
// A client offering both gets PairProtocol when pairing is true. The returned
// connection's NegotiatedProtocol says which one ran.
func AcceptAfterJoin(ctx context.Context, ws *websocket.Conn, identity *Identity, legacyPin string, pairing bool) (_ *tls.Conn, err error) {
	if ws == nil {
		return nil, errors.New("WebSocket connection is required")
	}
	defer func() {
		if err != nil {
			_ = ws.Close()
		}
	}()
	verify, err := peerVerifier(legacyPin)
	if err != nil {
		return nil, err
	}
	if !usableIdentity(identity) {
		return nil, errors.New("local identity certificate and private key are required")
	}
	legacy := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos:             []string{Protocol},
		Certificates:           []tls.Certificate{identity.Certificate},
		VerifyConnection:       verify,
		SessionTicketsDisabled: true,
		ClientAuth:             tls.RequireAnyClientCert,
	}
	pair := legacy.Clone()
	pair.NextProtos = []string{PairProtocol}
	pair.VerifyConnection = verifyPairPeer
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, SessionTicketsDisabled: true,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if pairing && slices.Contains(hello.SupportedProtos, PairProtocol) {
				return pair, nil
			}
			return legacy, nil
		},
	}
	return pairHandshake(ctx, ws, config, true, true)
}

// PairClient is the joiner's cage-pair/1 handshake with its own device key.
// It accepts any valid host key; JoinHost decides whether to trust it.
func PairClient(ctx context.Context, ws *websocket.Conn, identity *Identity) (_ *tls.Conn, err error) {
	if ws == nil {
		return nil, errors.New("WebSocket connection is required")
	}
	defer func() {
		if err != nil {
			_ = ws.Close()
		}
	}()
	if !usableIdentity(identity) {
		return nil, errors.New("local identity certificate and private key are required")
	}
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos:             []string{PairProtocol},
		Certificates:           []tls.Certificate{identity.Certificate},
		VerifyConnection:       verifyPairPeer,
		SessionTicketsDisabled: true,
		// No key is known before the code is proven. VerifyConnection checks the
		// version, ALPN, certificate validity and key type; the MACs bind the key.
		InsecureSkipVerify: true,
	}
	return pairHandshake(ctx, ws, config, false, false)
}

func usableIdentity(identity *Identity) bool {
	return identity != nil && len(identity.Certificate.Certificate) != 0 && identity.Certificate.PrivateKey != nil
}

// verifyPairPeer is the cage-pair/1 counterpart of peerVerifier without a pin.
func verifyPairPeer(state tls.ConnectionState) error {
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != PairProtocol {
		return errors.New("peer did not negotiate the required TLS version and application protocol")
	}
	if len(state.PeerCertificates) == 0 {
		return errors.New("peer did not provide an identity certificate")
	}
	certificate := state.PeerCertificates[0]
	now := time.Now()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return errors.New("peer identity certificate is outside its validity period")
	}
	switch key := certificate.PublicKey.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() {
			return errors.New("peer identity must use Ed25519 or P-256")
		}
	default:
		return errors.New("peer identity must use Ed25519 or P-256")
	}
	return nil
}

// pairHandshake runs the TLS handshake with the bounds of handshake in link.go.
// The caller closes ws on error.
func pairHandshake(ctx context.Context, ws *websocket.Conn, config *tls.Config, server, waitForJoin bool) (*tls.Conn, error) {
	stream := newWebSocketConn(ws)
	if waitForJoin {
		wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
		stop := context.AfterFunc(wait, func() { _ = stream.Close() })
		kind, first, readErr := ws.NextReader()
		stop()
		waitErr := wait.Err()
		cancel()
		if waitErr != nil {
			return nil, fmt.Errorf("waiting for paired device: %w: %w", waitErr, ErrNoPeer)
		}
		if readErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoPeer, readErr)
		}
		if kind != websocket.BinaryMessage {
			return nil, errors.New("peer did not send a binary TLS handshake")
		}
		stream.reader = first
	}
	var connection *tls.Conn
	if server {
		connection = tls.Server(stream, config)
	} else {
		connection = tls.Client(stream, config)
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	err := connection.HandshakeContext(ctx)
	stop()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("remote TLS handshake: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("remote TLS handshake: %w", err)
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}
	return connection, nil
}

// pairBinding is what both MACs and the check number cover: the TLS exporter
// of this session and both keys. The exporter does not cover the client
// certificate (it follows the server Finished), so the pins must be in the MACs.
// Pins come from the session and the local identity, never from a message.
type pairBinding struct{ exporter, joiner, host [32]byte }

func bindPair(conn *tls.Conn, identity *Identity, host bool) (pairBinding, error) {
	var b pairBinding
	state := conn.ConnectionState()
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != PairProtocol || len(state.PeerCertificates) == 0 {
		return b, errors.New("pairing requires a completed cage-pair/1 session")
	}
	exported, err := state.ExportKeyingMaterial(pairExporterLabel, nil, len(b.exporter))
	if err != nil {
		return b, fmt.Errorf("pairing exporter: %w", err)
	}
	copy(b.exporter[:], exported)
	local, err := identityDigest(identity)
	if err != nil {
		return b, err
	}
	peer := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
	if host {
		b.host, b.joiner = local, peer
	} else {
		b.joiner, b.host = local, peer
	}
	return b, nil
}

func identityDigest(identity *Identity) ([32]byte, error) {
	if !usableIdentity(identity) {
		return [32]byte{}, errors.New("local identity certificate and private key are required")
	}
	leaf := identity.Certificate.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(identity.Certificate.Certificate[0])
		if err != nil {
			return [32]byte{}, errors.New("invalid local identity certificate")
		}
		leaf = parsed
	}
	return sha256.Sum256(leaf.RawSubjectPublicKeyInfo), nil
}

func pinOf(digest [32]byte) string {
	return "sha256:" + base64.StdEncoding.EncodeToString(digest[:])
}

// hostProof is the host's first message: it proves the code before the
// joiner says anything about itself.
func (b pairBinding) hostProof(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("cage-pair/1 host-proof\x00"))
	mac.Write(b.exporter[:])
	mac.Write(b.host[:])
	mac.Write(b.joiner[:])
	return mac.Sum(nil)
}

// joinerConfirm is the joiner's last message: its person saw the same check
// number, so the host may save it.
func (b pairBinding) joinerConfirm(key, record []byte) []byte {
	digest := sha256.Sum256(record)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("cage-pair/1 joiner-confirm\x00"))
	mac.Write(b.exporter[:])
	mac.Write(b.joiner[:])
	mac.Write(b.host[:])
	mac.Write(digest[:])
	return mac.Sum(nil)
}

func (b pairBinding) joinerMAC(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("cage-pair/1 joiner\x00"))
	mac.Write(b.exporter[:])
	mac.Write(b.joiner[:])
	mac.Write(b.host[:])
	return mac.Sum(nil)
}

func (b pairBinding) hostMAC(key, record []byte) []byte {
	digest := sha256.Sum256(record)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("cage-pair/1 host\x00"))
	mac.Write(b.exporter[:])
	mac.Write(b.host[:])
	mac.Write(b.joiner[:])
	mac.Write(digest[:])
	return mac.Sum(nil)
}

func pairCommit(nonce []byte) [32]byte {
	return sha256.Sum256(append([]byte("cage-pair/1 commit\x00"), nonce...))
}

func (b pairBinding) checkDigest(hostNonce, joinerNonce []byte) [32]byte {
	digest := sha256.New()
	digest.Write([]byte("cage-pair/1 check\x00"))
	digest.Write(b.exporter[:])
	digest.Write(hostNonce)
	digest.Write(joinerNonce)
	return [32]byte(digest.Sum(nil))
}

func checkNumber(digest [32]byte) string {
	return fmt.Sprintf("%04d", binary.BigEndian.Uint64(digest[:8])%10000)
}

// ValidPairName reports whether a device name may be shown in the approval
// prompt: trimmed, 1 to 64 characters, printable (no control, format or
// line-separator characters, so no bidirectional overrides).
func ValidPairName(name string) bool {
	if !utf8.ValidString(name) || name != strings.TrimSpace(name) {
		return false
	}
	count := utf8.RuneCountInString(name)
	if count == 0 || count > maxPairName {
		return false
	}
	for _, r := range name {
		if !unicode.IsGraphic(r) {
			return false
		}
	}
	return true
}

// pairMessage is one frame of the confirmation exchange. Each step accepts
// exactly its own fields (proof, hello, nonce, result, confirmation, refusal).
type pairMessage struct {
	Version  int    `json:"v,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	Name     string `json:"name,omitempty"`
	Platform string `json:"platform,omitempty"`
	Record   string `json:"record,omitempty"`
	Error    string `json:"error,omitempty"`
}

var errPairMessage = errors.New("malformed pairing message")

func writePairMessage(w io.Writer, m pairMessage) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > maxPairFrame {
		return errPairMessage
	}
	frame := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(body)), uint16(len(body)))
	_, err = w.Write(append(frame, body...))
	return err
}

// readPairMessage reads exactly one frame (2-byte big-endian length, then one
// JSON object) and nothing beyond it.
func readPairMessage(r io.Reader) (pairMessage, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return pairMessage{}, err
	}
	size := int(binary.BigEndian.Uint16(head[:]))
	if size == 0 || size > maxPairFrame {
		return pairMessage{}, errPairMessage
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return pairMessage{}, err
	}
	return parsePairMessage(body)
}

func parsePairMessage(body []byte) (pairMessage, error) {
	var m pairMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&m) != nil || decoder.Decode(new(any)) != io.EOF {
		return pairMessage{}, errPairMessage
	}
	return m, nil
}

func encode32(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func decode32(value string) ([32]byte, bool) {
	var out [32]byte
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != len(out) || base64.RawURLEncoding.EncodeToString(raw) != value {
		return out, false
	}
	copy(out[:], raw)
	return out, true
}

// proof is a frame that carries only a MAC: the host's proof of the code
// and the joiner's confirmation.
func (m pairMessage) proof() ([32]byte, error) {
	if m != (pairMessage{MAC: m.MAC}) {
		return [32]byte{}, errPairMessage
	}
	mac, ok := decode32(m.MAC)
	if !ok {
		return mac, errPairMessage
	}
	return mac, nil
}

func (m pairMessage) hello() (mac, commit [32]byte, err error) {
	if m.Version != 1 || m.Nonce != "" || m.Record != "" || m.Error != "" ||
		!ValidPairName(m.Name) || !slices.Contains(PairPlatforms, m.Platform) {
		return mac, commit, errPairMessage
	}
	mac, okMAC := decode32(m.MAC)
	commit, okCommit := decode32(m.Commit)
	if !okMAC || !okCommit {
		return mac, commit, errPairMessage
	}
	return mac, commit, nil
}

func (m pairMessage) nonce() ([32]byte, error) {
	if m.Version != 0 || m.MAC != "" || m.Commit != "" || m.Name != "" || m.Platform != "" || m.Record != "" || m.Error != "" {
		return [32]byte{}, errPairMessage
	}
	nonce, ok := decode32(m.Nonce)
	if !ok {
		return nonce, errPairMessage
	}
	return nonce, nil
}

func (m pairMessage) result() (mac [32]byte, record []byte, err error) {
	if m.Version != 0 || m.Commit != "" || m.Nonce != "" || m.Name != "" || m.Platform != "" || m.Error != "" ||
		m.Record == "" || len(m.Record) > maxPairRecord || !json.Valid([]byte(m.Record)) {
		return mac, nil, errPairMessage
	}
	mac, ok := decode32(m.MAC)
	if !ok {
		return mac, nil, errPairMessage
	}
	return mac, []byte(m.Record), nil
}

// refusal returns the other side's refusal when m is one, and nil otherwise.
func (m pairMessage) refusal() error {
	if m.Error == "" || m != (pairMessage{Error: m.Error}) {
		return nil
	}
	if err := pairRefusals[m.Error]; err != nil {
		return err
	}
	return errPairMessage
}

func refusalCode(reason error) string {
	for code, err := range pairRefusals {
		if err == reason {
			return code
		}
	}
	return "failed"
}

// pairWatch applies ctx to one step on conn and returns its release.
func pairWatch(ctx context.Context, conn *tls.Conn) func() {
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.NetConn().Close() })
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}
}

// pairStepError reports a step cut by ctx as ctx's error. The connection's
// deadline can fire just before ctx's own timer marks it done.
func pairStepError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

// JoinRequest is a joiner that proved the code. Name and Platform are its own
// claims; Pin is its key, proven by the TLS session; Check is the four-digit
// number both screens show.
type JoinRequest struct {
	Name     string
	Platform string
	Pin      string
	Check    string
}

// HostPairing waits for the person's 允许 or 拒绝, then for the joiner's
// person to confirm the same check number. It ends with Refuse, or with
// Approve and AwaitConfirm; the caller closes the connection.
type HostPairing struct {
	Request JoinRequest
	conn    *tls.Conn
	key     []byte
	binding pairBinding
	record  []byte
	// The joiner's next frame, read as soon as it arrives: before the host's
	// answer it means the joiner left; after Approve it is its confirmation.
	next    chan struct{}
	peer    pairMessage
	peerErr error
}

// Left is closed when the joiner leaves before the host's answer: its
// connection ends, or it sends anything at all. The approval should end then.
func (h *HostPairing) Left() <-chan struct{} { return h.next }

// ConfirmJoiner proves the code to the joiner first, then checks the joiner's
// proof and runs the commit-then-reveal exchange for the check number. key is
// the code's confirmation key. Any failure is final for this code and closes
// the connection.
func ConfirmJoiner(ctx context.Context, conn *tls.Conn, identity *Identity, key []byte) (*HostPairing, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("pairing key must be 32 bytes")
	}
	binding, err := bindPair(conn, identity, true)
	if err != nil {
		return nil, err
	}
	release := pairWatch(ctx, conn)
	request, err := confirmJoiner(conn, key, binding)
	release()
	if err != nil {
		_ = conn.NetConn().Close()
		return nil, pairStepError(ctx, err)
	}
	h := &HostPairing{Request: request, conn: conn, key: bytes.Clone(key), binding: binding, next: make(chan struct{})}
	go func() {
		h.peer, h.peerErr = readPairMessage(conn)
		close(h.next)
	}()
	return h, nil
}

func confirmJoiner(rw io.ReadWriter, key []byte, binding pairBinding) (request JoinRequest, err error) {
	defer func() {
		if err != nil {
			_ = writePairMessage(rw, pairMessage{Error: "failed"})
			request, err = JoinRequest{}, ErrPairFailed
		}
	}()
	// The host speaks first and proves the code: a joiner that finds no valid
	// proof says nothing, so a relay without the code learns at most the
	// joiner's key from the TLS handshake.
	if err := writePairMessage(rw, pairMessage{MAC: encode32(binding.hostProof(key))}); err != nil {
		return request, err
	}
	m, err := readPairMessage(rw)
	if err != nil {
		return request, err
	}
	mac, commit, err := m.hello()
	if err != nil {
		return request, err
	}
	request = JoinRequest{Name: m.Name, Platform: m.Platform, Pin: pinOf(binding.joiner)}
	if !hmac.Equal(mac[:], binding.joinerMAC(key)) {
		return request, ErrPairFailed
	}
	var hostNonce [32]byte
	if _, err := io.ReadFull(pairRand, hostNonce[:]); err != nil {
		return request, err
	}
	if err := writePairMessage(rw, pairMessage{Nonce: encode32(hostNonce[:])}); err != nil {
		return request, err
	}
	if m, err = readPairMessage(rw); err != nil {
		return request, err
	}
	joinerNonce, err := m.nonce()
	if err != nil {
		return request, err
	}
	if revealed := pairCommit(joinerNonce[:]); subtle.ConstantTimeCompare(revealed[:], commit[:]) != 1 {
		return request, ErrPairFailed
	}
	request.Check = checkNumber(binding.checkDigest(hostNonce[:], joinerNonce[:]))
	return request, nil
}

// Approve sends the record with the host's proof of the code. record must be
// JSON of at most 2 KiB.
func (h *HostPairing) Approve(ctx context.Context, record []byte) error {
	if len(record) == 0 || len(record) > maxPairRecord || !json.Valid(record) || !utf8.Valid(record) {
		return errors.New("pairing record must be JSON of at most 2 KiB")
	}
	release := pairWatch(ctx, h.conn)
	defer release()
	h.record = bytes.Clone(record)
	mac := h.binding.hostMAC(h.key, record)
	return pairStepError(ctx, writePairMessage(h.conn, pairMessage{MAC: encode32(mac), Record: string(record)}))
}

// AwaitConfirm waits, after Approve, for the joiner's person to confirm the
// check number. nil means the joiner confirmed this session's record; its
// refusal is ErrPairRejected or ErrPairTimedOut. Save the device only on nil.
func (h *HostPairing) AwaitConfirm(ctx context.Context) error {
	if h.record == nil {
		return errors.New("approve before awaiting the confirmation")
	}
	release := pairWatch(ctx, h.conn)
	defer release()
	select {
	case <-h.next:
	case <-ctx.Done():
		return ctx.Err()
	}
	if h.peerErr != nil {
		return pairStepError(ctx, h.peerErr)
	}
	return h.confirmed(h.peer)
}

func (h *HostPairing) confirmed(m pairMessage) error {
	if refused := m.refusal(); refused != nil {
		return refused
	}
	mac, err := m.proof()
	if err != nil || !hmac.Equal(mac[:], h.binding.joinerConfirm(h.key, h.record)) {
		return ErrPairFailed
	}
	return nil
}

// Refuse tells the joiner why it was not added: ErrPairRejected,
// ErrPairTimedOut or ErrPairFailed.
func (h *HostPairing) Refuse(ctx context.Context, reason error) error {
	release := pairWatch(ctx, h.conn)
	defer release()
	return pairStepError(ctx, writePairMessage(h.conn, pairMessage{Error: refusalCode(reason)}))
}

// JoinerPairing is a joiner whose check number can be shown. HostPin is the
// host key seen in this session. Await returns the host's approved record;
// it is trusted only once this side's person confirmed the check number
// (Confirm), or dropped (Refuse).
type JoinerPairing struct {
	Check   string
	HostPin string
	conn    *tls.Conn
	key     []byte
	binding pairBinding
	record  []byte
}

// JoinHost checks the host's proof of the code, then proves the code itself
// and runs the commit-then-reveal exchange. key is the code's confirmation
// key. It sends nothing (no name, no platform) unless the host's proof is
// valid. Any failure closes the connection.
func JoinHost(ctx context.Context, conn *tls.Conn, identity *Identity, key []byte, name, platform string) (*JoinerPairing, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("pairing key must be 32 bytes")
	}
	if !ValidPairName(name) || !slices.Contains(PairPlatforms, platform) {
		return nil, errors.New("invalid device name or platform")
	}
	binding, err := bindPair(conn, identity, false)
	if err != nil {
		return nil, err
	}
	release := pairWatch(ctx, conn)
	check, err := joinHost(conn, key, binding, name, platform)
	release()
	if err != nil {
		_ = conn.NetConn().Close()
		return nil, pairStepError(ctx, err)
	}
	return &JoinerPairing{Check: check, HostPin: pinOf(binding.host), conn: conn, key: bytes.Clone(key), binding: binding}, nil
}

func joinHost(rw io.ReadWriter, key []byte, binding pairBinding, name, platform string) (string, error) {
	first, err := readPairMessage(rw)
	if err != nil {
		return "", err
	}
	if refused := first.refusal(); refused != nil {
		return "", refused
	}
	proof, err := first.proof()
	if err != nil || !hmac.Equal(proof[:], binding.hostProof(key)) {
		return "", ErrPairFailed
	}
	var nonce [32]byte
	if _, err := io.ReadFull(pairRand, nonce[:]); err != nil {
		return "", err
	}
	commit := pairCommit(nonce[:])
	hello := pairMessage{Version: 1, MAC: encode32(binding.joinerMAC(key)), Commit: encode32(commit[:]), Name: name, Platform: platform}
	if err := writePairMessage(rw, hello); err != nil {
		return "", err
	}
	m, err := readPairMessage(rw)
	if err != nil {
		return "", err
	}
	if refused := m.refusal(); refused != nil {
		return "", refused
	}
	hostNonce, err := m.nonce()
	if err != nil {
		return "", ErrPairFailed
	}
	if err := writePairMessage(rw, pairMessage{Nonce: encode32(nonce[:])}); err != nil {
		return "", err
	}
	return checkNumber(binding.checkDigest(hostNonce[:], nonce[:])), nil
}

// Await waits for the host's decision. It returns the record only with a
// valid host proof of the code for this session.
func (j *JoinerPairing) Await(ctx context.Context) ([]byte, error) {
	release := pairWatch(ctx, j.conn)
	defer release()
	m, err := readPairMessage(j.conn)
	if err != nil {
		return nil, pairStepError(ctx, err)
	}
	return j.result(m)
}

func (j *JoinerPairing) result(m pairMessage) ([]byte, error) {
	if refused := m.refusal(); refused != nil {
		return nil, refused
	}
	mac, record, err := m.result()
	if err != nil || !hmac.Equal(mac[:], j.binding.hostMAC(j.key, record)) {
		return nil, ErrPairFailed
	}
	j.record = bytes.Clone(record)
	return record, nil
}

// Confirm tells the host that this side's person saw the same check number,
// so the host saves the device. Call it only after Await returned a record.
func (j *JoinerPairing) Confirm(ctx context.Context) error {
	if j.record == nil {
		return errors.New("no approved record to confirm")
	}
	release := pairWatch(ctx, j.conn)
	defer release()
	return pairStepError(ctx, writePairMessage(j.conn, pairMessage{MAC: encode32(j.binding.joinerConfirm(j.key, j.record))}))
}

// Refuse tells the host that this side's person did not confirm the check
// number: ErrPairRejected (the numbers differ) or ErrPairTimedOut.
func (j *JoinerPairing) Refuse(ctx context.Context, reason error) error {
	release := pairWatch(ctx, j.conn)
	defer release()
	return pairStepError(ctx, writePairMessage(j.conn, pairMessage{Error: refusalCode(reason)}))
}
