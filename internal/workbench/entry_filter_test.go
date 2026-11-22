//go:build !windows

package workbench

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// The ssh entry's pty sessions are windows the way cage attach's are: the
// same escape filter, the same settings file, the same rules for clipboard
// writes, answers and sizes, and the exit reset on every way out.

// entryExitReset is what every way out writes at least: termfilter's End for
// a guest that changed nothing End restores only when changed.
var entryExitReset = string(termfilter.New(termfilter.Options{}).End(nil, 0))

const entryTitlePop = "\x1b[23;0t"

func TestEntryOutputFramesLikeAttach(t *testing.T) {
	here := true
	output := &entryOutput{filter: termfilter.New(termfilter.Options{ClipboardWrite: true, ClipboardNow: func() bool { return here }})}
	frame := func(data string, replay, restart bool) string {
		return string(output.frame(nil, []byte(data), replay, restart))
	}
	// Live output passes, a clipboard write too while the owner is here.
	if got := frame("live\x1b]52;c;QUJD\x07", false, false); got != "live\x1b]52;c;QUJD\x07" {
		t.Fatalf("live %q", got)
	}
	here = false
	if got := frame("\x1b]52;c;QUJD\x07x", false, false); got != "x" {
		t.Fatalf("a write while the owner is away passed: %q", got)
	}
	here = true
	// History only draws: no clipboard write, no query.
	if got := frame("h\x1b]52;c;QUJD\x07\x1b[6n", true, false); got != "h" {
		t.Fatalf("replay %q", got)
	}
	// A restart spoils the write the window is inside, clears the window as
	// the filter does for ESC c and turns focus reports back on.
	frame("\x1b]52;c;QUJ", false, false)
	if !output.filter.Streaming() {
		t.Fatal("the write is not streaming")
	}
	clear := string(termfilter.New(termfilter.Options{}).Clear(nil))
	if got, want := frame("again", true, true), "!\x07"+clear+entryFocusOn+"again"; got != want || !strings.HasPrefix(clear, termfilter.ClearReset) {
		t.Fatalf("restart %q, want %q", got, want)
	}
	// A reset by the guest owes focus reports, written only once no
	// clipboard write is open.
	if got := frame("\x1bc\x1b]52;c;QUJ", false, false); strings.Contains(got, entryFocusOn) || !strings.Contains(got, termfilter.ClearReset) {
		t.Fatalf("reset inside a write %q", got)
	}
	if got := frame("D\x07after", false, false); got != "D\x07after"+entryFocusOn {
		t.Fatalf("after the write %q", got)
	}
}

func TestEntryPresenceIsAttachsRule(t *testing.T) {
	var p entryPresence
	guest := false
	asked := func() bool { return guest }
	if p.here() {
		t.Fatal("here before any input")
	}
	// Pointer motion with no button is not the owner at the window.
	if out := p.note([]byte("\x1b[<35;10;5M"), asked); string(out) != "\x1b[<35;10;5M" || p.here() {
		t.Fatalf("motion %q here=%v", out, p.here())
	}
	if out := p.note([]byte("a"), asked); string(out) != "a" || !p.here() {
		t.Fatal("a key does not mark the owner")
	}
	p.lastInput.Store(time.Now().Add(-attach.ClipboardWindow - time.Second).UnixNano())
	if p.here() {
		t.Fatal("still here after the clipboard window")
	}
	// Focus reports decide once they arrive; the program gets them only
	// when it asked for them.
	if out := p.note([]byte("\x1b[O"), asked); len(out) != 0 || p.here() {
		t.Fatalf("focus out %q here=%v", out, p.here())
	}
	if p.note([]byte("b"), asked); p.here() {
		t.Fatal("a key overrode the focus report")
	}
	guest = true
	if out := p.note([]byte("\x1b[I"), asked); string(out) != "\x1b[I" || !p.here() {
		t.Fatalf("focus in %q here=%v", out, p.here())
	}
	// Inside a bracketed paste a focus report's shape is pasted text.
	guest = false
	p.focus.Store(entryFocusOut)
	if out := p.note([]byte("\x1b[200~x\x1b[Iy"), asked); string(out) != "\x1b[200~x\x1b[Iy" || p.here() {
		t.Fatalf("paste %q here=%v", out, p.here())
	}
	if out := p.note([]byte("z\x1b[201~\x1b[I"), asked); string(out) != "z\x1b[201~" || !p.here() {
		t.Fatalf("after the paste %q here=%v", out, p.here())
	}
	if id := entryViewerID(); !strings.HasPrefix(id, "ssh-") || !terminalViewerPattern.MatchString(id) {
		t.Fatalf("viewer id %q", id)
	}
}

