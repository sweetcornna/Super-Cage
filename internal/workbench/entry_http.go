package workbench

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
)

// The entry's routes sit behind the workbench's session, Origin and
// account-lock checks like every route, and in addition answer only the
// trusted local view (the desktop window on this computer or a cage command
// that read the launcher record): they write this computer's ~/.ssh and
// device key, which a phone or a remote desktop must never do.
func (s *Server) entryRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/entry", s.entryLocal(s.entryStatus))
	mux.HandleFunc("POST /api/entry/on", s.entryLocal(s.entryOn))
	mux.HandleFunc("POST /api/entry/off", s.entryLocal(s.entryOff))
	mux.HandleFunc("GET /api/entry/include", s.entryLocal(s.entryIncludeRoute))
	mux.HandleFunc("POST /api/entry/include", s.entryLocal(s.entryIncludeRoute))
	mux.HandleFunc("POST /api/entry/devices/{id}/revoke", s.entryLocal(s.entryRevoke))
	mux.HandleFunc("POST /api/entry/devices/{id}/rename", s.entryLocal(s.entryRename))
	mux.HandleFunc("GET /api/entry/ssh", s.entrySSH)
	mux.HandleFunc("POST /api/terminals/{id}/keep", s.keepTerminal)
}

func (s *Server) entryLocal(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.desktopEntry || s.clientOnly {
			http.NotFound(w, r)
			return
		}
		if !s.hasTrustedView(r) {
			entryRefuse(w, errEntryLocalOnly)
			return
		}
		handler(w, r)
	}
}

type entryDeviceView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Created     time.Time  `json:"created"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
	Revoked     bool       `json:"revoked"`
	Current     bool       `json:"current"`
}

type entryCommands struct {
	SSH     string `json:"ssh"`
	CageSSH string `json:"cage_ssh"`
	Attach  string `json:"attach,omitempty"`
	// Cage is true when the cage command is on PATH; the commands then use
	// its short name.
	Cage bool `json:"cage_on_path"`
}

type entryStatusView struct {
	Enabled     bool   `json:"enabled"`
	Supported   bool   `json:"supported"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
	// State is off, idle, in_use, paused, stopped, starting or unsupported;
	// a locked Super Cage answers 423 before any of this.
	State       string            `json:"state"`
	HostAlias   string            `json:"host_alias,omitempty"`
	Connections entryConnections  `json:"connections"`
	Commands    *entryCommands    `json:"commands,omitempty"`
	Include     *entryInclude     `json:"include,omitempty"`
	Computer    string            `json:"computer,omitempty"`
	Revoked     bool              `json:"revoked"`
	Devices     []entryDeviceView `json:"devices"`
	// Herdr is offered only once its filter exists (step 3).
	Herdr    bool               `json:"herdr"`
	Settings entryLocalSettings `json:"settings"`
	Error    string             `json:"error,omitempty"`
}

