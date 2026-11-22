package mobilelink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"time"

	"github.com/sylvinus/agent-vm/internal/remotelink"
	"golang.org/x/crypto/argon2"
)

// The short matching code: 8 digits typed by hand instead of the long code.
// One relay room serves today's CAGE1 QR and long code and the short code.
// The code is stretched with Argon2id into the relay lookup key and into a
// confirmation key that both devices prove inside the TLS session
// (remotelink.ConfirmJoiner and JoinHost). The relay learns sha256 of the
// lookup key only. The code is never written to disk or logs.

// DeviceCodeDigits is the length of a short code; it lives as long as its
// relay room (the relay's PairTimeout, 2 minutes by default), never more than
// deviceCodeLife whatever the relay answers, and serves one device.
const DeviceCodeDigits = 8

// deviceCodeLife bounds a code, its QR and the host's wait for the joiner's
// proof. The relay's expires_at is only an upper bound it may lower: a relay
// that answered a longer life would buy itself time to search the code.
const deviceCodeLife = 2 * time.Minute

const deviceCodeSalt = "super-cage device code v1\x00"

// deviceCodeKDF is Argon2id at t=3, 64 MiB, one lane: about 160 ms and 64 MiB
// per derivation on this project's Macs (design section 13). Host and joiner
// must agree, so changing it needs a new salt version. Tests lower it.
var deviceCodeKDF = struct {
	time, memory uint32
	threads      uint8
}{3, 64 << 10, 1}

// deviceCodeSlot runs one derivation at a time per process, so repeated
// requests cannot stack 64 MiB allocations.
var deviceCodeSlot = make(chan struct{}, 1)

// deviceJoinWindow bounds the joiner from the room lookup to the host's proof
// and the check number: joining the seat, TLS and the first proofs. The host
// speaks first right after TLS, so the relay's expires_at never extends it.
// The approval and the joiner's confirmation have their own bounds. Tests
// shorten it.
var deviceJoinWindow = 30 * time.Second

// DeviceApprovalTimeout bounds the host's 允许 / 拒绝 once a joiner has proven
// the code. The joiner waits 15 seconds longer for the answer. Tests shorten it.
var DeviceApprovalTimeout = 60 * time.Second

// newDeviceCode is replaced in tests to force a collision on the relay.
var newDeviceCode = NewDeviceCode

// Errors of the short-code flow, written for the person.
var (
	ErrDeviceCodeFailed      = errors.New("配对失败：配对码不对或连接被篡改，请重新生成。")
	ErrDeviceRejected        = errors.New("对方拒绝了加入请求")
	ErrDeviceApprovalTimeout = errors.New("对方没有在 60 秒内允许，请重新获取配对码")
	ErrDeviceCodeUnknown     = errors.New("配对码不对、已过期或已被使用")
	ErrDeviceCodeUsed        = errors.New("配对码已被使用")
	ErrDeviceJoinerLeft      = errors.New("对方已取消加入")
	ErrDeviceCodeExpired     = errors.New("配对码已过期，请重新生成")
	ErrDeviceNotConfirmed    = errors.New("加入的设备没有确认核对数字，没有添加")
	ErrDeviceHostLeft        = errors.New("对方电脑已取消，没有加入，也没有保存任何内容")
	errDeviceCodeFormat      = errors.New("请输入 8 位数字配对码")
)

// NewDeviceCode returns 8 uniformly random digits.
func NewDeviceCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08d", n.Int64()), nil
}

// FormatDeviceCode shows a code as two groups of four digits.
func FormatDeviceCode(code string) string {
	if len(code) != DeviceCodeDigits {
		return code
	}
	return code[:4] + " " + code[4:]
}

// ParseDeviceCode accepts what a person types: digits (also full-width),
// spaces and dashes. It returns exactly 8 ASCII digits.
func ParseDeviceCode(input string) (string, error) {
	digits := make([]byte, 0, DeviceCodeDigits)
	for _, r := range input {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, byte(r))
		case r >= '０' && r <= '９':
			digits = append(digits, byte('0'+r-'０'))
		case r == ' ' || r == '-' || r == '\u3000' || r == '\t':
			continue
		default:
			return "", errDeviceCodeFormat
		}
		if len(digits) > DeviceCodeDigits {
			return "", errDeviceCodeFormat
		}
	}
	if len(digits) != DeviceCodeDigits {
		return "", errDeviceCodeFormat
	}
	return string(digits), nil
}

