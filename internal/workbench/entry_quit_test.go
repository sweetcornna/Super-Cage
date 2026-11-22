//go:build !windows

package workbench

import (
	"strings"
	"testing"
	"time"
)

// Quitting the app counts as lock for the entry (owner decision 2026-11-22).
// The window quits through POST /api/account/suspend (api.js
// suspendBeforeQuit, before quitNow), and the desktop gateway sends the same
// request to the local view it shows when it closes, also when the app went
// without the window's quit (desktopGateway.close;
// TestDesktopQuitSuspendsTheLocalView). suspend ends the account's unlock as
// the lock does, so the entry needs nothing of its own: this test drives the
// window's lock and the quit's suspend over HTTP and checks that an ssh
// client sees the same. The connection is cut, a running command with it,
// the terminal gets the exit reset and the lock's notice, it is kept and
// offered again after unlock; a dial in between is refused as locked. Past
// the kept limit a lock ends the terminal (entry_filter_test.go); that is
// the same account.lock the suspend runs.
func TestEntryQuitCutsLikeLock(t *testing.T) {
	tails := map[string]string{}
	for _, route := range []string{"lock", "suspend"} {
		t.Run(route, func(t *testing.T) {
			f := newEntryFixture(t, true)
			f.on()
			client, err := f.dial(f.device)
			if err != nil {
				t.Fatal(err)
			}
			shell := entryOpenShell(t, client, 100, 30, "")
			shell.send("export MARK=kept42; printf 'R=%s\\n' ready\n")
			shell.expect("R=")
			command, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start("sleep 30"); err != nil {
				t.Fatal(err)
			}
			commandEnded := make(chan error, 1)
			go func() { commandEnded <- command.Wait() }()
			ended := make(chan struct{})
			go func() { _ = client.Wait(); close(ended) }()
			if w := request(f.s, "POST", "/api/account/"+route, f.s.origin, "{}", true); w.Code != 200 {
				t.Fatalf("POST /api/account/%s: %d %s", route, w.Code, w.Body.String())
			}
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s did not cut the connection", route)
			}
			select {
			case err := <-commandEnded:
				if err == nil {
					t.Fatal("the running command ended well")
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s left the running command", route)
			}
			text := shell.text()
			reset := strings.LastIndex(text, entryExitReset)
			if reset < 0 || !strings.Contains(text[reset:], entryLockedNotice) {
				t.Fatalf("no reset and notice before the cut: %q", text[max(0, len(text)-400):])
			}
			tails[route] = text[reset:]
			listed := entrySSHTerminals(f.s)
			if len(listed) != 1 || listed[0]["closed"] == true || listed[0]["origin"].(map[string]any)["kept"] != true {
				t.Fatalf("after %s: %v", route, listed)
			}
			if _, err := f.dial(f.device); err == nil || entryClientError(err) != "Super Cage 已锁定，请在应用中解锁" {
				t.Fatalf("dial after %s: %v", route, err)
			}
			f.s.account.setKey(make([]byte, 32))
			again := entryOpenShell(t, f.client(), 100, 30, "")
			again.prompt("输入编号重新接入")
			again.send("1\r")
			again.send("printf 'M=%s\\n' \"$MARK\"\n")
			if got := again.expect("M="); got != "kept42" {
				t.Fatalf("re-joined a different terminal after %s: M=%q", route, got)
			}
		})
	}
	if tails["lock"] == "" || tails["lock"] != tails["suspend"] {
		t.Fatalf("the quit's cut differs from the lock's: %q / %q", tails["lock"], tails["suspend"])
	}
}
