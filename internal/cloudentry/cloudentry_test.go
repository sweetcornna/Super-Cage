package cloudentry

import (
	"crypto/ed25519"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClosedByDefault(t *testing.T) {
	if verified != "" || Open() {
		t.Fatal("the cloud entry is open in this build; only an -X build parameter or a reviewed commit opens it")
	}
}

// Only tests lift the gate: no other file of the module calls Lift.
func TestOnlyTestsLift(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "third_party" || strings.HasPrefix(entry.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "cloudentry.Lift(") || (filepath.Base(filepath.Dir(path)) == "cloudentry" && strings.Contains(string(data), "\tLift(")) {
			callers = append(callers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 0 {
		t.Fatalf("non-test files lift the cloud entry gate: %v", callers)
	}
}

func TestLiftIsUndoneAfterTheTest(t *testing.T) {
	t.Run("lifted", func(t *testing.T) {
		Lift(t)
		if !Open() {
			t.Fatal("not lifted")
		}
	})
	if Open() {
		t.Fatal("the lift outlived its test")
	}
}

func TestAuthorizationRoundTrip(t *testing.T) {
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	device := strings.Repeat("0a", 16)
	vm := strings.Repeat("b1", 16)
	host := "vm-" + vm + ".portal.example:9443"
	now := time.Unix(1_800_000_000, 0)
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	a := Sign(key, device, host, vm, now, nonce)
	line := a.String()
	if !strings.HasPrefix(line, "CageEntry device="+device+", ts=1800000000, nonce=") {
		t.Fatal(line)
	}
	parsed, ok := Parse(line)
	if !ok || !parsed.Verify(public, host, vm) || !parsed.Fresh(now.Add(MaxSkew)) || parsed.Fresh(now.Add(MaxSkew+time.Second)) || parsed.Fresh(now.Add(-MaxSkew-time.Second)) {
		t.Fatal("round trip")
	}
	for _, other := range [][2]string{{host, strings.Repeat("c", 32)}, {"vm-" + vm + ".portal.example", vm}, {strings.ToUpper(host), vm}} {
		if parsed.Verify(public, other[0], other[1]) {
			t.Fatalf("verified for %v", other)
		}
	}
	for _, bad := range []string{
		"", "CageEntry", line + " ", " " + line, strings.Replace(line, ", ", ",", 1),
		strings.Replace(line, "CageEntry", "Bearer", 1),
		strings.Replace(line, "device="+device, "device="+strings.ToUpper(device), 1),
		strings.Replace(line, "ts=1800000000", "ts=+1800000000", 1),
		strings.Replace(line, "ts=1800000000", "ts=", 1),
		strings.Replace(line, "nonce=", "nonce=A", 1),
		line[:len(line)-1],
		strings.Replace(line, "device=", "dev=", 1),
		line + ", extra=1",
	} {
		if _, ok := Parse(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestAuthorizedKeys(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	line := AuthorizedKey(public)
	key, blob, ok := ParseAuthorizedKey(line + " owner@mac")
	if !ok || !key.Equal(public) || len(blob) != 51 || !strings.HasPrefix(Fingerprint(blob), "SHA256:") || len(Fingerprint(blob)) != 7+43 {
		t.Fatal("ed25519 key")
	}
	for _, bad := range []string{"ssh-rsa " + strings.Fields(line)[1], "ssh-ed25519", "ssh-ed25519 !!!", line + " a b", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5"} {
		if _, _, ok := ParseAuthorizedKey(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	if vm, ok := VMOf(Environment(strings.Repeat("a", 32))); !ok || vm != strings.Repeat("a", 32) {
		t.Fatal("environment")
	}
	for _, bad := range []string{"default", "cloud-", "cloud-" + strings.Repeat("A", 32), strings.Repeat("a", 32)} {
		if _, ok := VMOf(bad); ok {
			t.Errorf("environment %q", bad)
		}
	}
}
