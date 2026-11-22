package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/cloudproxy"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

// The desktop's side of the cloud entry (design 5.2, 5.3, 8 row 4). When the
// desktop app is connected to a cloud workspace (a tenant VM, through its
// existing remote-host link), the 本机终端 page's status, on, off and include
// are answered here, on this computer, which owns ~/.ssh and the device key:
//   - on enrols this computer's device key through the link (the gateway's
//     POST /api/client/cloud-entry with the workspace's grant), which also
//     adds it to the workspace's own list and answers the endpoint's host
//     key; then it pins that key and writes the Host block, whose
//     ProxyCommand is cage-cloud-entry (or cage) entry proxy cloud-<vm id>,
//     and the record the proxy reads (portal, VM, device id);
//   - off removes them; the device stays enrolled at the portal, where it
//     is revoked.
// Closed builds (cloudentry) answer none of it: the request goes to the
// workspace as before, where the gateway refuses /api/entry/, and the
// client context has no cloud_entry, so the page shows no cloud row.

// cloudProfile is the tenant workspace the desktop is connected to.
func (d *desktopGateway) cloudProfile() (remotehost.HostProfile, bool) {
	d.mu.RLock()
	active := d.active
	d.mu.RUnlock()
	return d.cloudProfileOf(active)
}

func (d *desktopGateway) cloudProfileOf(active *desktopTarget) (remotehost.HostProfile, bool) {
	if active == nil || active.mode != "remote" {
		return remotehost.HostProfile{}, false
	}
	id := active.hostID
	if id == "" {
		id = active.state().HostID
	}
	profile, err := d.store.Get(id)
	if err != nil || profile.Kind != remotehost.TenantKind || !cloudentry.ValidVM(profile.VMID) {
		return remotehost.HostProfile{}, false
	}
	return profile, true
}

// cloudPortalHost is the portal's host as the gateway signs it: the name,
// and the port unless it is 443 (remotehost.parseTenantPortal).
func cloudPortalHost(portal string) (string, bool) {
	parsed, err := url.Parse(portal)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") {
		return "", false
	}
	host := strings.ToLower(parsed.Host)
	host = strings.TrimSuffix(host, ":443")
	return host, true
}

// cloudEntryRoute answers the page's entry routes for a cloud workspace. It
// reports whether it answered.
func (s *Server) cloudEntryRoute(w http.ResponseWriter, r *http.Request, session *remotehost.Session) bool {
	if !cloudentry.Open() || s.remote == nil || s.remote.desktop == nil || session == nil {
		return false
	}
	route := r.Method + " " + r.URL.Path
	switch route {
	case "GET /api/entry", "POST /api/entry/on", "POST /api/entry/off", "GET /api/entry/include", "POST /api/entry/include":
	default:
		return false
	}
	profile, ok := s.remote.desktop.cloudProfile()
	if !ok {
		return false
	}
	if r.Header.Get(agentRequestHeader) != "" {
		entryRefuse(w, errEntryWindowOnly)
		return true
	}
	if !s.hasTrustedView(r) {
		entryRefuse(w, errEntryLocalOnly)
		return true
	}
	paths, err := s.entryPaths()
	if err != nil {
		requestError(w, err)
		return true
	}
	cloud := cloudWorkspace{paths: paths, profile: profile, session: session}
	switch route {
	case "GET /api/entry":
		jsonReply(w, cloud.view())
	case "POST /api/entry/on":
		s.entry.mu.Lock()
		err := cloud.on(r.Context())
		s.entry.mu.Unlock()
		if err != nil {
			requestError(w, errors.New("无法开启本机终端："+err.Error()))
			return true
		}
		jsonReply(w, cloud.view())
	case "POST /api/entry/off":
		s.entry.mu.Lock()
		err := cloud.off()
		s.entry.mu.Unlock()
		if err != nil {
			requestError(w, err)
			return true
		}
		jsonReply(w, cloud.view())
	case "GET /api/entry/include":
		jsonReply(w, entryIncludePreviewFor(paths, cloud.environment(), profile.Name))
	case "POST /api/entry/include":
		if !cloud.enabled() {
			entryRefuse(w, errEntryOff)
			return true
		}
		var input struct {
			Token string `json:"token"`
		}
		if !readJSON(w, r, &input) {
			return true
		}
		s.entry.mu.Lock()
		view, err := entryIncludeApply(paths, input.Token)
		s.entry.mu.Unlock()
		if errors.Is(err, errEntryIncludeChanged) {
			http.Error(w, err.Error(), http.StatusConflict)
			return true
		}
		if err != nil {
			requestError(w, errors.New("无法写入 ~/.ssh/config："+err.Error()))
			return true
		}
		jsonReply(w, view)
	}
	return true
}

// cloudWorkspace is the entry of one cloud workspace on this computer.
type cloudWorkspace struct {
	paths   entryPaths
	profile remotehost.HostProfile
	session *remotehost.Session
}

func (c cloudWorkspace) environment() string { return cloudentry.Environment(c.profile.VMID) }

func (c cloudWorkspace) proxyPaths() entryproxy.Paths {
	return entryproxy.Paths{SSHDir: c.paths.SSHDir, DeviceDir: c.paths.DeviceDir}
}

func (c cloudWorkspace) enabled() bool {
	_, err := cloudproxy.ReadCloudRecord(c.proxyPaths(), c.environment())
	return err == nil && entryReadAlias(c.paths.hostFile(c.environment())) != ""
}

