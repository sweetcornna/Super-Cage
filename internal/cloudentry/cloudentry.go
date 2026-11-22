// Package cloudentry is what the three ends of Super Cage's cloud entry
// share (design 5.2): the owner's cage entry proxy, the portal gateway and
// the workspace's workbench. It holds the wire format of the admission (the
// Authorization line, the two headers the gateway sets, the activity mark)
// and the build gate that keeps the whole feature dark.
//
// It imports only the standard library's crypto and encoding, so the slim
// cage-attach can link it (cmd/cage-attach/main_test.go).
package cloudentry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// verified says whether the cloud entry has run end to end against a real
// cloud workspace. Until it has, every build ships with it empty and every
// part of the cloud entry stays closed:
//   - the gateway answers /_cage/entry as not found before it reads a header;
//   - the portal's device API refuses enrolment;
//   - the workbench registers none of its cloud routes;
//   - the desktop shows no cloud row and enrols nothing;
//   - cage entry proxy in cloud mode says it is not available.
//
// A build for the end-to-end run lifts it without a code change:
//
//	go build -ldflags "-X github.com/sylvinus/agent-vm/internal/cloudentry.verified=yes" ./cmd/cage ./cmd/cage-tenant ./cmd/cage-attach
//
// After a successful run "yes" is written here, in a reviewed commit.
var verified = ""

// open is verified, read once: tests lift it with Lift, never by writing
// verified, so a lifted test and a server goroutine never race.
var open atomic.Bool

func init() { open.Store(verified == "yes") }

// Open reports whether this build offers the cloud entry.
func Open() bool { return open.Load() }

// Lift opens the gate for one test and closes it again when the test ends.
// Only tests call it (TestOnlyTestsLift checks every non-test file).
func Lift(t interface{ Cleanup(func()) }) {
	before := open.Swap(true)
	t.Cleanup(func() { open.Store(before) })
}

// The wire format.
const (
	// Path is the gateway's entry on a workspace's host.
	Path = "/_cage/entry"
	// WorkbenchPath is the workbench's endpoint the gateway dials, and
	// DevicesPath the route that adds a device to the workspace's own list.
	// Both are under /api/entry/, which the gateway never proxies.
	WorkbenchPath = "/api/entry/cloud/ssh"
	DevicesPath   = "/api/entry/cloud/devices"
	// Scheme is the Authorization scheme of an admission.
	Scheme = "CageEntry"
	// HeaderDevice and HeaderKey are set by the gateway alone on its dial
	// of WorkbenchPath: the admitted device and the SHA-256 fingerprint of
	// its key. The gateway strips both from every request it proxies.
	HeaderDevice = "Cage-Entry-Device"
	HeaderKey    = "Cage-Entry-Key"
	// HeaderWake asks a cooled workspace to wake; only an interactive ssh
	// sends it.
	HeaderWake = "Cage-Entry-Wake"
	// HostKeyHeader carries the endpoint's host key in the 101 answer
	// (entryproxy.HostKeyHeader).
	HostKeyHeader = "Cage-Entry-Host-Key"
	// ActiveMark is the workbench's text message for owner activity. The
	// gateway never forwards text messages to the owner.
	ActiveMark = "{\"t\":\"active\"}"
	// MarkInterval is the least time between two marks, at the workbench
	// and again at the gateway.
	MarkInterval = 30 * time.Second
	// MaxSkew bounds |now - ts|; NonceWindow is how long a nonce is
	// remembered (twice the skew, so a replay is always seen).
	MaxSkew     = 60 * time.Second
	NonceWindow = 2 * MaxSkew
	// EnvironmentPrefix starts a cloud environment's id (cloud-<vm id>),
	// which can never be a local VM's id (hexadecimal only).
	EnvironmentPrefix = "cloud-"
)

// Authorization is one admission.
type Authorization struct {
	Device string // the portal's device id, 32 hexadecimal digits
	TS     int64  // unix seconds
	Nonce  string // 16 random bytes, base64url without padding
	Sig    []byte // Ed25519 over SignedMessage
}

// SignedMessage is what the device signs: the workspace's host as the
// owner dialled it (vm-<id>.<portal host>[:port]), the VM id, the time and
// the nonce.
func SignedMessage(host, vm string, ts int64, nonce string) []byte {
	return []byte("cage-entry-v1\n" + host + "\n" + vm + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce)
}

