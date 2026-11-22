package workbench

import (
	"context"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

// installTestDevice adds a paired device whose current room is room and whose
// room proof is proof, as the serving loop does once a device authenticated.
// No loop runs. cancel, if given, is what removing the device or stopping the
// service calls.
func installTestDevice(s *Server, room context.Context, cancel context.CancelFunc, proof string) *pairedDevice {
	if cancel == nil {
		cancel = func() {}
	}
	seed := make([]byte, 32)
	copy(seed, proof)
	d := &pairedDevice{deviceRecord: deviceRecord{ID: newDeviceID(), Kind: "phone", AddedAt: time.Now().UTC(), Seed: seed, Relay: mobilelink.OfficialRelay}, cancel: cancel, proof: proof, roomCtx: room, connected: true}
	s.phones.mu.Lock()
	s.phones.devices = append(s.phones.devices, d)
	s.phones.mu.Unlock()
	return d
}

// clearTestDevices forgets every device without stopping anything.
func clearTestDevices(s *Server) {
	s.phones.mu.Lock()
	s.phones.devices = nil
	s.phones.mu.Unlock()
}

// onlyDevice is the single paired device of s.
func onlyDevice(t *testing.T, s *Server) *pairedDevice {
	t.Helper()
	s.phones.mu.Lock()
	defer s.phones.mu.Unlock()
	if len(s.phones.devices) != 1 {
		t.Fatalf("want one paired device, have %d", len(s.phones.devices))
	}
	return s.phones.devices[0]
}

// liveProof is the room proof of the single device's current room.
func liveProof(t *testing.T, s *Server) string {
	t.Helper()
	d := onlyDevice(t, s)
	s.phones.mu.Lock()
	defer s.phones.mu.Unlock()
	return d.proof
}