// deviceCodeKeys stretches a code for one relay origin into the relay lookup
// bearer (base64url, as saved lookup tokens) and the 32-byte confirmation key.
func deviceCodeKeys(ctx context.Context, code, origin string) (string, []byte, error) {
	if parsed, err := ParseDeviceCode(code); err != nil || parsed != code {
		return "", nil, errDeviceCodeFormat
	}
	if !ValidRelayOrigin(origin) {
		return "", nil, errors.New("relay must use an HTTPS origin or a local loopback HTTP origin")
	}
	select {
	case deviceCodeSlot <- struct{}{}:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	stretched := argon2.IDKey([]byte(code), []byte(deviceCodeSalt+origin), deviceCodeKDF.time, deviceCodeKDF.memory, deviceCodeKDF.threads, 32)
	<-deviceCodeSlot
	defer clear(stretched)
	lookup, err := hkdf.Key(sha256.New, stretched, nil, "lookup", 32)
	if err != nil {
		return "", nil, err
	}
	defer clear(lookup)
	confirm, err := hkdf.Key(sha256.New, stretched, nil, "confirm", 32)
	if err != nil {
		return "", nil, err
	}
	return base64.RawURLEncoding.EncodeToString(lookup), confirm, nil
}

func relayConflict(err error) bool {
	var transient retryableError
	var status relayStatus
	return errors.As(err, &transient) && errors.As(transient.error, &status) && status == http.StatusConflict
}

// DeviceInvite is one relay room with two ways in: today's QR and long code
// (Raw and Manual, unchanged CAGE1 format, so today's phone apps scan it) and
// the short Code. Whoever claims the room's one seat first wins.
type DeviceInvite struct {
	Code    string // 8 digits: show it, never store or log it
	Pair    PairCode
	Raw     string // "CAGE1:..." for the QR
	Manual  string // "CAGE-..." long code
	Expires time.Time

	origin    string
	hostToken string
	confirm   []byte
}

// NewDeviceInvite creates the room for a new short code (one Argon2id
// derivation). When another host on the relay shows the same code (relay 409),
// it picks a new code, up to three codes.
func NewDeviceInvite(ctx context.Context, origin, admin string, host *remotelink.Identity) (*DeviceInvite, error) {
	if host == nil {
		return nil, errors.New("host identity required")
	}
	for range 3 {
		code, err := newDeviceCode()
		if err != nil {
			return nil, err
		}
		lookup, confirm, err := deviceCodeKeys(ctx, code, origin)
		if err != nil {
			return nil, err
		}
		room, err := CreateDiscoverableRoom(ctx, origin, admin, lookup)
		if relayConflict(err) {
			clear(confirm)
			continue
		}
		if err != nil {
			clear(confirm)
			return nil, err
		}
		// The QR's e, the dialog's countdown and AcceptDevice all follow this.
		if bound := time.Now().Add(deviceCodeLife); room.ExpiresAt.After(bound) {
			room.ExpiresAt = bound
		}
		pair, raw, err := NewPairCode(origin, room, host.Pin)
		if err != nil {
			clear(confirm)
			return nil, err
		}
		// The code carries whole seconds and expires at that second, for the
		// QR (PairCode.identity) and the short code alike.
		return &DeviceInvite{
			Code: code, Pair: pair, Raw: raw, Manual: pairCodePasted + base64.RawURLEncoding.EncodeToString([]byte(raw)), Expires: time.Unix(pair.Expires, 0),
			origin: origin, hostToken: room.HostToken, confirm: confirm,
		}, nil
	}
	return nil, errors.New("中继上的配对码冲突，请重新生成")
}

// DeviceRequest is a device asking to join. Pin is its key, proven by the TLS
// session. For a short-code join (ShortCode) Name and Platform are its own
// claims and Check is the four-digit number both screens show.
type DeviceRequest struct {
	ShortCode bool
	Pin       string
	Name      string
	Platform  string
	Check     string
}

// DeviceHello is what a joining device says about itself. Platform is one of
// remotelink.PairPlatforms; Name passes remotelink.ValidPairName.
type DeviceHello struct {
	Name     string
	Platform string
}

// AcceptDevice joins the invite's room as host and serves exactly one peer;
// afterwards the code and the QR are dead, whatever the outcome.
//
// ctx is the caller's cancel (dialog closed, new code, lock). Do not give it
// the code's 2-minute life: AcceptDevice applies inv.Expires itself up to the
// joiner's proof, because once both seats are taken the relay no longer does,
// and a stalling relay must not buy time to search the code. The approval
// and the joiner's confirmation then get their own bounds.
//
// A cage-remote/1 peer is today's QR or long code: today's exchange, no
// approval, save(DeviceRequest{Pin}, record). A cage-pair/1 peer must prove
// the code; approve is called with a context that ends at the approval bound
// or when the joiner leaves, and returns true for 允许. The record then goes to
// the joiner under the host's proof of the code, and save runs only once the
// joiner's person has confirmed the same check number. record is the pairing
// the joiner keeps (a fresh lookup token, as today).
func AcceptDevice(ctx context.Context, inv *DeviceInvite, host *remotelink.Identity, approve func(context.Context, DeviceRequest) bool, save func(DeviceRequest, PairedHost) error) error {
	if inv == nil || host == nil || approve == nil || save == nil {
		return errors.New("device invite, host identity and callbacks required")
	}
	defer clear(inv.confirm)
	// The QR's temporary key. Its expiry is inv.Expires, applied below.
	if len(inv.Pair.Seed) != ed25519.SeedSize {
		return errors.New("配对码无效或已过期")
	}
	temp, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(inv.Pair.Seed))
	if err != nil {
		return err
	}
	lookup, err := freshLookupToken()
	if err != nil {
		return err
	}
	record := PairedHost{Version: 1, Relay: inv.origin, HostPin: host.Pin, Lookup: lookup}
	codeCtx, cancel := context.WithDeadline(ctx, inv.Expires)
	defer cancel()
	ws, err := join(codeCtx, inv.origin, inv.Pair.Room, "host", inv.hostToken)
	if err != nil {
		return err
	}
	conn, err := remotelink.AcceptAfterJoin(codeCtx, ws, host, temp.Pin, true)
	if err != nil {
		if errors.Is(err, remotelink.ErrNoPeer) && ctx.Err() == nil {
			// Nobody came before the code expired or the relay ended its room.
			return fmt.Errorf("%w: %w", ErrDeviceCodeExpired, err)
		}
		return err
	}
	defer conn.NetConn().Close()
	if conn.ConnectionState().NegotiatedProtocol == remotelink.Protocol {
		return enrollPhone(codeCtx, conn, func(pin string) (PairedHost, error) { return record, save(DeviceRequest{Pin: pin}, record) })
	}
	pending, err := remotelink.ConfirmJoiner(codeCtx, conn, host, inv.confirm)
	clear(inv.confirm)
	if err != nil {
		return deviceError(err)
	}
	request := DeviceRequest{ShortCode: true, Pin: pending.Request.Pin, Name: pending.Request.Name, Platform: pending.Request.Platform, Check: pending.Request.Check}
	decide, stop := context.WithTimeout(ctx, DeviceApprovalTimeout)
	go func() {
		select {
		case <-pending.Left():
			stop()
		case <-decide.Done():
		}
	}()
	allowed := approve(decide, request)
	expired := decide.Err() != nil
	stop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case <-pending.Left():
		// The joiner gave up (closed its dialog, locked, timed out).
		return ErrDeviceJoinerLeft
	default:
	}
	reply, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	if !allowed || expired {
		reason, result := remotelink.ErrPairRejected, ErrDeviceRejected
		if expired {
			reason, result = remotelink.ErrPairTimedOut, ErrDeviceApprovalTimeout
		}
		_ = pending.Refuse(reply, reason)
		return result
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := pending.Approve(reply, payload); err != nil {
		return err
	}
	// The joiner's person compares the number too; only then is it saved, so
	// a joiner that declines or never answers leaves nothing behind here.
	confirmed, stopConfirm := context.WithTimeout(ctx, DeviceApprovalTimeout+15*time.Second)
	defer stopConfirm()
	if err := pending.AwaitConfirm(confirmed); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, remotelink.ErrPairFailed) {
			return ErrDeviceCodeFailed
		}
		return fmt.Errorf("%w: %w", ErrDeviceNotConfirmed, err)
	}
	return save(request, record)
}

func freshLookupToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

// RedeemDeviceCode joins the host showing code. identity is this device's own
// key; the host records its pin from the TLS session. check is called with the
// four-digit number as soon as both screens can show it; the call then waits
// for the host's 允许 (the approval bound plus 15 seconds). With 允许, confirm
// asks this side's person whether the host showed the same number (its
// context ends at the approval bound). Only true returns the pairing, and only
// then does the host save it: a relay that knows the code and forges 允许
// plants nothing unless the person confirms a number the host never showed.
func RedeemDeviceCode(ctx context.Context, code, selectedRelay string, identity *remotelink.Identity, hello DeviceHello, check func(string), confirm func(context.Context, string) bool) (PairedHost, error) {
	var none PairedHost
	code, err := ParseDeviceCode(code)
	if err != nil {
		return none, err
	}
	if !ValidRelayOrigin(selectedRelay) {
		return none, errors.New("中继地址无效，请检查自建中继地址")
	}
	if identity == nil || confirm == nil || !remotelink.ValidPairName(hello.Name) || !slices.Contains(remotelink.PairPlatforms, hello.Platform) {
		return none, errors.New("设备名称或平台无效")
	}
	lookup, key, err := deviceCodeKeys(ctx, code, selectedRelay)
	if err != nil {
		return none, err
	}
	defer clear(key)
	room, err := LookupRoom(ctx, selectedRelay, lookup)
	if errors.Is(err, ErrNoInvitation) {
		return none, ErrDeviceCodeUnknown
	}
	if err != nil {
		return none, err
	}
	// Not room.ExpiresAt: the relay chose it, and a relay that withholds the
	// host's proof must not keep this side waiting for it.
	codeCtx, cancel := context.WithTimeout(ctx, deviceJoinWindow)
	defer cancel()
	ws, err := join(codeCtx, selectedRelay, room.ID, "client", room.ClientToken)
	if relayConflict(err) {
		return none, ErrDeviceCodeUsed
	}
	if err != nil {
		return none, err
	}
	conn, err := remotelink.PairClient(codeCtx, ws, identity)
	if err != nil {
		return none, err
	}
	defer conn.NetConn().Close()
	pending, err := remotelink.JoinHost(codeCtx, conn, identity, key, hello.Name, hello.Platform)
	if err != nil {
		return none, deviceError(err)
	}
	if check != nil {
		check(pending.Check)
	}
	wait, stop := context.WithTimeout(ctx, DeviceApprovalTimeout+15*time.Second)
	defer stop()
	raw, err := pending.Await(wait)
	if err != nil {
		if ctx.Err() == nil && (wait.Err() != nil || errors.Is(err, context.DeadlineExceeded)) {
			return none, ErrDeviceApprovalTimeout
		}
		return none, deviceError(err)
	}
	var record PairedHost
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 ||
		record.HostPin != pending.HostPin || record.Relay != selectedRelay || !validCapability(record.Lookup) {
		return none, errors.New("电脑未完成配对，请重新获取配对码")
	}
	asked, stopAsking := context.WithTimeout(ctx, DeviceApprovalTimeout)
	go func() {
		select {
		case <-pending.Left():
			stopAsking()
		case <-asked.Done():
		}
	}()
	same := confirm(asked, pending.Check)
	expired := asked.Err() != nil
	stopAsking()
	select {
	case <-pending.Left():
		// The host closed its dialog or locked: nothing to confirm to.
		return none, ErrDeviceHostLeft
	default:
	}
	reply, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	if !same || expired {
		reason := remotelink.ErrPairRejected
		if expired {
			reason = remotelink.ErrPairTimedOut
		}
		_ = pending.Refuse(reply, reason)
		return none, ErrDeviceNotConfirmed
	}
	if err := pending.Confirm(reply); err != nil {
		return none, err
	}
	return record, nil
}

func deviceError(err error) error {
	switch {
	case errors.Is(err, remotelink.ErrPairFailed):
		return ErrDeviceCodeFailed
	case errors.Is(err, remotelink.ErrPairRejected):
		return ErrDeviceRejected
	case errors.Is(err, remotelink.ErrPairTimedOut):
		return ErrDeviceApprovalTimeout
	}
	return err
}
