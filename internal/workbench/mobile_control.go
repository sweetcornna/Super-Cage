package workbench

import "net/http"

// Phone control of the environment, its terminals and project files. Every
// route is also listed in mobilelink's allowlist and in pairedDevice.
func (s *Server) mobileControlRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/mobile/environment", s.mobileEnvironment)
	mux.HandleFunc("POST /api/mobile/environment/start", s.mobileEnvironmentStart)
	mux.HandleFunc("POST /api/mobile/environment/stop", s.mobileEnvironmentStop)
	mux.HandleFunc("POST /api/mobile/environment/restart", s.mobileEnvironmentRestart)
	mux.HandleFunc("GET /api/mobile/environment/proxy", s.mobileProxy)
	mux.HandleFunc("POST /api/mobile/environment/proxy/select", s.mobileProxySelect)
	mux.HandleFunc("POST /api/mobile/environment/proxy/delay", s.mobileProxyDelay)
	mux.HandleFunc("GET /api/mobile/terminals", s.mobileTerminals)
	mux.HandleFunc("POST /api/mobile/terminals", s.mobileNewTerminal)
	mux.HandleFunc("GET /api/mobile/terminals/{id}/output", s.mobileTerminalOutput)
	mux.HandleFunc("POST /api/mobile/terminals/{id}/input", s.mobileTerminalInput)
	mux.HandleFunc("POST /api/mobile/terminals/{id}/resize", s.mobileTerminalResize)
	mux.HandleFunc("POST /api/mobile/terminals/{id}/close", s.mobileCloseTerminal)
	mux.HandleFunc("POST /api/mobile/files/list", s.mobileFileList)
	mux.HandleFunc("POST /api/mobile/files/read", s.mobileFileRead)
	mux.HandleFunc("POST /api/mobile/files/mkdir", s.mobileFileMkdir)
	mux.HandleFunc("POST /api/mobile/files/upload/start", s.mobileUploadStart)
	mux.HandleFunc("POST /api/mobile/files/upload/chunk", s.mobileUploadChunk)
	mux.HandleFunc("POST /api/mobile/files/upload/finish", s.mobileUploadFinish)
	mux.HandleFunc("POST /api/mobile/files/upload/abort", s.mobileUploadAbort)
}

// mobileAllowed re-checks, before a change, the unlock that admitted this
// phone request. An operation begun under an old unlock gains nothing later.
func (s *Server) mobileAllowed(w http.ResponseWriter, r *http.Request) bool {
	s.account.mu.Lock()
	ok := s.mobileAuthorized(r.Context())
	s.account.mu.Unlock()
	if !ok {
		http.Error(w, errMobileLocked.Error(), http.StatusLocked)
	}
	return ok
}