// entryView reads everything the page shows and writes nothing.
func (s *Server) entryView() entryStatusView {
	view := entryStatusView{Supported: s.macosGuest == nil && s.windowsGuest == nil, Environment: entryEnvironmentID(s.profile), Name: entryEnvironmentName(s.profile), Devices: []entryDeviceView{}}
	paths, err := s.entryPaths()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	view.Settings = entryLoadLocalSettings(paths)
	st, err := entryLoadState(s.profile)
	if err != nil {
		view.Error = err.Error()
	}
	computer, _, _ := entryReadComputer(paths)
	view.Computer = computer.ID
	for _, device := range st.Devices {
		view.Devices = append(view.Devices, entryDeviceView{ID: device.ID, Name: device.Name, Fingerprint: entryFingerprint(device.PublicKey), Created: device.Created, LastUsed: device.LastUsed, Revoked: device.Revoked != nil, Current: device.ID == computer.ID})
		if device.ID == computer.ID && device.Revoked != nil {
			view.Revoked = true
		}
	}
	view.Enabled = st.Enabled
	view.Connections = s.entry.registry.counts()
	s.mu.Lock()
	ready, paused := s.ready, s.networkPaused
	s.mu.Unlock()
	switch {
	case !view.Supported:
		view.State = "unsupported"
	case !st.Enabled:
		view.State = "off"
	case !s.vmRunning():
		view.State = "stopped"
	case !ready:
		view.State = "starting"
	case paused:
		view.State = "paused"
	case view.Connections.Total > 0:
		view.State = "in_use"
	default:
		view.State = "idle"
	}
	if st.Enabled {
		view.HostAlias = entryReadAlias(paths.hostFile(view.Environment))
		include := entryIncludePreview(paths)
		view.Include = &include
		cage := paths.Cage
		onPath := false
		if found, err := lookPathCage(); err == nil && sameFile(found, paths.Cage) {
			cage, onPath = "cage", true
		} else if runtime.GOOS == "windows" {
			cage = "& " + entryQuote(cage)
		} else {
			cage = shellescape.Quote(cage)
		}
		// HOOK(attach): Attach stays empty, and the page hides its row,
		// until cage attach (branch local-terminal-attach) is merged; then
		// set it to cage + " attach " + view.Environment.
		view.Commands = &entryCommands{SSH: "ssh " + view.HostAlias, CageSSH: cage + " ssh " + view.Environment, Cage: onPath}
	}
	return view
}

// lookPathCage finds a cage command on the controller's PATH.
var lookPathCage = func() (string, error) { return exec.LookPath("cage") }

func sameFile(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

func (s *Server) entryStatus(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, s.entryView())
}

// entryOn turns the entry on for this environment on this computer: the
// device key (created once per computer, renewed after a revocation), the
// endpoint's host key, the device's enrolment and the generated Host block.
// It never writes ~/.ssh/config; that is the separate include step.
func (s *Server) entryOn(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest != nil || s.windowsGuest != nil {
		entryRefuse(w, errEntryUnsupported)
		return
	}
	paths, err := s.entryPaths()
	if err != nil {
		requestError(w, err)
		return
	}
	environment, name := entryEnvironmentID(s.profile), entryEnvironmentName(s.profile)
	err = func() error {
		s.entry.mu.Lock()
		defer s.entry.mu.Unlock()
		st, err := entryLoadState(s.profile)
		if err != nil {
			st = entryState{Version: entryStateVersion}
		}
		current, known, _ := entryReadComputer(paths)
		rotate := known && st.device(current.ID) != nil && st.device(current.ID).Revoked != nil
		computer, err := entryEnsureComputer(paths, rotate)
		if err != nil {
			return err
		}
		signer, err := entryHostSigner(s.profile, true)
		if err != nil {
			return err
		}
		if record := st.device(computer.ID); record != nil {
			record.PublicKey, record.Revoked = computer.Public, nil
		} else {
			st.Devices = append(st.Devices, entryDevice{ID: computer.ID, Name: computer.Name, PublicKey: computer.Public, Created: time.Now().UTC()})
		}
		st.Enabled = true
		if err := entrySaveState(s.profile, st); err != nil {
			return err
		}
		_, err = entryWriteClient(paths, environment, name, signer.PublicKey())
		return err
	}()
	if err != nil {
		requestError(w, errors.New("无法开启本机终端："+err.Error()))
		return
	}
	// Put the guest's run script in place now when the environment runs, so
	// the first scp does not wait for it; a failure here is retried then.
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready && s.vmRunning() {
		ctx, cancel := context.WithTimeout(r.Context(), entryWrapperTimeout)
		_ = s.entryEnsureWrapper(ctx)
		cancel()
	}
	jsonReply(w, s.entryView())
}