// The entry reads the file the desktop's settings page writes: the desktop
// keeps it beside its full configuration, the default profile, which is also
// the profile a local VM's controller maps back to.
func TestEntryReadsTheSettingsTheDesktopWrites(t *testing.T) {
	profile := t.TempDir()
	desktop := &Server{appUpdateConfigPath: filepath.Join(profile, "config.json")}
	vm := filepath.Join(profile, cage.LocalVMDir, "0123456789abcdef")
	if desktop.localTerminalDir() != profile || entryComputerProfile(profile) != profile || entryComputerProfile(vm) != profile {
		t.Fatalf("%s %s %s", desktop.localTerminalDir(), entryComputerProfile(profile), entryComputerProfile(vm))
	}
}

// Saving "允许环境写入剪贴板" off on the desktop's page stops clipboard writes
// in the next ssh terminal, though the owner is typing in it.
func TestEntryClipboardFollowsTheDesktopSetting(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	shell := entryOpenShell(t, f.client(), 100, 30, "")
	shell.send("printf 'C1=%s\\033]52;c;QUJD\\007\\n' x\n")
	shell.expect("C1=")
	if !strings.Contains(shell.text(), "\x1b]52;c;QUJD\x07") {
		t.Fatalf("the write did not pass with the default setting: %q", shell.text())
	}
	desktop := attachController(t, nil)
	desktop.remote = newDesktopGateway(desktop.profile, &desktopLocal{profile: f.s.profile})
	t.Cleanup(desktop.remote.close)
	desktop.appUpdateConfigPath = f.s.configPath
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"\",\"image_upload\":true,\"clipboard_write\":false}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	next := entryOpenShell(t, f.client(), 100, 30, "")
	next.send("printf 'C2=%s\\033]52;c;QUJD\\007\\n' x\n")
	next.expect("C2=")
	if strings.Contains(next.text(), "\x1b]52;c;") {
		t.Fatalf("the write passed with the setting off: %q", next.text())
	}
}

// The ssh window is a viewer of its own identity: answers to a query reach
// the program only while it decides the size, as for cage attach.
func TestEntryTerminalAnswersOnlyAsTheSizeOwner(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	shell := entryOpenShell(t, f.client(), 100, 30, "")
	shell.send("printf 'R=%s\\n' ready\n")
	shell.expect("R=")
	listed := entrySSHTerminals(f.s)
	if len(listed) != 1 {
		t.Fatalf("terminals %v", listed)
	}
	session := f.s.terminals.get(listed[0]["id"].(string))
	session.mu.Lock()
	var key string
	for k, p := range session.sizing.peers {
		if strings.HasPrefix(k, "v:ssh-") && p.live == 1 {
			key = k
		}
	}
	owner := session.sizing.owner
	session.mu.Unlock()
	if key == "" || owner != key {
		t.Fatalf("no ssh viewer owning the size: key %q owner %q", key, owner)
	}
	ask := func(name string, other bool) string {
		before := strings.Count(shell.text(), "\x1b[6n")
		shell.send("stty -echo; printf '\\033[6n'; A=; IFS= read -r -t 3 -d R A; stty echo; printf '" + name + "=%s.\\n' \"${A#*[}\"\n")
		entryAwait(t, 10*time.Second, "the query", func() bool { return strings.Count(shell.text(), "\x1b[6n") > before })
		if other {
			web := session.openPeer("web-viewer-1", false)
			defer session.closePeer(web)
			if _, err := session.peerInput(web, "\x15"); err != nil {
				t.Fatal(err)
			}
		}
		shell.send("\x1b[5;5R")
		return shell.expect(name + "=")
	}
	if got := ask("OWN", false); got != "5;5." {
		t.Fatalf("the owner's answer: %q", got)
	}
	if got := ask("OTHER", true); got != "." {
		t.Fatalf("an answer passed while another window decided the size: %q", got)
	}
}

