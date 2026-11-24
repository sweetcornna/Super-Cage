package controlclient

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The answer is the controller's: this vector was computed by
// controllerProofMAC of origin/controller-dial-hardening f35cd5f. After that
// branch merges, the workbench uses ProofMAC itself.
func TestProofMACMatchesTheController(t *testing.T) {
	got := ProofMAC(strings.Repeat("0123456789abcdef", 4), "localhost:17777", "127.0.0.1:50001", "127.0.0.1:17777", strings.Repeat("fedcba9876543210", 4))
	if got != "e05bf53a2ef582940551d1eb163a3700da026e42112ece0d5bc7f1f7ad9b8253" {
		t.Fatal(got)
	}
	nonce, body := ProofChallenge()
	if !proofKey(nonce) || string(body) != `{"nonce":"`+nonce+`","version":1}` {
		t.Fatal(nonce, string(body))
	}
	good := `{"version":1,"proof":"` + ProofMAC("k", "h", "c", "s", nonce) + `"}`
	if CheckProof([]byte(good), "k", "h", "c", "s", nonce) != nil {
		t.Fatal("a right answer was refused")
	}
	for _, wrong := range []string{
		strings.Replace(good, `"version":1`, `"version":2`, 1), `{"version":1}`, "",
		`{"version":1,"proof":"` + ProofMAC("k", "h", "s", "c", nonce) + `"}`,
	} {
		if CheckProof([]byte(wrong), "k", "h", "c", "s", nonce) != ErrControllerUnproven {
			t.Fatalf("accepted %q", wrong)
		}
	}
}

func writeRecord(t *testing.T, profile, extra string) {
	t.Helper()
	record := `{"port":17777,"token":"` + strings.Repeat("a", 64) + `","pid":42` + extra + `}`
	if err := os.WriteFile(filepath.Join(profile, "control.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

// What a record says about the proof, checked before anything is sent.
func TestRecordAnnouncesTheProof(t *testing.T) {
	key := strings.Repeat("5e", 32)
	for _, c := range []struct {
		name, extra string
		want        error
	}{
		{"before the proof", "", nil},
		{"proof v1", `,"auth":1,"proof_key":"` + key + `"`, nil},
		{"a newer proof", `,"auth":2,"proof_key":"` + key + `"`, ErrClientOutdated},
		{"v1 without a key", `,"auth":1`, errInvalidRecord},
		{"a key without a version", `,"proof_key":"` + key + `"`, errInvalidRecord},
		{"a malformed key", `,"auth":1,"proof_key":"` + strings.ToUpper(key) + `"`, errInvalidRecord},
		{"a negative version", `,"auth":-1`, errInvalidRecord},
	} {
		profile := t.TempDir()
		writeRecord(t, profile, c.extra)
		record, err := ReadRecord(profile)
		if err != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want == nil && c.extra != "" && (record.Auth != 1 || record.ProofKey != key) {
			t.Errorf("%s: %+v", c.name, record)
		}
	}
}

// fakeConn stands for a controller connection.
type fakeConn struct {
	record ControlFile
	calls  []string
	reply  func(path string) (int, error)
}

func (f *fakeConn) Record() ControlFile           { return f.record }
func (f *fakeConn) Launch() LaunchResponse        { return LaunchResponse{} }
func (f *fakeConn) SetTrustedViewCookie(*url.URL) {}
func (f *fakeConn) Close()                        {}
func (f *fakeConn) CallContext(_ context.Context, _, path string, _, _ any) (int, error) {
	f.calls = append(f.calls, path)
	return f.reply(path)
}

// holdLock makes profile's controller look alive.
func holdLock(t *testing.T, profile string) {
	t.Helper()
	f, err := OpenLock(profile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := Lock(f); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Unlock(f); f.Close() })
}

// Clients never contact a controller from before the proof.
func TestConnectProvenRefusesAControllerFromBeforeTheProof(t *testing.T) {
	profile := t.TempDir()
	holdLock(t, profile)
	old := &fakeConn{record: ControlFile{Port: 17777, Token: strings.Repeat("a", 64), PID: 42}, reply: func(string) (int, error) { return 503, nil }}
	open := func(string) (Conn, error) { return old, nil }
	if _, _, err := ConnectProven(profile, open); err != ErrControllerOutdated || len(old.calls) != 0 {
		t.Fatal(err, old.calls)
	}
	if _, _, err := Connect(profile, open); err != ErrControllerOutdated || len(old.calls) != 0 {
		t.Fatal(err, old.calls)
	}
}

// A server that fails the proof is named as such, not as a controller that
// is not running, all the way to the agent's answer.
func TestAnUnprovenControllerIsNamed(t *testing.T) {
	profile := t.TempDir()
	holdLock(t, profile)
	unproven := func(path string) (int, error) {
		return 0, &url.Error{Op: "POST", URL: "http://localhost:17777" + path, Err: ErrControllerUnproven}
	}
	conn := &fakeConn{record: ControlFile{Port: 17777, Token: strings.Repeat("a", 64), PID: 42, Auth: 1, ProofKey: strings.Repeat("5e", 32)}, reply: unproven}
	open := func(string) (Conn, error) { return conn, nil }
	if _, _, err := ConnectProven(profile, open); err != ErrControllerUnproven {
		t.Fatal(err)
	}
	connect := func(profile string) (Conn, error) {
		c, _, err := ConnectProven(profile, open)
		return c, err
	}
	_, err := AgentConnect(context.Background(), filepath.Join(profile, "config.json"), SameBuild("b"), connect)
	var refused *AgentError
	if !errors.As(err, &refused) || refused.Code != "controller_mismatch" || refused.Message != ErrControllerUnproven.Error() || Identity(err) != ErrControllerUnproven {
		t.Fatalf("%#v", err)
	}
	if err := Session(context.Background(), &fakeConn{reply: unproven}); err != ErrControllerUnproven {
		t.Fatal("a session over an unproven connection reads as", err)
	}
}
