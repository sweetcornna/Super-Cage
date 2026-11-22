package mobilelink

import (
	"bytes"
	"context"
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
// relay room (the relay's PairTimeout, 2 minutes by default) and serves one
// device.
const DeviceCodeDigits = 8

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

// deviceApprovalTimeout bounds the host's 允许 / 拒绝 once a joiner has proven
// the code. The joiner waits 15 seconds longer for the answer.
var deviceApprovalTimeout = 60 * time.Second

// newDeviceCode is replaced in tests to force a collision on the relay.
var newDeviceCode = NewDeviceCode

// Errors of the short-code flow, written for the person.
var (
	ErrDeviceCodeFailed      = errors.New("配对失败：配对码不对或连接被篡改，请重新生成。")
	ErrDeviceRejected        = errors.New("对方拒绝了加入请求")
	ErrDeviceApprovalTimeout = errors.New("对方没有在 60 秒内允许，请重新获取配对码")
	ErrDeviceCodeUnknown     = errors.New("配对码不对、已过期或已被使用")
	ErrDeviceCodeUsed        = errors.New("配对码已被使用")
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
		pair, raw, err := NewPairCode(origin, room, host.Pin)
		if err != nil {
			clear(confirm)
			return nil, err
		}
		return &DeviceInvite{
			Code: code, Pair: pair, Raw: raw, Manual: "CAGE-" + base64.RawURLEncoding.EncodeToString([]byte(raw)), Expires: room.ExpiresAt,
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
// then gets its own bound (60 s).
//
// A cage-remote/1 peer is today's QR or long code: today's exchange, no
// approval, save(DeviceRequest{Pin}). A cage-pair/1 peer must prove the code;
// approve is called with a context that ends at the approval bound and
// returns true for 允许; then save, and the record goes back under the host's
// proof of the code.
func AcceptDevice(ctx context.Context, inv *DeviceInvite, host *remotelink.Identity, approve func(context.Context, DeviceRequest) bool, save func(DeviceRequest) (PairedHost, error)) error {
	if inv == nil || host == nil || approve == nil || save == nil {
		return errors.New("device invite, host identity and callbacks required")
	}
	defer clear(inv.confirm)
	temp, err := inv.Pair.identity()
	if err != nil {
		return err
	}
	codeCtx, cancel := context.WithDeadline(ctx, inv.Expires)
	defer cancel()
	ws, err := join(codeCtx, inv.origin, inv.Pair.Room, "host", inv.hostToken)
	if err != nil {
		return err
	}
	conn, err := remotelink.AcceptAfterJoin(codeCtx, ws, host, temp.Pin, true)
	if err != nil {
		return err
	}
	defer conn.NetConn().Close()
	if conn.ConnectionState().NegotiatedProtocol == remotelink.Protocol {
		return enrollPhone(codeCtx, conn, func(pin string) (PairedHost, error) { return save(DeviceRequest{Pin: pin}) })
	}
	pending, err := remotelink.ConfirmJoiner(codeCtx, conn, host, inv.confirm)
	clear(inv.confirm)
	if err != nil {
		return deviceError(err)
	}
	request := DeviceRequest{ShortCode: true, Pin: pending.Request.Pin, Name: pending.Request.Name, Platform: pending.Request.Platform, Check: pending.Request.Check}
	decide, stop := context.WithTimeout(ctx, deviceApprovalTimeout)
	allowed := approve(decide, request)
	expired := decide.Err() != nil
	stop()
	if ctx.Err() != nil {
		return ctx.Err()
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
	record, err := save(request)
	if err != nil {
		_ = pending.Refuse(reply, remotelink.ErrPairFailed)
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return pending.Approve(reply, payload)
}

// RedeemDeviceCode joins the host showing code. identity is this device's own
// key; the host records its pin from the TLS session. check is called with the
// four-digit number as soon as both screens can show it; the call then waits
// for the host's 允许 (the approval bound plus 15 seconds).
func RedeemDeviceCode(ctx context.Context, code, selectedRelay string, identity *remotelink.Identity, hello DeviceHello, check func(string)) (PairedHost, error) {
	var none PairedHost
	code, err := ParseDeviceCode(code)
	if err != nil {
		return none, err
	}
	if !ValidRelayOrigin(selectedRelay) {
		return none, errors.New("中继地址无效，请检查自建中继地址")
	}
	if identity == nil || !remotelink.ValidPairName(hello.Name) || !slices.Contains(remotelink.PairPlatforms, hello.Platform) {
		return none, errors.New("设备名称或平台无效")
	}
	lookup, confirm, err := deviceCodeKeys(ctx, code, selectedRelay)
	if err != nil {
		return none, err
	}
	defer clear(confirm)
	room, err := LookupRoom(ctx, selectedRelay, lookup)
	if errors.Is(err, ErrNoInvitation) {
		return none, ErrDeviceCodeUnknown
	}
	if err != nil {
		return none, err
	}
	codeCtx, cancel := context.WithDeadline(ctx, room.ExpiresAt)
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
	pending, err := remotelink.JoinHost(codeCtx, conn, identity, confirm, hello.Name, hello.Platform)
	if err != nil {
		return none, deviceError(err)
	}
	if check != nil {
		check(pending.Check)
	}
	wait, stop := context.WithTimeout(ctx, deviceApprovalTimeout+15*time.Second)
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