func (c cloudWorkspace) view() entryStatusView {
	environment := c.environment()
	view := entryStatusView{Supported: true, Environment: environment, Name: c.profile.Name, State: "off", Devices: []entryDeviceView{}, Kept: []entryKeptView{}}
	if computer, ok, _ := entryReadComputer(c.paths); ok {
		view.Computer = computer.ID
	}
	if !c.enabled() {
		return view
	}
	// Whether a connection is open is the gateway's to know; the page
	// shows the entry as on.
	view.Enabled, view.State = true, "idle"
	view.HostAlias = entryReadAlias(c.paths.hostFile(environment))
	include := entryIncludePreview(c.paths)
	view.Include = &include
	cage := c.paths.Cage
	onPath := false
	if found, err := lookPathCage(); err == nil && sameFile(found, c.paths.Cage) {
		cage, onPath = "cage", true
	} else if runtime.GOOS == "windows" {
		cage = "& " + entryQuote(cage)
	} else {
		cage = shellescape.Quote(cage)
	}
	view.Commands = &entryCommands{SSH: "ssh " + view.HostAlias, CageSSH: cage + " ssh " + environment, Cage: onPath}
	view.Herdr = &entryHerdrView{State: "not_set_up", Command: entryHerdrCommand(view.HostAlias, include.Present, c.paths.cageSSHFile())}
	return view
}

// cloudEnrolment is the gateway's answer to POST /api/client/cloud-entry.
type cloudEnrolment struct {
	DeviceID string `json:"device_id"`
	HostKey  string `json:"host_key"`
	VMID     string `json:"vm_id"`
	Host     string `json:"host"`
}

var errCloudRevoked = errors.New("这台电脑的钥匙已在门户撤销")

// enrol sends this computer's key through the desktop's link to the
// workspace, as the workspace's grant.
func (c cloudWorkspace) enrol(ctx context.Context, computer entryComputer) (cloudEnrolment, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"name": computer.Name, "public_key": strings.TrimSpace(computer.Public)})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.session.Origin+"/api/client/cloud-entry", bytes.NewReader(body))
	if err != nil {
		return cloudEnrolment{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.session.Client.Do(request)
	if err != nil {
		return cloudEnrolment{}, errors.New("无法连接云端工作区")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(data) > 8192 {
		return cloudEnrolment{}, errors.New("门户的回答无效")
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct{ Code, Message string }
		_ = json.Unmarshal(data, &refusal)
		switch {
		case refusal.Code == "revoked":
			return cloudEnrolment{}, errCloudRevoked
		case response.StatusCode == http.StatusNotFound:
			return cloudEnrolment{}, errors.New("这个门户还没有开放本机终端")
		case refusal.Message != "" && validRefusalText(refusal.Message):
			return cloudEnrolment{}, errors.New(refusal.Message)
		}
		return cloudEnrolment{}, errors.New("门户没有接受这台电脑")
	}
	var reply cloudEnrolment
	if json.Unmarshal(data, &reply) != nil {
		return cloudEnrolment{}, errors.New("门户的回答无效")
	}
	return reply, nil
}

func validRefusalText(text string) bool {
	return len(text) <= 600 && !strings.ContainsAny(text, "\r\n\x00<>")
}

// on enrols this computer and writes its client configuration. Caller
// holds s.entry.mu.
func (c cloudWorkspace) on(ctx context.Context) error {
	portal, ok := cloudPortalHost(c.profile.Portal)
	if !ok {
		return errors.New("门户地址无效")
	}
	computer, err := entryEnsureComputer(c.paths, false)
	if err != nil {
		return err
	}
	reply, err := c.enrol(ctx, computer)
	if errors.Is(err, errCloudRevoked) {
		// A key revoked at the portal stays refused: a new one, as when it
		// was revoked locally.
		if computer, err = entryEnsureComputer(c.paths, true); err != nil {
			return err
		}
		reply, err = c.enrol(ctx, computer)
	}
	if err != nil {
		return err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(reply.HostKey))
	if err != nil || hostKey.Type() != ssh.KeyAlgoED25519 || reply.VMID != c.profile.VMID || !cloudentry.ValidDevice(reply.DeviceID) ||
		reply.Host != "vm-"+c.profile.VMID+"."+portal {
		return errors.New("门户的回答和这个工作区不一致")
	}
	record, _ := json.Marshal(cloudproxy.CloudRecord{Version: 1, Portal: portal, VM: c.profile.VMID, Device: reply.DeviceID})
	path := cloudproxy.RecordPath(c.proxyPaths(), c.environment())
	if err := host.EnsurePrivateDir(c.paths.DeviceDir); err != nil {
		return err
	}
	if err := host.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := entryWriteFile(path, record, 0o600); err != nil {
		return err
	}
	_, err = entryWriteClient(c.paths, c.environment(), c.profile.Name, hostKey)
	return err
}

// off removes this computer's configuration of the workspace. Caller holds
// s.entry.mu.
func (c cloudWorkspace) off() error {
	if !c.enabled() {
		return errEntryOff
	}
	if err := entryRemoveClient(c.paths, c.environment()); err != nil {
		return errors.New("本机终端已关闭，但 ssh 配置没能全部移除：" + err.Error())
	}
	if err := os.Remove(cloudproxy.RecordPath(c.proxyPaths(), c.environment())); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
