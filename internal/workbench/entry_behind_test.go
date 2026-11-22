//go:build !windows

package workbench

import (
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// R3-3: a cut while the client is behind (it reads nothing, so its window
// is full and the endpoint's write waits) still ends the window with the
// exit reset and the notice: guest output stops at the cut, and the client
// has entryCutGrace to take in what was on its way.
func TestEntryCutReachesAClientThatIsBehind(t *testing.T) {
	post := func(path string) func(*entryFixture) {
		return func(f *entryFixture) {
			if err := f.call("POST", path, map[string]any{}, nil); err != nil {
				f.t.Error(err)
			}
		}
	}
	cases := []struct {
		name   string
		fire   func(*entryFixture)
		notice string
		limit  int
	}{
		{name: "lock", fire: func(f *entryFixture) { f.s.account.lock() }, notice: entryLockedNotice, limit: 4},
		{name: "off", fire: post("/api/entry/off"), notice: errEntryOff.Message},
		{name: "revoke", fire: func(f *entryFixture) { post("/api/entry/devices/" + f.computer.ID + "/revoke")(f) }, notice: errEntryRevoked.Message},
		{name: "stop", fire: func(f *entryFixture) {
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
			})
			client, err := f.dial(f.device)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
				t.Fatal(err)
			}
			stdin, _ := session.StdinPipe()
			stdout, _ := session.StdoutPipe()
			if err := session.Shell(); err != nil {
				t.Fatal(err)
			}
			// Far more than the client's window, and nothing read.
			if _, err := io.WriteString(stdin, "head -c 20000000 /dev/zero | tr '\\0' x; exec sleep 60\n"); err != nil {
				t.Fatal(err)
			}
			entryAwait(t, 10*time.Second, "the endpoint waiting on the client", func() bool { return len(entrySSHTerminals(f.s)) == 1 })
			time.Sleep(time.Second)
			start := time.Now()
			c.fire(f)
			// Later than the old grace (250 ms), well within the new one.
			time.Sleep(500 * time.Millisecond)
			read := make(chan string, 1)
			go func() {
				read <- entryDrain(stdout)
			}()
			var text string
			select {
			case text = <-read:
			case <-time.After(10 * time.Second):
				t.Fatal("the client's stream did not end")
			}
			took := time.Since(start)
			last := strings.LastIndexByte(text, 'x')
			if last < 0 {
				t.Fatal("no output at all")
			}
			rest := text[last+1:]
			if !strings.Contains(rest, entryTitlePop) || !strings.Contains(rest, c.notice) {
				t.Fatalf("after the guest's output: %q", rest)
			}
			tail := rest[strings.LastIndex(rest, entryTitlePop)+len(entryTitlePop):]
			if strings.IndexByte(tail, 0x1b) >= 0 {
				t.Fatalf("after the reset: %q", tail)
			}
			// What reached the client is what its window held when the
			// guest output stopped, and one frame more at most.
			if guest := strings.Count(text, "x"); guest > 2<<20+terminalFrameLimit+4<<10 {
				t.Fatalf("%d bytes of guest output after the cut", guest)
			}
			t.Logf("%s: %d bytes of guest output, then the reset and %q; the stream ended %v after the cut", c.name, strings.Count(text, "x"), tail, took.Round(10*time.Millisecond))
		})
	}
}

// entryDrain reads a channel to its end as a terminal client does: what
// arrived is read even when a read also reports an error. x/crypto/ssh
// returns the failure of the window adjustment it sends after a read
// together with the data, while the connection is ending; the rest is still
// in its buffer.
func entryDrain(r io.Reader) string {
	var out []byte
	buffer := make([]byte, 32<<10)
	for {
		n, err := r.Read(buffer)
		out = append(out, buffer[:n]...)
		if n == 0 && err != nil {
			return string(out)
		}
	}
}