// Every way out of an ssh terminal ends the window the same way, here with
// the guest inside a clipboard write: the write is spoiled, the exit reset
// follows, and after it only Super Cage's own line.
func TestEntryEveryWayOutEndsTheWindow(t *testing.T) {
	post := func(path string) func(*entryFixture, string) {
		return func(f *entryFixture, _ string) {
			if err := f.call("POST", path, map[string]any{}, nil); err != nil {
				f.t.Error(err)
			}
		}
	}
	cases := []struct {
		name    string
		fire    func(*entryFixture, string)
		notice  string
		limit   int
		program string
	}{
		{name: "program ends", program: "exec sleep 2"},
		{name: "closed in the app", fire: func(f *entryFixture, id string) {
			if err := f.call("DELETE", "/api/terminals/"+id, nil, nil); err != nil {
				f.t.Error(err)
			}
		}},
		{name: "lock", fire: func(f *entryFixture, _ string) { f.s.account.lock() }, notice: entryLockedNotice, limit: 4},
		{name: "lock past the kept limit", fire: func(f *entryFixture, _ string) { f.s.account.lock() }, notice: "这个终端随连接结束"},
		{name: "off", fire: post("/api/entry/off"), notice: errEntryOff.Message},
		{name: "revoke", fire: func(f *entryFixture, id string) { post("/api/entry/devices/"+f.computer.ID+"/revoke")(f, id) }, notice: errEntryRevoked.Message},
		{name: "quit", fire: func(f *entryFixture, _ string) { go func() { _ = f.s.closeService() }() }},
		{name: "pause", fire: func(f *entryFixture, _ string) { _ = f.s.pauseNetworkNow("测试暂停") }, notice: errEntryPaused.Message},
		{name: "stop", fire: func(f *entryFixture, _ string) {
			if err := f.s.stopEnvironment(); err != nil {
				f.t.Error(err)
			}
			entrySetRunning(f.t, f.s, false)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			previous := entryKeptLimit
			entryKeptLimit = c.limit
			t.Cleanup(func() { entryKeptLimit = previous })
			f := newEntryFixture(t, true)
			f.on()
			t.Cleanup(func() {
				for _, session := range f.s.terminals.keptSessions() {
					f.s.terminals.remove(session.id)
				}
				f.s.entryUnkept()
				for deadline := time.Now().Add(10 * time.Second); f.s.entryKeptTimerRunning() && time.Now().Before(deadline); {
					time.Sleep(20 * time.Millisecond)
				}
			})
			client, err := f.dial(f.device)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			shell := entryOpenShell(t, client, 100, 30, "")
			shell.send("printf 'R=%s\\n' ready\n")
			shell.expect("R=")
			id := entrySSHTerminals(f.s)[0]["id"].(string)
			program := c.program
			if program == "" {
				program = "exec sleep 60"
			}
			// Typed just now, so the write passes as the owner's.
			shell.send("printf '\\033]52;c;QUJD'; " + program + "\n")
			const open = "\x1b]52;c;QUJD"
			entryAwait(t, 10*time.Second, "the open clipboard write", func() bool { return strings.Contains(shell.text(), open) })
			if c.fire != nil {
				c.fire(f, id)
			}
			after := func() string {
				text := shell.text()
				return text[strings.Index(text, open)+len(open):]
			}
			entryAwait(t, 10*time.Second, "the exit reset", func() bool {
				return strings.Contains(after(), entryTitlePop) && strings.Contains(after(), c.notice)
			})
			rest := after()
			if !strings.HasPrefix(rest, "!\x07"+entryExitReset[:len("\x18\x1b\\\x1b[?2026l")]) {
				t.Fatalf("not spoiled, then reset: %q", rest)
			}
			tail := rest[strings.LastIndex(rest, entryTitlePop)+len(entryTitlePop):]
			if strings.IndexByte(tail, 0x1b) >= 0 || !strings.Contains(tail, c.notice) {
				t.Fatalf("after the reset: %q", tail)
			}
			t.Logf("%s: after the reset %q", c.name, tail)
		})
	}
}
