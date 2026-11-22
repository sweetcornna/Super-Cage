//go:build !windows

package workbench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// lockConversations holds the conversation lock for the given time, as a slow
// discovery or session read does on a guest whose commands take long.
func lockConversations(t *testing.T, s *Server, hold time.Duration) {
	t.Helper()
	locked := make(chan struct{})
	released := make(chan struct{})
	go func() {
		s.conversationMu.Lock()
		close(locked)
		time.Sleep(hold)
		s.conversationMu.Unlock()
		close(released)
	}()
	<-locked
	t.Cleanup(func() {
		<-released
		// Let the looks that waited for the lock finish before the
		// fixture's directories go.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			s.cliDiscovery.mu.Lock()
			running := len(s.cliDiscovery.flights)
			s.cliDiscovery.mu.Unlock()
			if running == 0 && s.conversationMu.TryLock() {
				s.conversationMu.Unlock()
				break
			}
		}
	})
}

func TestHeldSessionListAnswersAtItsDeadlineWhileTheGuestIsSlow(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	t.Cleanup(s.cliDiscovery.resident.close)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	observe := observedGuest(t, s, home)
	observe([]terminalConversation{{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "guest", UpdatedAt: 1700000000}})
	s.ready = true
	sessionList(t, s, "")
	_, current := sessionList(t, s, "")
	// The guest observation cannot finish within the wait: the request still
	// answers at its deadline, from what the server already knows.
	lockConversations(t, s, 4*time.Second)
	started := time.Now()
	code, answer := sessionList(t, s, "?after="+current.Revision+"&wait=1")
	elapsed := time.Since(started)
	if code != http.StatusNoContent || elapsed > 1800*time.Millisecond {
		t.Fatalf("held list answered %d %+v after %v; want 204 at its 1 s deadline", code, answer, elapsed)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("held list answered after %v, before its deadline", elapsed)
	}
	// The cut look is not an error: once the guest answers, lists are as before.
	time.Sleep(3500 * time.Millisecond)
	if code, answer := sessionList(t, s, "?after="+current.Revision); code != http.StatusNoContent {
		t.Fatalf("after the slow look: %d %+v", code, answer)
	}
}

func TestHeldSessionListLookInsideTheHoldEndsAtTheDeadline(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	_, current := sessionList(t, s, "")
	type result struct {
		code    int
		elapsed time.Duration
	}
	held := make(chan result, 1)
	started := time.Now()
	go func() {
		code, _ := sessionList(t, s, "?after="+current.Revision+"&wait=3")
		held <- result{code, time.Since(started)}
	}()
	time.Sleep(500 * time.Millisecond)
	// The watch reports the change, and the discovery it starts waits behind a
	// slow guest read for longer than the hold has left.
	lockConversations(t, s, 5*time.Second)
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "named while the guest is slow"})
	select {
	case got := <-held:
		if got.code != http.StatusNoContent || got.elapsed > 3800*time.Millisecond {
			t.Fatalf("held list answered %d after %v; want 204 at its 3 s deadline", got.code, got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held list did not answer")
	}
}

func TestPlainSessionListAnswersWithinTheLookLimit(t *testing.T) {
	previous := sessionLookLimit
	t.Cleanup(func() { sessionLookLimit = previous })
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	t.Cleanup(s.cliDiscovery.resident.close)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	observe := observedGuest(t, s, home)
	observe([]terminalConversation{{CLIID: record.CLIID, Workspace: guestPath(project), Config: filepath.Join(home, ".claude"), Title: "guest", UpdatedAt: 1700000000}})
	s.ready = true
	// The first list sees the guest in full time; only the second is cut.
	_, first := sessionList(t, s, "")
	if len(first.Sessions) != 1 || first.Sessions[0].Title != "guest" {
		t.Fatalf("the first list did not see the guest: %+v", first)
	}
	sessionLookLimit = time.Second
	lockConversations(t, s, 4*time.Second)
	started := time.Now()
	code, answer := sessionList(t, s, "")
	if elapsed := time.Since(started); code != http.StatusOK || elapsed > 1800*time.Millisecond {
		t.Fatalf("list answered %d after %v; want an answer within the 1 s look limit", code, elapsed)
	}
	// The answer is the latest finished observation, not an error notice.
	if answer.Notice != "" || answer.Revision != first.Revision {
		t.Fatalf("a cut list = %+v, want the earlier answer %+v", answer, first)
	}
}

func TestHeldSessionReadAnswersAtItsDeadlineWhileTheGuestIsSlow(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "question"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	read := func(query string) (int, mobileSession) {
		w := request(s, "GET", "/api/mobile/cli/sessions/"+id+query, s.origin, "", true)
		var view mobileSession
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, view
	}
	read("")
	_, view := read("")
	query := fmt.Sprintf("?after=%d&epoch=%s", view.Revision, view.Generation)
	type result struct {
		code    int
		elapsed time.Duration
	}
	held := make(chan result, 1)
	started := time.Now()
	go func() {
		code, _ := read(query + "&wait=3")
		held <- result{code, time.Since(started)}
	}()
	time.Sleep(500 * time.Millisecond)
	// The guest reply wakes the held read, and its look then waits behind a
	// slow guest read for longer than the hold has left.
	lockConversations(t, s, 5*time.Second)
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "assistant", "uuid": uuid.NewString(), "message": map[string]any{"content": []map[string]string{{"type": "text", "text": "answer"}}}})
	select {
	case got := <-held:
		if got.code != http.StatusNoContent || got.elapsed > 3800*time.Millisecond {
			t.Fatalf("held read answered %d after %v; want 204 at its 3 s deadline", got.code, got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held read did not answer")
	}
	// The look cut at the deadline is not held against the resident helper.
	time.Sleep(5 * time.Second)
	if !s.cliDiscovery.usable() {
		t.Fatal("a look cut at a held read's deadline disabled the resident helper")
	}
	if code, view := read(query); code != http.StatusOK || len(view.Turns) != 1 || view.Turns[0].Text != "answer" {
		t.Fatalf("after the slow look: %d %+v", code, view.Turns)
	}
}

func TestHeldSessionReadsFirstLookFinishesBeyondAShortWait(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "question"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	w := request(s, "GET", "/api/mobile/cli/sessions/"+id, s.origin, "", true)
	var view mobileSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "assistant", "uuid": uuid.NewString(), "message": map[string]any{"content": []map[string]string{{"type": "text", "text": "answer"}}}})
	// A slow first read still finishes past a 1 s wait and reports the
	// change: cutting it would leave a short wait on a slow guest seeing none.
	lockConversations(t, s, 2*time.Second)
	w = request(s, "GET", fmt.Sprintf("/api/mobile/cli/sessions/%s?after=%d&epoch=%s&wait=1", id, view.Revision, view.Generation), s.origin, "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("held read answered %d; want the change its first read found", w.Code)
	}
}
