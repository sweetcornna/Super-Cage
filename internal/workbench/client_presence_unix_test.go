//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// The probe runs in the guest's Bash exactly as written; here with its own
// HOME, PATH and receipt folder, and names no guest would hold.
func TestClientPresenceProbeInBash(t *testing.T) {
	root := t.TempDir()
	home, bin, receipts := filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "agent-clients")
	for _, dir := range []string{filepath.Join(home, ".local", "bin"), bin, receipts} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	program := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	run := func(locale string) string {
		t.Helper()
		probe := clientPresenceProbe(receipts)
		cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", probe)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "LC_ALL=" + locale}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return string(out)
	}
	if got := run("C"); got != "agent-clients/1 present= receipts=\n" || len(got) > 64 {
		t.Fatalf("nothing installed: %q", got)
	}
	program(filepath.Join(home, ".local", "bin", "codex"), 0o755)
	// Not executable, and a directory: neither is found.
	program(filepath.Join(bin, "agy"), 0o644)
	if err := os.Mkdir(filepath.Join(bin, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex.json", "co\ndex.json", ".codex.json.new", "Codex.json", "é.json", "-x.json", "codex.txt", strings.Repeat("a", 33) + ".json"} {
		program(filepath.Join(receipts, name), 0o644)
	}
	if err := os.Mkdir(filepath.Join(receipts, "folder.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, locale := range []string{"C", "en_US.UTF-8"} {
		got := run(locale)
		if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
			t.Fatalf("%s: not one line: %q", locale, got)
		}
		p, ok := parseClientPresence(strings.TrimSuffix(got, "\n"))
		if !ok || !slices.Equal(p.Clients, []string{"codex"}) || !slices.Equal(p.Receipts, []string{"codex"}) {
			t.Fatalf("%s: %q read as %+v", locale, got, p)
		}
	}
	// The PATH of the session counts too, and at most 32 receipts are listed.
	if err := os.Remove(filepath.Join(home, ".local", "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(bin, "agy"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		program(filepath.Join(receipts, "r"+strconv.Itoa(100+i)+".json"), 0o644)
	}
	got := run("C")
	p, ok := parseClientPresence(strings.TrimSuffix(got, "\n"))
	if !ok || !slices.Equal(p.Clients, []string{"antigravity-cli"}) || len(p.Receipts) != clientReceiptLimit {
		t.Fatalf("%q read as %+v", got, p)
	}
}

// recordingGuestSession answers like cannedGuestSession and appends every
// script it receives to a file.
const recordingGuestSession = `import sys
out, inp = sys.stdout.buffer, sys.stdin.buffer
payload, log = sys.argv[1].encode(), sys.argv[2]
out.write(b"CAGE-GUEST-SESSION 1\n")
out.flush()
while True:
    fields = inp.readline().split()
    if len(fields) != 3 or fields[0] != b"R":
        break
    script = inp.read(int(fields[2]))
    with open(log, "ab") as f:
        f.write(b"%d\n" % len(script) + script)
    out.write(b"D %d 0 %d 0\n" % (int(fields[1]), len(payload)) + payload)
    out.flush()
`

// The presence rides the attach read: still one guest command per attach.
func TestClientPresenceAttachKeepsOneGuestQuick(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	s := transferMountServer(t, true, "current")
	s.clientsTracked = true
	manifest, err := json.Marshal(cage.RequiredGuestManifest())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := "{\"token\":\"attach-token\"}\nvnc-password\nagent-clients/1 present=codex receipts=codex\n" + string(manifest) + "\n"
	log := filepath.Join(t.TempDir(), "scripts")
	ctx := t.Context()
	t.Cleanup(s.controlSession.close)
	if _, sent, err := s.controlSession.run(ctx, ctx, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "python3", "-I", "-u", "-c", recordingGuestSession, snapshot, log)
	}, "true"); err != nil || !sent {
		t.Fatalf("opening control connection: sent=%v err=%v", sent, err)
	}
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}, nil
	}))
	if err := s.attach(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var scripts []string
	for len(data) > 0 {
		size, rest, _ := strings.Cut(string(data), "\n")
		n, err := strconv.Atoi(size)
		if err != nil || n > len(rest) {
			t.Fatalf("bad log %q", data)
		}
		scripts = append(scripts, rest[:n])
		data = []byte(rest[n:])
	}
	if len(scripts) != 2 || scripts[0] != "true" || scripts[1] != guestConnectionSnapshot {
		t.Fatalf("guest scripts during attach: %d, want the opener and one snapshot", len(scripts))
	}
	s.mu.Lock()
	presence := s.clients
	s.mu.Unlock()
	if !presence.Known || !slices.Equal(presence.Clients, []string{"codex"}) {
		t.Fatalf("presence after attach: %+v", presence)
	}
	if p := loadClientPresence(s.profile); !p.Known || !slices.Equal(p.Clients, []string{"codex"}) {
		t.Fatalf("last known file: %+v", p)
	}
}
