package workbench

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// launchIdentity is this controller's identity with the cage-attach beside
// its executable, verified once per file (controlclient.CompanionOf), so the
// slim program can prove it is the one built and installed with this cage.
func (s *Server) launchIdentity() ControllerIdentity {
	id := s.identity
	exe := s.exe
	if exe == "" || !filepath.IsAbs(exe) {
		var err error
		if exe, err = os.Executable(); err != nil {
			return id
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	id.Attach = controlclient.CompanionOf(filepath.Join(filepath.Dir(exe), controlclient.AttachProgram(runtime.GOOS)))
	return id
}
