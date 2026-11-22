package entryproxy

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyLine(kind string, body string) string {
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(kind)))
	blob = append(append(blob, kind...), body...)
	return kind + " " + base64.StdEncoding.EncodeToString(blob)
}

// The pin is compared without x/crypto/ssh: the same type and key blob.
func TestPinnedKeyComparesTypeAndBlob(t *testing.T) {
	paths := Paths{SSHDir: t.TempDir()}
	pinned := keyLine("ssh-ed25519", "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(filepath.Dir(paths.KnownHosts("default")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.KnownHosts("default"), []byte("cage-default "+pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kind, blob, ok := pinnedKey(paths, "default")
	if !ok || kind != "ssh-ed25519" {
		t.Fatal("pinned key unread")
	}
	for line, same := range map[string]bool{
		pinned + " comment": true,
		keyLine("ssh-ed25519", "another key of the same length!!"): false,
		keyLine("ssh-rsa", "0123456789abcdef0123456789abcdef"):     false,
	} {
		offered, offeredBlob, ok := parseKey(strings.Fields(line))
		if !ok || (offered == kind && string(offeredBlob) == string(blob)) != same {
			t.Errorf("%q: ok %v", line, ok)
		}
	}
	// A blob whose inner type is not the one named is no key.
	forged := "ssh-ed25519 " + strings.Fields(keyLine("ssh-rsa", "x"))[1]
	if _, _, ok := parseKey(strings.Fields(forged)); ok {
		t.Error("a mislabelled key was read")
	}
	if _, _, ok := pinnedKey(paths, "0123456789abcdef"); ok {
		t.Error("a key for an environment never pinned")
	}
}

func TestRefusalsAndProfiles(t *testing.T) {
	if r := HTTPRefusal(409, []byte(`{"code":"off","message":"本机终端未在这个环境里开启"}`)); r.Status != 409 || r.Code != "off" || r.Message != "本机终端未在这个环境里开启" {
		t.Fatal(r)
	}
	if HTTPRefusal(423, []byte("locked")) != ErrLocked || HTTPRefusal(503, nil) != ErrClosing || HTTPRefusal(403, nil) != ErrLocalOnly {
		t.Fatal("status fallbacks")
	}
	if r := HTTPRefusal(500, nil); r.Code != "failed" || !strings.Contains(r.Message, "500") {
		t.Fatal(r)
	}
	if ClientError(HTTPRefusal(423, nil)) != ErrLocked.Message {
		t.Fatal("client error of a refusal")
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if got, err := ProfileConfig(config, "default"); err != nil || got != config {
		t.Fatal(got, err)
	}
	if got, err := ProfileConfig(config, "0123456789abcdef"); err != nil || got != filepath.Join(filepath.Dir(config), "vms", "0123456789abcdef", "config.json") {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "../x", "0123456", "0123456789ABCDEF", strings.Repeat("a", 65)} {
		if _, err := ProfileConfig(config, bad); err == nil {
			t.Errorf("environment %q accepted", bad)
		}
	}
}
