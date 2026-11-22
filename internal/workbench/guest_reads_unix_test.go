//go:build !windows

package workbench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGuestSessionServer speaks the control-session protocol like the server
// that runs on a guest, but answers every script with a canned reply and
// records the scripts it was given. A script that did not go through it would
// need a real SSH connection, which these tests do not have.
const fakeGuestSessionServer = `
import sys
log, reply = sys.argv[1], sys.argv[2]
requests, replies = sys.stdin.buffer, sys.stdout.buffer
replies.write(b'CAGE-GUEST-SESSION 1\n')
replies.flush()
while True:
    fields = requests.readline(64).split()
    if len(fields) != 3 or fields[0] != b'R':
        break
    ident, size = int(fields[1]), int(fields[2])
    script = requests.read(size)
    with open(log, 'ab') as handle:
        handle.write(script + b'\n--\n')
    with open(reply, 'rb') as handle:
        out = handle.read()
    replies.write(b'D %d 0 %d 0\n' % (ident, len(out)) + out)
    replies.flush()
`

type fakeGuest struct{ log, reply string }

func (f *fakeGuest) answer(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(f.reply, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeGuest) scripts() string {
	data, _ := os.ReadFile(f.log)
	return string(data)
}

// serverWithFakeGuest is a server whose control connection to the guest is
// already open and served by fakeGuestSessionServer.
func serverWithFakeGuest(t *testing.T) (*Server, *fakeGuest) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	s := testServer(t)
	dir := t.TempDir()
	fake := &fakeGuest{log: filepath.Join(dir, "scripts"), reply: filepath.Join(dir, "reply")}
	fake.answer(t, "{}")
	t.Cleanup(s.controlSession.close)
	ctx := t.Context()
	_, sent, err := s.controlSession.run(ctx, ctx, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "python3", "-I", "-u", "-c", fakeGuestSessionServer, fake.log, fake.reply)
	}, "true")
	if err != nil || !sent {
		t.Fatalf("opening the fake control connection: sent=%v err=%v", sent, err)
	}
	return s, fake
}

// The reads that run on every launch and every accounts-page poll share the
// control connection instead of opening an SSH connection each.
func TestFrequentGuestReadsShareTheControlConnection(t *testing.T) {
	s, fake := serverWithFakeGuest(t)
	ctx := t.Context()
	claudeID, codexID := strings.Repeat("a", 24), strings.Repeat("b", 24)
	quota := `{"identity":"digest","five_hour":{"used_percentage":39,"resets_at":150},"observed_at":100}`
	// Each case answers with its canned reply and must find the script it
	// sent among those the fake guest received.
	cases := []struct {
		name   string
		reply  string
		script string
		check  func(t *testing.T) error
	}{
		{"claude quotas", `{"` + claudeID + `":` + quota + `}`, "cage-claude-account quotas " + claudeID, func(t *testing.T) error {
			got, err := s.readClaudeGuestQuotas(ctx, []string{claudeID})
			if err == nil && (got[claudeID].Identity != "digest" || got[claudeID].FiveHour == nil) {
				err = errors.New("reading was not parsed")
			}
			return err
		}},
		{"codex quotas", `{"` + codexID + `":` + quota + `}`, "cage-codex-account quotas " + codexID, func(t *testing.T) error {
			got, err := s.readCodexGuestQuotas(ctx, []string{codexID})
			if err == nil && got[codexID].Identity != "digest" {
				err = errors.New("reading was not parsed")
			}
			return err
		}},
		{"claude snapshot", `{"logged_in":true,"email":"a@example.org"}`, "cage-claude-account snapshot " + claudeID, func(t *testing.T) error {
			got, err := s.readClaudeGuestSnapshot(ctx, claudeID)
			if err == nil && (!got.LoggedIn || got.Email != "a@example.org") {
				err = errors.New("snapshot was not parsed")
			}
			return err
		}},
		{"claude default snapshot", `{"logged_in":true,"email":"a@example.org"}`, "CAGE_CLAUDE_SNAPSHOT", func(t *testing.T) error {
			got, err := s.readClaudeGuestSnapshot(ctx, "default")
			if err == nil && !got.LoggedIn {
				err = errors.New("snapshot was not parsed")
			}
			return err
		}},
		{"codex snapshot", `{"logged_in":true,"email":"a@example.org"}`, "cage-codex-account snapshot " + codexID, func(t *testing.T) error {
			got, err := s.probeCodex(ctx, codexID)
			if err == nil && (!got.LoggedIn || got.Email != "a@example.org") {
				err = errors.New("snapshot was not parsed")
			}
			return err
		}},
		{"codex default snapshot", `{"logged_in":true}`, "CAGE_CODEX_SNAPSHOT", func(t *testing.T) error {
			got, err := s.probeCodex(ctx, "default")
			if err == nil && !got.LoggedIn {
				err = errors.New("snapshot was not parsed")
			}
			return err
		}},
		{"project mount probe", "", "mountpoint -q '/home/cage/project'", func(t *testing.T) error {
			return s.projectMountReady(ctx, "'/home/cage/project'", "/home/cage/project/.probe", "challenge")
		}},
		{"environment versions", `{"manifest":null,"tools":{"git":"git version 2"}}`, "CAGE_VERSIONS", func(t *testing.T) error {
			data, err := s.readEnvironmentVersions(ctx)
			if err == nil && !strings.Contains(string(data), "git version 2") {
				err = errors.New("reply was not returned")
			}
			return err
		}},
		{"device system", `{"hostname":"cage","username":"alex","home":"/home/alex","audio_name":"","audio_description":""}`, "CAGE_DEVICE_SYSTEM", func(t *testing.T) error {
			if system := s.readDeviceSystem(ctx); system == nil || system.Hostname != "cage" || system.Username != "alex" {
				return errors.New("system was not read")
			}
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake.answer(t, c.reply)
			if err := c.check(t); err != nil {
				t.Fatalf("read failed without an SSH connection of its own: %v", err)
			}
			if !strings.Contains(fake.scripts(), c.script) {
				t.Fatalf("the control connection did not carry %q", c.script)
			}
		})
	}
}

// Usage readings contact the account's provider and can run long, and login
// recovery changes the account: both keep a connection of their own.
func TestUsageAndLoginRecoveryKeepTheirOwnConnection(t *testing.T) {
	s, fake := serverWithFakeGuest(t)
	ctx := t.Context()
	id, subject := strings.Repeat("a", 24), strings.Repeat("c", 64)
	fake.answer(t, `{"logged_in":true,"email":"a@example.org"}`)
	// Without an SSH connection these fail; what matters is that they did not
	// use the shared one.
	_, _ = s.readClaudeGuestUsage(ctx, id, subject)
	_, _ = s.readClaudeGuestReport(ctx, id, "exec /usr/local/bin/cage-claude-account recover-login "+id+" "+id+" "+subject)
	_, _ = s.readCodexGuestUsage(ctx, id, subject)
	if sent := fake.scripts(); strings.Contains(sent, " usage ") || strings.Contains(sent, "recover-login") {
		t.Fatalf("a long or mutating read used the shared connection: %q", sent)
	}
}
