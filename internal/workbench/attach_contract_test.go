package workbench

import (
	"testing"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
)

// cage-attach cannot import this package; what it repeats must agree.
func TestAttachAgreesWithTheServer(t *testing.T) {
	if attach.ViewerProtocol != terminalViewerProtocol || attach.MaxImageBytes != maxImageBytes || attach.ConfigEnv != cage.ConfigEnv || attach.WriteTimeout != terminalWriteTimeout {
		t.Fatal("cage attach and the workbench disagree on a limit")
	}
	// A live stream always speaks within the silence cage attach allows.
	if attach.Silence < 2*terminalHeartbeatInterval {
		t.Fatal("cage attach would drop live streams between heartbeats")
	}
	for cols := uint16(0); cols < 700; cols++ {
		for _, rows := range []uint16{0, 1, 2, 24, 399, 400, 401} {
			if attach.ValidSize(cols, rows) != validTerminalSize(cols, rows) || attach.ValidSize(rows, cols) != validTerminalSize(rows, cols) {
				t.Fatal("size rules differ at", cols, rows)
			}
		}
	}
}