// entryOff cuts every connection, refuses the next dial and removes the Host
// block; with no block left on this computer the Include line goes too.
func (s *Server) entryOff(w http.ResponseWriter, r *http.Request) {
	paths, err := s.entryPaths()
	if err != nil {
		requestError(w, err)
		return
	}
	s.entry.mu.Lock()
	st, err := entryLoadState(s.profile)
	if err == nil && !st.Enabled {
		s.entry.mu.Unlock()
		entryRefuse(w, errEntryOff)
		return
	}
	st.Enabled = false
	err = entrySaveState(s.profile, st)
	s.entry.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	s.entry.registry.cutAll(errEntryOff)
	if err := entryRemoveClient(paths, entryEnvironmentID(s.profile)); err != nil {
		requestError(w, errors.New("本机终端已关闭，但 ssh 配置没能全部移除："+err.Error()))
		return
	}
	jsonReply(w, s.entryView())
}

func (s *Server) entryEnabled() bool {
	st, err := entryLoadState(s.profile)
	return err == nil && st.Enabled
}

// entryIncludeRoute is the confirmation step for the Include line: GET shows
// the exact line and the file it goes into, POST writes it with the token
// that GET returned.
//
// The preview answers while the entry is off too: the dialog that turns it on
// shows the line first, and reading ~/.ssh/config writes nothing.
func (s *Server) entryIncludeRoute(w http.ResponseWriter, r *http.Request) {
	paths, err := s.entryPaths()
	if err != nil {
		requestError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		jsonReply(w, entryIncludePreview(paths))
		return
	}
	if !s.entryEnabled() {
		entryRefuse(w, errEntryOff)
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	s.entry.mu.Lock()
	view, err := entryIncludeApply(paths, input.Token)
	s.entry.mu.Unlock()
	if errors.Is(err, errEntryIncludeChanged) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		requestError(w, errors.New("无法写入 ~/.ssh/config："+err.Error()+"。你仍可以用 cage ssh 连接。"))
		return
	}
	jsonReply(w, view)
}

// entryRevoke cuts the device's connections at once and refuses it until
// the entry is turned on again from that computer, which renews its key.
func (s *Server) entryRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.entry.mu.Lock()
	st, err := entryLoadState(s.profile)
	if err == nil && !st.Enabled {
		s.entry.mu.Unlock()
		entryRefuse(w, errEntryOff)
		return
	}
	record := st.device(id)
	if err != nil || record == nil {
		s.entry.mu.Unlock()
		http.Error(w, "没有这台设备", http.StatusNotFound)
		return
	}
	if record.Revoked == nil {
		now := time.Now().UTC()
		record.Revoked = &now
		err = entrySaveState(s.profile, st)
	}
	s.entry.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	s.entry.registry.cutDevice(id, errEntryRevoked)
	jsonReply(w, s.entryView())
}

func validEntryDeviceName(name string) bool {
	length := utf8.RuneCountInString(name)
	return length >= 1 && length <= 64 && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) == -1
}

func (s *Server) entryRename(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validEntryDeviceName(input.Name) {
		http.Error(w, "设备名称需要 1–64 个字符，不能包含控制字符", http.StatusBadRequest)
		return
	}
	s.entry.mu.Lock()
	st, err := entryLoadState(s.profile)
	if err == nil && !st.Enabled {
		s.entry.mu.Unlock()
		entryRefuse(w, errEntryOff)
		return
	}
	record := st.device(r.PathValue("id"))
	if err != nil || record == nil {
		s.entry.mu.Unlock()
		http.Error(w, "没有这台设备", http.StatusNotFound)
		return
	}
	record.Name = input.Name
	err = entrySaveState(s.profile, st)
	s.entry.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, s.entryView())
}

// keepTerminal is "保留" on a terminal an ssh session opened: it stays when
// the connection ends, as an ordinary terminal.
func (s *Server) keepTerminal(w http.ResponseWriter, r *http.Request) {
	session := s.terminals.get(r.PathValue("id"))
	if session == nil {
		http.NotFound(w, r)
		return
	}
	session.mu.Lock()
	ssh := session.origin == "ssh"
	if ssh {
		session.kept = true
	}
	session.mu.Unlock()
	if !ssh {
		http.Error(w, "只有来自本机终端的终端需要保留", http.StatusConflict)
		return
	}
	jsonReply(w, session.summary())
}
