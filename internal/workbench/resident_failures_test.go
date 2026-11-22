//go:build !windows

package workbench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
)

func TestResidentRequestErrorKeepsTheHelper(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	// The next CLI listing fails once, as claude agents --json can.
	failing := "#!/bin/sh\n[ \"$1\" = agents ] || exit 64\nif [ -e \"$HOME/fail\" ]; then rm -f \"$HOME/fail\"; exit 1; fi\nprintf '[]\\n'\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "claude"), []byte(failing), 0700); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	if logins() != 2 {
		t.Fatalf("%d logins to start the resident helper", logins())
	}
	if err := os.WriteFile(filepath.Join(home, "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// A new registered process makes the resident helper list again.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions", "1.json"), []byte(`{"pid":1,"sessionId":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// This request fails in the resident helper and is answered by the
	// channel helper; the helper itself stays in use for the next ones.
	if code, answer := sessionList(t, s, ""); code != http.StatusOK || len(answer.Sessions) != 1 || answer.Notice != "" {
		t.Fatalf("list after a failed listing: %d %+v", code, answer)
	}
	if logins() != 3 {
		t.Fatalf("%d logins: want one channel helper run for the failed request", logins())
	}
	if !s.cliDiscovery.usable() {
		t.Fatal("one failed request disabled the resident helper")
	}
	for range 2 {
		if code, _ := sessionList(t, s, ""); code != http.StatusOK {
			t.Fatal(code)
		}
	}
	if logins() != 3 {
		t.Fatalf("lists after a failed request opened %d more logins", logins()-3)
	}
}

// While the resident helper backs off (a stalled guest timed it out), every
// guest look is a login. They are bounded by the conversation lock, which
// runs one at a time per workbench, by the pace of the held loops (a list
// looks at the guest every 5 s, a read every 1 s or 4 s, as the clients
// polled), by share (concurrent lists cost at most two discoveries per
// trigger), and by the requests' own time: a look left waiting past its
// deadline starts none.
func TestFallbackLoginsAreSerializedAndPaced(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	_, current := sessionList(t, s, "")
	w := request(s, "GET", "/api/cli/sessions/"+id, s.origin, "", true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var view mobileSession
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// A slow guest: each login takes 300 ms; overlapping logins are recorded.
	profile := "if ! mkdir \"$HOME/login.lock\" 2>/dev/null; then echo overlap >> \"$HOME/overlaps\"; fi\nsleep 0.3\nrmdir \"$HOME/login.lock\" 2>/dev/null\necho login >> \"$HOME/logins\"\nexport PATH=" + shellescape.Quote(filepath.Join(home, "bin")) + ":/usr/bin:/bin\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	s.cliDiscovery.failed()
	before := logins()
	const hold = 6
	var group sync.WaitGroup
	for range 3 {
		group.Add(2)
		go func() {
			defer group.Done()
			sessionList(t, s, fmt.Sprintf("?after=%s&wait=%d", current.Revision, hold))
		}()
		go func() {
			defer group.Done()
			request(s, "GET", fmt.Sprintf("/api/cli/sessions/%s?after=%d&epoch=%s&wait=%d", id, view.Revision, view.Generation, hold), s.origin, "", true)
		}()
	}
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			sessionList(t, s, "")
		}()
	}
	group.Wait()
	// Looks that waited for the lock may still be finishing.
	time.Sleep(time.Second)
	if data, _ := os.ReadFile(filepath.Join(home, "overlaps")); len(data) > 0 {
		t.Fatalf("fallback logins overlapped %d times", strings.Count(string(data), "overlap"))
	}
	// Lists: at most two discoveries for the arrival and two for the 5 s
	// look; reads: each one first look and one at the 4 s pace; plain
	// lists: at most two.
	if spent, limit := logins()-before, 2+2+3*2+2; spent > limit {
		t.Fatalf("%d fallback logins in %d s, want at most %d", spent, hold, limit)
	} else {
		t.Logf("%d fallback logins in %d s for three held lists, three held reads and two lists", spent, hold)
	}
	if s.cliDiscovery.usable() {
		t.Fatal("the resident helper was used again while it backs off")
	}
}