// Sign makes an admission for device at time now.
func Sign(key ed25519.PrivateKey, device, host, vm string, now time.Time, nonce [16]byte) Authorization {
	a := Authorization{Device: device, TS: now.Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce[:])}
	a.Sig = ed25519.Sign(key, SignedMessage(host, vm, a.TS, a.Nonce))
	return a
}

// Verify checks the signature against the device's public key.
func (a Authorization) Verify(key ed25519.PublicKey, host, vm string) bool {
	return len(key) == ed25519.PublicKeySize && ed25519.Verify(key, SignedMessage(host, vm, a.TS, a.Nonce), a.Sig)
}

// String is the Authorization header's value.
func (a Authorization) String() string {
	return Scheme + " device=" + a.Device + ", ts=" + strconv.FormatInt(a.TS, 10) + ", nonce=" + a.Nonce + ", sig=" + base64.RawURLEncoding.EncodeToString(a.Sig)
}

// Parse reads an Authorization value strictly: the four fields in this
// order, each once, nothing else.
func Parse(value string) (Authorization, bool) {
	rest, ok := strings.CutPrefix(value, Scheme+" ")
	if !ok {
		return Authorization{}, false
	}
	parts := strings.Split(rest, ", ")
	if len(parts) != 4 {
		return Authorization{}, false
	}
	var fields [4]string
	for i, name := range []string{"device", "ts", "nonce", "sig"} {
		field, ok := strings.CutPrefix(parts[i], name+"=")
		if !ok {
			return Authorization{}, false
		}
		fields[i] = field
	}
	var a Authorization
	if !ValidDevice(fields[0]) || len(fields[1]) == 0 || len(fields[1]) > 12 || len(fields[2]) != 22 || len(fields[3]) != 86 {
		return Authorization{}, false
	}
	for _, c := range fields[1] {
		if c < '0' || c > '9' {
			return Authorization{}, false
		}
	}
	ts, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Authorization{}, false
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(fields[2])
	if err != nil || len(nonce) != 16 {
		return Authorization{}, false
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(fields[3])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Authorization{}, false
	}
	a.Device, a.TS, a.Nonce, a.Sig = fields[0], ts, fields[2], sig
	return a, true
}

// Fresh reports whether ts is within MaxSkew of now.
func (a Authorization) Fresh(now time.Time) bool {
	skew := now.Unix() - a.TS
	return skew <= int64(MaxSkew/time.Second) && -skew <= int64(MaxSkew/time.Second)
}

// ValidDevice accepts a portal device id: 32 lower-case hexadecimal digits.
func ValidDevice(id string) bool { return validHex(id, 32) }

// ValidVM accepts a tenant VM id: 32 lower-case hexadecimal digits.
func ValidVM(id string) bool { return validHex(id, 32) }

func validHex(id string, size int) bool {
	if len(id) != size {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// Environment is a cloud environment's id; VMOf reads it back.
func Environment(vm string) string { return EnvironmentPrefix + vm }

func VMOf(environment string) (string, bool) {
	vm, ok := strings.CutPrefix(environment, EnvironmentPrefix)
	return vm, ok && ValidVM(vm)
}

// ParseAuthorizedKey reads an ssh-ed25519 public key in authorized_keys
// form ("ssh-ed25519 <base64> [comment]"): its raw key and its wire blob.
// Every other key type is refused: device keys are Ed25519.
func ParseAuthorizedKey(line string) (ed25519.PublicKey, []byte, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || len(fields) > 3 || fields[0] != "ssh-ed25519" || len(line) > 512 {
		return nil, nil, false
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(fields[1])
	const name = "ssh-ed25519"
	// string "ssh-ed25519", string key (32 bytes)
	if err != nil || len(blob) != 4+len(name)+4+ed25519.PublicKeySize ||
		string(blob[:4]) != "\x00\x00\x00\x0b" || string(blob[4:4+len(name)]) != name ||
		string(blob[4+len(name):8+len(name)]) != "\x00\x00\x00\x20" {
		return nil, nil, false
	}
	return ed25519.PublicKey(blob[8+len(name):]), blob, true
}

// AuthorizedKey is the authorized_keys line of a raw Ed25519 key, without
// a comment.
func AuthorizedKey(key ed25519.PublicKey) string {
	blob := append([]byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"), key...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

// Fingerprint is a key blob's SHA-256 fingerprint as ssh-keygen -l prints
// it: "SHA256:" and the unpadded standard base64 of the digest.
func Fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
