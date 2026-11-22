//go:build !windows

package workbench

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// residentRunning starts the resident discovery helper of a fixture guest.
func residentRunning(t *testing.T) *Server {
	t.Helper()
	s, id, project := metadataFixture(t)
	home, _, _ := residentGuest(t, s)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "fixture", record.CLIID+".jsonl"), map[string]any{"cwd": guestPath(project), "type": "user", "uuid": uuid.NewString(), "message": map[string]string{"content": "guest title"}})
	s.ready = true
	sessionList(t, s, "")
	sessionList(t, s, "")
	s.cliDiscovery.resident.mu.Lock()
	open := s.cliDiscovery.resident.conn != nil
	s.cliDiscovery.resident.mu.Unlock()
	if !open {
		t.Fatal("the resident helper did not start")
	}
	return s
}

func residentClosed(s *Server) bool {
	s.cliDiscovery.resident.mu.Lock()
	defer s.cliDiscovery.resident.mu.Unlock()
	return s.cliDiscovery.resident.conn == nil
}

func TestEnvironmentStartClosesTheResidentHelper(t *testing.T) {
	s := residentRunning(t)
	// A back-off the old guest earned does not hold up the new one.
	s.cliDiscovery.mu.Lock()
	s.cliDiscovery.retryAt = time.Now().Add(time.Minute)
	s.cliDiscovery.mu.Unlock()
	s.scienceLast.set(&claudeScienceState{Supported: true, Running: true, Ready: true, Port: 47800})
	// The start goes no further than the closes in this fixture.
	s.storageRecoveryError = "fixture"
	_ = s.startEnvironment(false)
	if !residentClosed(s) {
		t.Fatal("a start left the resident helper's connection to the old guest open")
	}
	if _, kept := s.scienceLast.get(); kept {
		t.Fatal("a start kept the old guest's Claude Science state for status polls")
	}
	if !s.cliDiscovery.usable() {
		t.Fatal("a start kept the resident helper's back-off")
	}
}

func TestEnvironmentStopClosesTheGuestSessions(t *testing.T) {
	s := residentRunning(t)
	s.controlSession.mu.Lock()
	control := s.controlSession.epoch
	s.controlSession.mu.Unlock()
	s.scienceLast.set(&claudeScienceState{Supported: true, Running: true, Ready: true, Port: 47800})
	if err := s.stopEnvironment(); err != nil {
		t.Fatal(err)
	}
	if !residentClosed(s) {
		t.Fatal("a stop left the resident helper's connection open")
	}
	if _, kept := s.scienceLast.get(); kept {
		t.Fatal("a stop kept the old guest's Claude Science state for status polls")
	}
	s.controlSession.mu.Lock()
	closed := s.controlSession.epoch != control
	s.controlSession.mu.Unlock()
	if !closed {
		t.Fatal("a stop left the control session open")
	}
}
