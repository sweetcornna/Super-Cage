//go:build !windows

package workbench

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHeldListSeesAGuestChangeMadeDuringAnotherLook(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	for range 3 {
		phoneList(t, s, "")
	}
	_, current := phoneList(t, s, "")
	type result struct {
		code  int
		title string
	}
	held := make(chan result, 1)
	started := time.Now()
	go func() {
		code, answer := phoneList(t, s, "?after="+current.Revision+"&wait=20")
		title := ""
		for _, session := range answer.Sessions {
			if session.ID == id {
				title = session.Title
			}
		}
		held <- result{code, title}
	}()
	// The held list's 5 s look at Codex sessions waits behind a slow guest
	// read; the Claude change the watch reports meanwhile must not be lost.
	time.Sleep(time.Until(started.Add(4500 * time.Millisecond)))
	lockConversations(t, s, 3*time.Second)
	time.Sleep(time.Until(started.Add(5500 * time.Millisecond)))
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "renamed during the Codex look"})
	select {
	case got := <-held:
		if got.code != http.StatusOK || got.title != "renamed during the Codex look" {
			t.Fatalf("held list answered %d with title %q", got.code, got.title)
		}
		if elapsed := time.Since(started); elapsed > 11*time.Second {
			t.Fatalf("the change arrived after %v", elapsed)
		} else {
			t.Logf("the change arrived %v after the request", elapsed)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the held list did not answer")
	}
}
