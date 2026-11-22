package controlclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSameBuild(t *testing.T) {
	controller := ControllerIdentity{Version: "0.1.15", Build: "abc", Protocol: Protocol, PID: 7}
	for _, c := range []struct {
		name  string
		check Check
		id    ControllerIdentity
		want  bool
	}{
		{"same build", SameBuild("abc"), controller, true},
		{"other build", SameBuild("abd"), controller, false},
		{"unknown own build", SameBuild(""), ControllerIdentity{Protocol: Protocol}, false},
		{"other protocol", SameBuild("abc"), ControllerIdentity{Build: "abc", Protocol: 2}, false},
	} {
		if got := c.check(c.id); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// cage-attach is accepted only as the file its controller verified beside
// itself, with the controller's build stamp.
func TestSameCompanion(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "cage-attach")
	if err := os.WriteFile(self, []byte("slim program"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "cage-attach")
	if err := os.WriteFile(other, []byte("slim program"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return self, nil }
	t.Cleanup(func() { executable = os.Executable })
	resolved, _ := filepath.EvalSymlinks(self)
	verified := CompanionOf(resolved)
	if verified == nil || verified.SHA256 == "" {
		t.Fatal("no companion")
	}
	if again := CompanionOf(resolved); *again != *verified {
		t.Fatal("the cached description differs")
	}
	good := ControllerIdentity{Build: "b", Protocol: Protocol, Stamp: "s1", Attach: verified}
	elsewhere := *CompanionOf(other)
	tampered := *verified
	tampered.SHA256 = "00"
	tampered.ModTime--
	for _, c := range []struct {
		name  string
		stamp string
		id    ControllerIdentity
		want  bool
	}{
		{"the verified file, same stamp", "s1", good, true},
		{"another stamp (a stale build)", "s0", good, false},
		{"no stamp (a source build)", "", ControllerIdentity{Build: "b", Protocol: Protocol, Attach: verified}, false},
		{"controller verified nothing", "s1", ControllerIdentity{Build: "b", Protocol: Protocol, Stamp: "s1"}, false},
		{"another installation's copy", "s1", ControllerIdentity{Build: "b", Protocol: Protocol, Stamp: "s1", Attach: &elsewhere}, false},
		{"changed content", "s1", ControllerIdentity{Build: "b", Protocol: Protocol, Stamp: "s1", Attach: &tampered}, false},
		{"other protocol", "s1", ControllerIdentity{Build: "b", Protocol: 2, Stamp: "s1", Attach: verified}, false},
	} {
		if got := SameCompanion(c.stamp)(c.id); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// A rewritten file is hashed again; the old description no longer fits.
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(self, []byte("another program"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(self, later, later)
	if SameCompanion("s1")(good) {
		t.Fatal("a replaced program was accepted")
	}
	if now := CompanionOf(resolved); now == nil || now.SHA256 == verified.SHA256 {
		t.Fatal("the cache kept a replaced file")
	}
}
