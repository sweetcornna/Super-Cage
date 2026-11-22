package workbench

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
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
	mux.HandleFunc("POST /api/terminals/{id}/keep", s.entryLocal(s.keepTerminal))
}

// agentRequestHeader marks the requests of the agent transport (AgentRequest,
// AgentSocket). Their callers cannot set it or leave it out, so the entry's
// routes can refuse them on the server as well.
const agentRequestHeader = "X-Cage-Agent"

func (s *Server) entryLocal(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.desktopEntry || s.clientOnly {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(agentRequestHeader) != "" {
			entryRefuse(w, errEntryWindowOnly)
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
	// Herdr is the Herdr row while the entry is on.
	Herdr *entryHerdrView `json:"herdr,omitempty"`
	Error string          `json:"error,omitempty"`
	// Unreached names the environments a revocation could not reach.
	Unreached []string `json:"unreached,omitempty"`
	// Kept are the kept ssh terminals of this environment, at most
	// KeptLimit, each with 结束 on the page.
	Kept      []entryKeptView `json:"kept"`
	KeptLimit int             `json:"kept_limit"`
}

// entryUnreachedNote is what the page and cage entry revoke say when a
// running environment did not answer a revocation in time.
func entryUnreachedNote(names []string) string {
	return "另一个环境暂时没有响应，已撤销的钥匙会在它下次连接时被拒绝（" + strings.Join(names, "、") + "）"
}

// entryView reads everything the page shows and writes nothing.
func (s *Server) entryView() entryStatusView {
	view := entryStatusView{Supported: s.macosGuest == nil && s.windowsGuest == nil, Environment: entryEnvironmentID(s.profile), Name: entryEnvironmentName(s.profile), Devices: []entryDeviceView{}}
	view.Kept, view.KeptLimit = s.entryKeptViews(), entryKeptLimit
	paths, err := s.entryPaths()
	if err != nil {
		view.Error = err.Error()
		return view
	}
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
		view.Commands = &entryCommands{SSH: "ssh " + view.HostAlias, CageSSH: cage + " ssh " + view.Environment, Cage: onPath}
		// cage attach opens a new terminal of the default environment in
		// the terminal it runs in. Not offered for a local VM: cage attach
		// --config <its profile> would read the per-computer settings
		// (local-terminal.json) beside that profile, not the ones the
		// desktop saves, so the copied command would not follow them.
		if view.Environment == defaultLocalVMID {
			view.Commands.Attach = cage + " attach"
		}
		view.Herdr = &entryHerdrView{State: "not_set_up", Lines: view.Connections.Herdr, Command: entryHerdrCommand(view.HostAlias, include.Present, paths.cageSSHFile())}
		switch {
		case view.Connections.Herdr > 0:
			view.Herdr.State = "connected"
		case st.HerdrSeen != nil:
			view.Herdr.State = "ready"
		}
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
		if _, err := entryLoadRevoked(paths); err != nil {
			return err
		}
		st, err := entryLoadState(s.profile)
		if err != nil {
			st = entryState{Version: entryStateVersion}
		}
		// A new key when this one was revoked, here or in any environment
		// of this computer (revoked.json); one revoked here before that
		// list existed counts too.
		current, known, _ := entryReadComputer(paths)
		rotate := false
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(current.Public)); known && err == nil {
			record := st.device(current.ID)
			rotate = entryKeyRevoked(paths, key) || record != nil && record.Revoked != nil && record.PublicKey == current.Public
		}
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
	if err := entrySweepOrphans(paths); err != nil {
		diagnostics.Log("warn", "entry", "removing the ssh configuration of deleted environments: "+err.Error())
	}
	// Start putting the guest's run script in place when the environment
	// runs, so the first scp does not wait for it; nothing waits for it
	// here, and a failure is retried by the first channel that needs it.
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready && s.vmRunning() {
		s.entryStartWrapper()
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
	s.entryCancelWrapper()
	if err := entryRemoveClient(paths, entryEnvironmentID(s.profile)); err != nil {
		requestError(w, errors.New("本机终端已关闭，但 ssh 配置没能全部移除："+err.Error()))
		return
	}
	if err := entrySweepOrphans(paths); err != nil {
		diagnostics.Log("warn", "entry", "removing the ssh configuration of deleted environments: "+err.Error())
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
		jsonReply(w, entryIncludePreviewFor(paths, entryEnvironmentID(s.profile), entryEnvironmentName(s.profile)))
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
// The key is revoked for the whole computer (revoked.json), and every other
// environment whose controller runs is told at once, so it cuts that
// device's connections too and marks it revoked (F10 of the review).
func (s *Server) entryRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input struct {
		// Propagated: another environment passes its revocation on.
		Propagated bool `json:"propagated"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &input, 1<<10) {
		return
	}
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
	record := st.device(id)
	if err != nil || record == nil {
		s.entry.mu.Unlock()
		http.Error(w, "没有这台设备", http.StatusNotFound)
		return
	}
	// The device is refused here even when revoked.json cannot be read
	// (every key is refused then).
	var keyErr error
	if record.PublicKey != "" {
		keyErr = entryRevokeKey(paths, record.PublicKey)
	}
	if record.Revoked == nil {
		now := time.Now().UTC()
		record.Revoked = &now
		err = entrySaveState(s.profile, st)
	}
	s.entry.mu.Unlock()
	s.entry.registry.cutDevice(id, errEntryRevoked)
	var unreached []string
	if !input.Propagated {
		unreached = s.entryRevokeElsewhere(paths, id)
	}
	if err = errors.Join(keyErr, err); err != nil {
		requestError(w, err)
		return
	}
	view := s.entryView()
	view.Unreached = unreached
	jsonReply(w, view)
}

// entryPropagateWait bounds how long a revocation waits for the other
// environments' controllers.
const entryPropagateWait = 2 * time.Second

// entryRevokeElsewhere passes a revocation on to the controllers of the
// other environments on this computer whose entry is on, so they cut the
// device's open connections (N4 of the second review). It returns the
// names of the running ones that did not answer in time: their open
// connections go on, and the page says so. One that does not run has no
// connection and refuses the key from revoked.json at its next dial.
func (s *Server) entryRevokeElsewhere(paths entryPaths, device string) []string {
	here := entryEnvironmentID(s.profile)
	defaultConfig := filepath.Join(paths.ComputerDir, "config.json")
	matches, _ := filepath.Glob(filepath.Join(paths.hostsDir(), "*.conf"))
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), entryPropagateWait)
	defer cancel()
	var calls sync.WaitGroup
	var mu sync.Mutex
	var unreached []string
	for _, match := range matches {
		environment := strings.TrimSuffix(filepath.Base(match), ".conf")
		configPath, err := entryProfileConfig(defaultConfig, environment)
		if environment == here || err != nil {
			continue
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			err := entryCall(ctx, configPath, "POST", "/api/entry/devices/"+url.PathEscape(device)+"/revoke", map[string]bool{"propagated": true}, nil)
			var refusal *entryRefusal
			if err == nil || errors.As(err, &refusal) && refusal.Code == errEntryOff.Code || controllerLockFree(filepath.Dir(configPath)) {
				return
			}
			diagnostics.Log("info", "entry", "revocation not passed on to environment "+entryShortID(environment)+": "+entryClientError(err))
			mu.Lock()
			unreached = append(unreached, entryEnvironmentName(filepath.Dir(configPath)))
			mu.Unlock()
		}()
	}
	calls.Wait()
	slices.Sort(unreached)
	return unreached
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
