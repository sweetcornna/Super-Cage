package controlclient

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

// The controller proof: before a client sends any credential on a new
// connection, the server must show that it holds the proof key of the
// private record. This is the client half; the controller answers at
// ProofPath. A controller's record announces the version it speaks in Auth;
// a client reads it from the record, never from the network, so an impostor
// cannot talk a client down to an older protocol.

// ProofVersion is the version of the controller proof this client speaks.
const ProofVersion = 1

// ProofPath is where a controller answers the challenge.
const ProofPath = "/api/launch/proof"

// ProofLimit bounds the answer to a challenge.
const ProofLimit = 1 << 10

var (
	// ErrControllerUnproven: the server did not prove that it is this
	// profile's controller, and was sent nothing.
	ErrControllerUnproven = errors.New("无法确认本机后台身份，未发送凭据。请重新打开 Super Cage。")
	// ErrControllerOutdated: the controller predates the proof, and this
	// client does not talk to such a controller.
	ErrControllerOutdated = errors.New("Super Cage 后台需要更新：请打开 Super Cage 完成更新后重试。")
	// ErrClientOutdated: the controller speaks a newer proof.
	ErrClientOutdated = errors.New("此 Super Cage 需要更新：本机后台使用了更新的连接协议。")
)

// Identity returns the identity error err carries, whose sentence the owner
// must see as is (an update is needed, or the server did not prove itself)
// rather than as an unavailable controller; nil when it carries none.
func Identity(err error) error {
	for _, known := range []error{ErrControllerUnproven, ErrControllerOutdated, ErrClientOutdated} {
		if errors.Is(err, known) {
			return known
		}
	}
	return nil
}

// IdentityAgentError is Identity in the agent transport's vocabulary; nil
// when err carries no identity error.
func IdentityAgentError(err error) *AgentError {
	known := Identity(err)
	if known == nil {
		return nil
	}
	return &AgentError{Code: "controller_mismatch", Message: known.Error(), NextAction: "open_desktop", cause: known}
}

// ProofMAC binds an answer to one TCP connection: the client's and the
// server's address as each side sees them. A server that relays the
// challenge to the real controller over its own connection gets an answer
// for different addresses, which the client rejects.
func ProofMAC(key, host, client, server, nonce string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = io.WriteString(mac, "super-cage controller proof v1\x00"+host+"\x00"+client+"\x00"+server+"\x00"+nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// ProofChallenge returns a new nonce and the request body that carries it.
func ProofChallenge() (nonce string, body []byte) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	nonce = hex.EncodeToString(b[:])
	body, _ = json.Marshal(map[string]any{"version": ProofVersion, "nonce": nonce})
	return nonce, body
}

// CheckProof checks the body of a server's answer to the challenge nonce on
// the connection from client to server, named host.
func CheckProof(body []byte, key, host, client, server, nonce string) error {
	var reply struct {
		Version int    `json:"version"`
		Proof   string `json:"proof"`
	}
	want := ProofMAC(key, host, client, server, nonce)
	if len(body) > ProofLimit || json.Unmarshal(body, &reply) != nil || reply.Version != ProofVersion || !hmac.Equal([]byte(reply.Proof), []byte(want)) {
		return ErrControllerUnproven
	}
	return nil
}

// checkRecordProof checks what a record says about the proof: a version
// this client speaks, and a key exactly when it announces one.
func checkRecordProof(record ControlFile) error {
	if record.Auth > ProofVersion {
		return ErrClientOutdated
	}
	if record.Auth < 0 || (record.Auth == ProofVersion) != proofKey(record.ProofKey) || record.Auth == 0 && record.ProofKey != "" {
		return errInvalidRecord
	}
	return nil
}

// proofKey reports whether key has the form a controller writes: 64
// lower-case hex digits.
func proofKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
