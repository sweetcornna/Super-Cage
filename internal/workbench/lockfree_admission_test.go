//go:build !windows

package workbench

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
)

func setGuestReady(s *Server, ready bool) {
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
}

// Science status polls are lock-free guest calls: while a job runs, or the
// guest is not ready, they answer the last state read with the job's
// operation and do not ask the guest.
func TestClaudeSciencePollsWaitOutJobs(t *testing.T) {
	withScienceHostArch(t, "amd64")
	s := testServer(t)
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if !s.vmRunning() {
		t.Fatal("the fixture VM does not run")
	}
	setGuestReady(s, true)
	pinned := cage.ClaudeSciencePin().Version
	home, _ := filepath.EvalSymlinks(t.TempDir())
	_, env := scienceGuest(t, home, scienceSnapshotCase{arch: "x86_64", unit: "active", installed: true, listener: true, invocation: "run-1",
		status: `{"running":true,"port":47800,"version":"` + pinned + `","started_at":"2026-11-20T09:30:00.000Z"}`})
	mobileTestSSH(t, s, home, env)
	t.Cleanup(s.controlSession.close)
	snapshots := func() int {
		data, _ := os.ReadFile(filepath.Join(home, "snapshot-runs"))
		return strings.Count(string(data), "x")
	}
	var body string
	poll := func(want int) claudeScienceState {
		t.Helper()
		w := request(s, "GET", "/api/claude-science", "", "", true)
		if body = w.Body.String(); w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body)
		}
		if want != http.StatusOK {
			return claudeScienceState{}
		}
		return decodeScienceState(t, w.Body.Bytes())
	}
	// Nothing was read yet: a poll during a job says so and waits.
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: true})
	if poll(http.StatusConflict); !strings.Contains(body, "其他操作") {
		t.Fatalf("a poll during a job: %s", body)
	}
	if snapshots() != 0 {
		t.Fatal("a poll during a job asked the guest")
	}
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: false})
	if state := poll(http.StatusOK); !state.Ready || state.Operation != "" {
		t.Fatalf("%+v", state)
	}
	if snapshots() != 1 {
		t.Fatalf("%d snapshots for one poll", snapshots())
	}
	s.setJobForTest(Job{ID: "update", Kind: "claude-science-update", Running: true})
	if state := poll(http.StatusOK); !state.Ready || state.Operation != "update" || state.Version != pinned {
		t.Fatalf("during the Science job: %+v", state)
	}
	s.setJobForTest(Job{ID: "tools", Kind: "environment-tools", Running: true})
	if state := poll(http.StatusOK); !state.Ready || state.Operation != "" {
		t.Fatalf("during another job: %+v", state)
	}
	s.setJobForTest(Job{ID: "tools", Kind: "environment-tools", Running: false})
	setGuestReady(s, false)
	poll(http.StatusOK)
	if snapshots() != 1 {
		t.Fatalf("polls asked the guest %d more times during jobs or while it was not ready", snapshots()-1)
	}
	setGuestReady(s, true)
	poll(http.StatusOK)
	if snapshots() != 2 {
		t.Fatal("a poll after the job did not read the guest")
	}
	// A guest that is not ready yet, with nothing read, is named as such.
	s.scienceLast.set(nil)
	setGuestReady(s, false)
	if poll(http.StatusConflict); !strings.Contains(body, "尚未就绪") || snapshots() != 2 {
		t.Fatalf("a poll before the guest was ready: %s", body)
	}
}

// The viewer's own display change goes to the guest at once; its retries wait
// while a job owns the guest and resume after it.
func TestDesktopVisibilityRetriesWaitForTheGuest(t *testing.T) {
	s := testServer(t)
	setGuestReady(s, true)
	var calls atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	s.desktopDisplayUpdate = func(bool) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("the guest is busy")
		}
		return nil
	}
	s.desktopViews.mu.Lock()
	s.desktopViews.retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	s.desktopViews.mu.Unlock()
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: true})
	viewer := s.joinDesktopView()
	defer viewer.close()
	eventually(t, "the viewer's own change", time.Second, func() bool { return calls.Load() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d retries reached the guest while a job owned it", n-1)
	}
	failing.Store(false)
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: false})
	eventually(t, "the display shown after the job", 2*time.Second, func() bool {
		s.desktopViews.op.Lock()
		defer s.desktopViews.op.Unlock()
		return s.desktopViews.known && s.desktopViews.shown
	})
}

// A job that begins while held lists rely on the guest watch pauses the watch
// without sending the lists to look at the guest themselves; after the job the
// watch catches up with what changed meanwhile.
func TestAJobPausesTheGuestWatchWithoutSendingListsToTheGuest(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, logins, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl")
	writeTranscript(t, history, map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	setGuestReady(s, true)
	for range 3 {
		phoneList(t, s, "")
	}
	changes, stop := s.subscribeGuestChanges() // as a held list does
	t.Cleanup(stop)
	eventually(t, "the watch to answer", 10*time.Second, func() bool { _, _, watching := changes(); return watching })
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: true})
	// The watch sent before the job reports this change.
	_, before, _ := changes()
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "before the pause"})
	eventually(t, "the change the watch saw", 10*time.Second, func() bool { _, at, _ := changes(); return at.After(before) })
	_, reported, _ := changes()
	started := logins()
	// Then it pauses and keeps its word: held lists do not look themselves.
	writeTranscript(t, history, map[string]any{"type": "custom-title", "customTitle": "during the job"})
	time.Sleep(2 * time.Second)
	if _, at, watching := changes(); !watching || at.After(reported) || logins() != started {
		t.Fatalf("during the job: watching %v, a change reported %v, %d logins", watching, at.After(reported), logins()-started)
	}
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: false})
	eventually(t, "the watch to catch up after the job", 10*time.Second, func() bool { _, at, _ := changes(); return at.After(reported) })
}

// The guest watch is a lock-free guest call too: while a job owns the guest it
// does not start the resident helper or send a watch.
func TestGuestWatchWaitsWhileAJobOwnsTheGuest(t *testing.T) {
	s := residentRunning(t)
	s.cliDiscovery.mu.Lock()
	input := s.cliDiscovery.lastInput
	s.cliDiscovery.mu.Unlock()
	if input == nil {
		t.Fatal("no discovery to watch")
	}
	s.cliDiscovery.resident.close()
	s.setJobForTest(Job{ID: "job", Kind: "environment-tools", Running: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchGuestSessions(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		s.cliDiscovery.resident.close()
	})
	time.Sleep(1500 * time.Millisecond)
	if !residentClosed(s) {
		t.Fatal("the watch started the resident helper while a job owned the guest")
	}
}
