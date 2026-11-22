//go:build !windows

package workbench

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
)

// A stand-in for the guest's clipboard helper on the far side of ssh: it
// says hello, keeps every line the workbench sends it and acknowledges each
// one, as shared-clipboard.py does for a text it took.
const clipboardGuestFake = `#!/bin/sh
printf '%s\n' '{"hello":{"version":1,"os":"linux","session":"fake-guest","formats":["text","png","files"],"available":true}}'
while IFS= read -r line; do
  printf '%s\n' "$line" >> %s
  printf '{"ack":%s}\n' "$(printf '%s' "$line" | sed -n 's/.*"serial":\([0-9]*\).*/\1/p')"
done
`

// The host's clipboard, pushed to the environment by the window, never
// carries a long pairing code: the workbench answers it as an unchanged
// clipboard, and what comes before and after still crosses.
func TestSharedClipboardNeverSendsAPairingCodeToTheGuest(t *testing.T) {
	dir := t.TempDir()
	received := filepath.Join(dir, "received")
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte(fmt.Sprintf(clipboardGuestFake, "%s", "%s", received, "%s", "%s")), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := sshProgram
	sshProgram = fake
	t.Cleanup(func() { sshProgram = saved })

	_, raw, err := mobilelink.NewPairCode("https://relay.example.com", relay.Invitation{ID: "room", ClientToken: "token", ExpiresAt: time.Now().Add(time.Minute)}, "host-pin")
	if err != nil {
		t.Fatal(err)
	}
	manual := "CAGE-" + base64.RawURLEncoding.EncodeToString([]byte(raw))

	s := testServer(t)
	s.ready = true
	server := httptest.NewServer(s.handler())
	t.Cleanup(server.Close)
	s.origin = server.URL
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/clipboard", http.Header{"Origin": {s.origin}, "Cookie": {s.cookieName() + "=" + s.session}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	texts := []string{"before 中文", "配对码 " + raw + " 请勿外传", "请输入\n" + manual + "\n", manual, "after"}
	for serial, text := range texts {
		if err := ws.WriteJSON(map[string]any{"text": text, "serial": serial + 1}); err != nil {
			t.Fatal(err)
		}
		for {
			var event map[string]any
			if err := ws.ReadJSON(&event); err != nil {
				t.Fatalf("%q: %v", text, err)
			}
			if event["ack"] == float64(serial+1) {
				break
			}
			if event["hello"] == nil {
				t.Fatalf("%q: %v", text, event)
			}
		}
	}
	got, _ := os.ReadFile(received)
	if !strings.Contains(string(got), "before 中文") || !strings.Contains(string(got), `"after"`) || strings.Contains(string(got), "CAGE") {
		t.Fatalf("the guest received:\n%s", got)
	}
}
