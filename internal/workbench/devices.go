package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/keepawake"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// The paired devices of this computer: phones, and other computers running
// Super Cage, that reach its phone surface through the relay. Each device has
// its own serving loop, room proof and room context; they share one keep-awake
// hold and the two graphical surfaces. CLI-managed devices (cage-mobile-link)
// keep their separate identities and are not listed here.
type mobilePairing struct {
	mu               sync.Mutex
	browser, desktop phoneSurfaceState
	devices          []*pairedDevice
	// unreadable: paired-devices.json could not be read at start. Nothing is
	// served or added until it is cleared (DELETE /api/mobile-pair).
	unreadable bool
	// legacyBroken: the phone file of earlier versions could not be read.
	legacyBroken bool
	invite       *deviceInvite
	status       string // the line today's settings page shows
	closed       bool
	power        deviceKeepAwake
}

// pairedDevice is one device and its serving state, guarded by mobilePairing.mu.
type pairedDevice struct {
	deviceRecord
	cancel    context.CancelFunc // ends its loop, its link and its requests
	proof     string             // the current room's proof, "" between rooms
	roomCtx   context.Context
	connected bool
	savedSeen time.Time // LastSeen as last written to disk
}

// deviceKeepAwake is the one keep-awake hold while any device is paired.
type deviceKeepAwake struct {
	cancel context.CancelFunc
	done   <-chan struct{}
	state  keepawake.State
}

// deviceInvite is the code shown by the add-device dialog, or its outcome.
// One relay room serves today's QR and long code and the 8-digit short code.
type deviceInvite struct {
	cancel  context.CancelFunc
	expires time.Time
	state   string // waiting, request, confirming, joined, rejected, failed, expired
	device  string // the id of the device that joined
	changed chan struct{}
	// request is the device that proved the short code and waits for 允许
	// (state request); answer takes the person's decision once.
	request *mobilelink.DeviceRequest
	answer  chan bool
	reason  string // why a code failed, when the short code says
}

// deviceInviteLife bounds an invite: the code's two minutes (applied by
// mobilelink.AcceptDevice) plus the wait for 允许 and the answer.
const deviceInviteLife = 4 * time.Minute

// guestProfile is the device's private guest browser profile: stable across
// its reconnects, unrelated to its id, and never a name the guest could match.
func (d *pairedDevice) guestProfile() string {
	digest := sha256.Sum256(d.Seed)
	return hex.EncodeToString(digest[:])
}

// pairedDevice returns the device whose current room proof r carries, and that
// room's context. The proof is added by this host's own link (backend.go) and
// never crosses the relay; it grants phone operations only and ends with the room.
func (s *Server) pairedDevice(r *http.Request) (*pairedDevice, context.Context) {
	if !phoneRoomRoute(r.URL.Path) {
		return nil, nil
	}
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil
	}
	d := p.deviceForRequestLocked(r)
	if d == nil {
		return nil, nil
	}
	return d, d.roomCtx
}

func (p *mobilePairing) deviceForRequestLocked(r *http.Request) *pairedDevice {
	got := r.Header.Get(mobilelink.PhoneRoomHeader)
	if got == "" {
		return nil
	}
	var found *pairedDevice
	for _, d := range p.devices {
		// Every live proof is compared, so the time taken does not tell which matched.
		if d.roomCtx != nil && d.roomCtx.Err() == nil && equalSecret(d.proof, got) && found == nil {
			found = d
		}
	}
	return found
}

// deviceRoutes are the device list of this computer (docs/design/devices.md).
// They belong to the workload: the desktop gateway forwards them to the local
// one and refuses them for an SSH host or a cloud workspace (routeRemote).
func (s *Server) deviceRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/devices", s.devicesLocal(s.listDevices))
	mux.HandleFunc("POST /api/devices/invite", s.deviceWindowOnly(s.inviteDevice))
	mux.HandleFunc("GET /api/devices/invite", s.devicesLocal(s.inviteState))
	mux.HandleFunc("DELETE /api/devices/invite", s.devicesLocal(s.cancelInvite))
	mux.HandleFunc("POST /api/devices/invite/approve", s.deviceWindowOnly(s.answerInvite(true)))
	mux.HandleFunc("POST /api/devices/invite/reject", s.deviceWindowOnly(s.answerInvite(false)))
	mux.HandleFunc("DELETE /api/devices/{id}", s.devicesLocal(s.removeDevice))
}

func (s *Server) devicesLocal(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.desktopEntry || s.clientOnly {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}
}

// deviceWindowOnly guards minting a code, which changes who can reach this
// computer. Like the SSH entry (entryLocal) it refuses the agent transport's
// header and requires the desktop window's trusted view. An agent session
// carries the trusted-view cookie too, so the header is what tells them apart;
// the CLI refuses these routes before sending (agentWindowRoutes).
func (s *Server) deviceWindowOnly(handler http.HandlerFunc) http.HandlerFunc {
	return s.devicesLocal(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(agentRequestHeader) != "" {
			http.Error(w, "添加设备会改变谁能访问这台电脑，只能在桌面窗口操作。", http.StatusForbidden)
			return
		}
		if !s.hasTrustedView(r) {
			http.Error(w, "请在这台电脑的 Super Cage 窗口里添加设备。", http.StatusForbidden)
			return
		}
		handler(w, r)
	})
}

// phoneRoomRoute is a route a paired device may use through its room.
func phoneRoomRoute(path string) bool {
	switch path {
	case "/api/mobile/device", "/api/mobile/environment", "/api/mobile/terminals":
		return true
	}
	for _, prefix := range []string{"/api/mobile/cli/", "/api/mobile/browser/", "/api/mobile/desktop/", "/api/mobile/environment/", "/api/mobile/terminals/", "/api/mobile/files/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// runKeepAwake holds the host awake until ctx ends. Tests replace it.
func (s *Server) runKeepAwake(ctx context.Context, report func(keepawake.State)) {
	if s.keepAwake != nil {
		s.keepAwake(ctx, report)
		return
	}
	keepawake.Run(ctx, report)
}

// restoreDevices reads the device list at start, migrating the phone of
// earlier versions, and starts one serving loop per device. With nothing
// paired it starts nothing: no goroutine, timer or relay request.
func (s *Server) restoreDevices() error {
	got := loadDevices(s.devicesFile(), s.phoneFile(), time.Now())
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	if got.unreadable != nil {
		p.unreadable = true
		p.status = errDevicesUnreadable.Error()
		return got.unreadable
	}
	if got.legacy != nil {
		p.legacyBroken = true
		p.status = got.legacy.Error()
	}
	if got.write {
		// On failure the devices are still served; the next start applies the
		// same rules to the same files again.
		if err := writeDevicesFile(s.devicesFile(), got.devices); err != nil {
			diagnostics.Log("warn", "devices", "could not write the device list: "+err.Error())
		}
	}
	for _, record := range got.devices {
		s.attachDeviceLocked(record)
	}
	return got.legacy
}

func (s *Server) attachDeviceLocked(record deviceRecord) *pairedDevice {
	ctx, cancel := context.WithCancel(context.Background())
	d := &pairedDevice{deviceRecord: record, cancel: cancel}
	if record.LastSeen != nil {
		d.savedSeen = *record.LastSeen
	}
	p := &s.phones
	p.devices = append(p.devices, d)
	s.holdAwakeLocked()
	go s.serveDevice(ctx, d)
	return d
}

// holdAwakeLocked starts the shared keep-awake hold if a device is paired.
func (s *Server) holdAwakeLocked() {
	p := &s.phones
	if p.power.cancel != nil || len(p.devices) == 0 || p.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.power = deviceKeepAwake{cancel: cancel, done: done}
	go func() {
		defer close(done)
		s.runKeepAwake(ctx, func(state keepawake.State) {
			p.mu.Lock()
			if p.power.done == (<-chan struct{})(done) {
				p.power.state = state
			}
			p.mu.Unlock()
			if state.Error != "" {
				diagnostics.Log("warn", "mobile-remote", "keep-awake request failed: "+state.Error)
			}
		})
	}()
}

// releaseAwakeLocked ends the hold. Wait for the returned channel only after
// unlocking: the hold reports its end under the same lock.
func (s *Server) releaseAwakeLocked() <-chan struct{} {
	p := &s.phones
	if p.power.cancel == nil {
		return nil
	}
	p.power.cancel()
	done := p.power.done
	p.power = deviceKeepAwake{}
	return done
}

func (s *Server) serveDevice(ctx context.Context, d *pairedDevice) {
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.deviceRoom(ctx, d)
		if ctx.Err() != nil {
			return
		}
		if err == nil || time.Since(start) > time.Minute {
			delay = time.Second
		} else {
			delay = min(30*time.Second, 2*delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// deviceRoom registers one discoverable room for d and serves the device that
// authenticates in it. The local workbench session is opened only then.
func (s *Server) deviceRoom(ctx context.Context, d *pairedDevice) error {
	p := &s.phones
	p.mu.Lock()
	r := d.deviceRecord
	p.mu.Unlock()
	id, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(r.Seed))
	if err != nil {
		return err
	}
	room, err := mobilelink.CreateDiscoverableRoom(ctx, r.Relay, r.Admin, r.Lookup)
	if err != nil {
		return err
	}
	proof := randomToken()
	p.mu.Lock()
	if ctx.Err() != nil || p.closed {
		p.mu.Unlock()
		return context.Canceled
	}
	roomCtx, cancelRoom := context.WithCancel(ctx)
	d.proof, d.roomCtx = proof, roomCtx
	p.mu.Unlock()
	defer func() {
		cancelRoom()
		p.mu.Lock()
		defer p.mu.Unlock()
		if d.proof == proof {
			d.proof, d.roomCtx = "", nil
			if d.connected {
				d.connected = false
				s.seenLocked(d)
			}
		}
	}()
	return mobilelink.ServeHost(ctx, r.Relay, room.ID, room.HostToken, id, r.Pin, func() (*mobilelink.Workbench, error) {
		b, err := mobilelink.OpenWorkbench(ctx, s.origin, s.issueTicket())
		if err != nil {
			return nil, err
		}
		b.SetPhoneRoom(proof)
		p.mu.Lock()
		if d.proof == proof {
			d.connected = true
			s.seenLocked(d)
		}
		p.mu.Unlock()
		return b, nil
	})
}

// seenLocked notes that d connected or disconnected now. It is written to disk
// at most every few minutes, so a phone that reconnects often costs no writes.
func (s *Server) seenLocked(d *pairedDevice) {
	now := time.Now().UTC().Truncate(time.Second)
	d.LastSeen = &now
	if now.Sub(d.savedSeen) < 5*time.Minute {
		return
	}
	if err := s.writeDevicesLocked(); err == nil {
		d.savedSeen = now
	}
}

func (s *Server) writeDevicesLocked() error {
	p := &s.phones
	if p.unreadable {
		return errDevicesUnreadable
	}
	records := make([]deviceRecord, 0, len(p.devices))
	for _, d := range p.devices {
		records = append(records, d.deviceRecord)
	}
	return writeDevicesFile(s.devicesFile(), records)
}

// removeDeviceLocked forgets d at once: its link closes, its in-flight
// requests end (server.go ties them to its room), the surfaces it holds are
// retired (their sessions derive from its room), its relay registration stops
// and its record is deleted, with the phone file of earlier versions when d
// is that phone.
func (s *Server) removeDeviceLocked(d *pairedDevice) error {
	p := &s.phones
	if d.Legacy {
		if err := os.Remove(s.phoneFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	before := p.devices
	p.devices = slices.DeleteFunc(slices.Clone(p.devices), func(x *pairedDevice) bool { return x == d })
	if err := s.writeDevicesLocked(); err != nil {
		if !d.Legacy {
			// Still on disk: it would come back at the next start.
			p.devices = before
			return err
		}
		// The phone file is gone, so the next start drops the entry anyway.
		diagnostics.Log("warn", "devices", "could not write the device list: "+err.Error())
	}
	d.cancel()
	d.proof, d.roomCtx, d.connected = "", nil, false
	if len(p.devices) == 0 {
		s.releaseAwakeLocked()
		p.status = ""
	}
	return nil
}

// stopDevices ends every device loop and the keep-awake hold (service exit).
func (s *Server) stopDevices() {
	p := &s.phones
	p.mu.Lock()
	p.closed = true
	p.dropInviteLocked()
	for _, d := range p.devices {
		d.cancel()
		d.proof, d.roomCtx, d.connected = "", nil, false
	}
	done := s.releaseAwakeLocked()
	p.mu.Unlock()
	if done != nil {
		<-done
	}
}

// legacyDeviceLocked is the device today's settings page shows: the phone of
// earlier versions if there is one, else the first device.
func (p *mobilePairing) legacyDeviceLocked() *pairedDevice {
	for _, d := range p.devices {
		if d.Legacy {
			return d
		}
	}
	if len(p.devices) > 0 {
		return p.devices[0]
	}
	return nil
}

// phoneStatus is GET /api/mobile-pair, kept for today's settings page.
func (s *Server) phoneStatus(w http.ResponseWriter, r *http.Request) {
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.legacyDeviceLocked()
	var device *mobilelink.DeviceInfo
	if d != nil && d.Platform != "" {
		device = &mobilelink.DeviceInfo{Platform: d.Platform, Model: d.Name}
	}
	jsonReply(w, map[string]any{"paired": d != nil, "status": p.status, "device": device, "keep_awake": p.power.state})
}

type deviceView struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Platform  string     `json:"platform"`
	Name      string     `json:"name"`
	AddedAt   time.Time  `json:"added_at"`
	LastSeen  *time.Time `json:"last_seen"`
	Connected bool       `json:"connected"`
}

// listDevices is GET /api/devices. No seed, pin, lookup or admin token leaves.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	views := make([]deviceView, 0, len(p.devices))
	for _, d := range p.devices {
		connected := d.connected && d.roomCtx != nil && d.roomCtx.Err() == nil
		views = append(views, deviceView{ID: d.ID, Kind: d.Kind, Platform: d.Platform, Name: d.Name, AddedAt: d.AddedAt, LastSeen: d.LastSeen, Connected: connected})
	}
	reply := map[string]any{"devices": views, "limit": deviceLimit, "keep_awake": p.power.state, "invite": nil}
	if p.invite != nil {
		reply["invite"] = map[string]any{"state": p.invite.state, "expires": p.invite.expires}
	}
	switch {
	case p.unreadable:
		reply["problem"] = errDevicesUnreadable.Error()
	case p.legacyBroken:
		reply["problem"] = errLegacyUnreadable.Error()
	}
	jsonReply(w, reply)
}

// removeDevice is DELETE /api/devices/{id}.
func (s *Server) removeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	at := slices.IndexFunc(p.devices, func(d *pairedDevice) bool { return d.ID == id })
	if at < 0 {
		http.Error(w, "这台设备已经移除", http.StatusNotFound)
		return
	}
	if err := s.removeDeviceLocked(p.devices[at]); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]string{"removed": id})
}

// reportPhoneDevice is POST /api/mobile/device: only a device's own link may
// describe it. The first report fixes its kind; later ones may change its name.
func (s *Server) reportPhoneDevice(w http.ResponseWriter, r *http.Request) {
	var input mobilelink.DeviceInfo
	if !readJSON(w, r, &input, 1024) {
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	// Phones report android or ios; a joined computer its GOOS.
	if !devicePlatform(input.Platform) || !deviceName(input.Model) {
		http.Error(w, "设备型号或平台无效", http.StatusBadRequest)
		return
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if !s.mobileAuthorized(r.Context()) {
		http.Error(w, "请先解锁 Super Cage", http.StatusLocked)
		return
	}
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.deviceForRequestLocked(r)
	if p.closed || d == nil {
		http.Error(w, "请通过已配对的设备连接", http.StatusForbidden)
		return
	}
	if d.Platform != "" && d.Platform != input.Platform {
		http.Error(w, "设备类型不能更改", http.StatusConflict)
		return
	}
	if d.Platform != input.Platform || d.Name != input.Model {
		before := d.deviceRecord
		d.Platform, d.Kind, d.Name = input.Platform, deviceKind(input.Platform), input.Model
		if err := s.writeDevicesLocked(); err != nil {
			d.deviceRecord = before
			requestError(w, err)
			return
		}
		if d.Legacy {
			// Earlier versions read the model from their own file.
			if err := writePrivate(s.phoneFile(), d.phoneRecord()); err != nil {
				diagnostics.Log("warn", "devices", "could not update the phone file of earlier versions: "+err.Error())
			}
		}
	}
	jsonReply(w, map[string]bool{"ok": true})
}

// removePhone is DELETE /api/mobile-pair, kept for today's settings page: it
// removes the only device, and clears a list or phone file that cannot be read.
func (s *Server) removePhone(w http.ResponseWriter, r *http.Request) {
	p := &s.phones
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropInviteLocked()
	switch {
	case p.unreadable:
		// Kept aside, never written over; the devices in it are not served.
		if err := os.Rename(s.devicesFile(), filepath.Join(s.profile, devicesAsideName)); err != nil && !os.IsNotExist(err) {
			requestError(w, err)
			return
		}
		if err := os.Remove(s.phoneFile()); err != nil && !os.IsNotExist(err) {
			requestError(w, err)
			return
		}
		p.unreadable, p.legacyBroken = false, false
	case p.legacyBroken:
		if err := os.Remove(s.phoneFile()); err != nil && !os.IsNotExist(err) {
			requestError(w, err)
			return
		}
		p.legacyBroken = false
	case len(p.devices) > 1:
		http.Error(w, "已添加多台设备，请在“设备”页面逐台移除。", http.StatusConflict)
		return
	case len(p.devices) == 1:
		if err := s.removeDeviceLocked(p.devices[0]); err != nil {
			requestError(w, err)
			return
		}
	default:
		if err := os.Remove(s.phoneFile()); err != nil && !os.IsNotExist(err) {
			requestError(w, err)
			return
		}
	}
	p.status = ""
	jsonReply(w, map[string]bool{"paired": len(p.devices) > 0})
}

// dropInviteLocked cancels the code that is showing and forgets the invite.
func (p *mobilePairing) dropInviteLocked() {
	if p.invite == nil {
		return
	}
	p.invite.cancel()
	close(p.invite.changed)
	p.invite = nil
	if p.status == "等待扫码" {
		p.status = ""
	}
}

func (p *mobilePairing) inviteStateLocked(invite *deviceInvite, state, device string) {
	invite.state, invite.device = state, device
	close(invite.changed)
	invite.changed = make(chan struct{})
}

// inviteDevice is POST /api/devices/invite and POST /api/mobile-pair: a new
// code (today's QR and long code) that adds one device. It dies on lock, on a
// newer code, on DELETE /api/devices/invite and after two minutes.
func (s *Server) inviteDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Admin string `json:"admin"`
		Relay string `json:"relay"`
	}
	if !readJSON(w, r, &input, 8192) {
		return
	}
	epoch, ok := s.account.access()
	if !ok {
		http.Error(w, "请先解锁 Super Cage", 423)
		return
	}
	s.mu.Lock()
	origin := s.preferences.MobileRelay
	s.mu.Unlock()
	if origin == "" {
		origin = mobilelink.OfficialRelay
	}
	if input.Admin != "" && (origin == mobilelink.OfficialRelay || origin != input.Relay) {
		requestError(w, errors.New("请先保存自建中继地址"))
		return
	}
	p := &s.phones
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		http.Error(w, "Super Cage 正在退出", http.StatusServiceUnavailable)
		return
	case p.unreadable:
		p.mu.Unlock()
		http.Error(w, errDevicesUnreadable.Error(), http.StatusConflict)
		return
	case len(p.devices) >= deviceLimit:
		p.mu.Unlock()
		http.Error(w, "最多添加 "+strconv.Itoa(deviceLimit)+" 台设备。请先移除不用的设备，再添加新的设备。", http.StatusConflict)
		return
	}
	p.dropInviteLocked()
	ctx, cancel := context.WithTimeout(epoch, deviceInviteLife)
	invite := &deviceInvite{cancel: cancel, state: "waiting", changed: make(chan struct{})}
	p.invite = invite
	p.status = "等待扫码"
	p.mu.Unlock()
	complete := false
	defer func() {
		if !complete {
			cancel()
			p.mu.Lock()
			if p.invite == invite {
				p.dropInviteLocked()
			}
			p.mu.Unlock()
		}
	}()
	// A fresh host identity per device: two devices cannot link this computer
	// by its pin.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		requestError(w, err)
		return
	}
	host, err := remotelink.IdentityFromKey(key)
	if err != nil {
		requestError(w, err)
		return
	}
	// One room for today's QR and long code (unchanged CAGE1, so today's
	// phone apps scan it) and for the short code; whoever takes its one seat
	// first wins. A code another host on the relay shows gets a new code.
	offer, err := mobilelink.NewDeviceInvite(ctx, origin, input.Admin, host)
	if err != nil {
		requestError(w, errors.New("中继未能创建连接。请检查网络、自建地址和管理令牌。"))
		return
	}
	png, err := qrcode.Encode(offer.Raw, qrcode.Medium, 384)
	if err != nil {
		requestError(w, errors.New("无法生成二维码"))
		return
	}
	// offer.Expires is the code's own whole second (mobilelink.NewDeviceInvite).
	p.mu.Lock()
	invite.expires = offer.Expires
	p.mu.Unlock()
	complete = true
	go func() {
		defer cancel()
		approve := func(decide context.Context, request mobilelink.DeviceRequest) bool {
			answer := make(chan bool, 1)
			p.mu.Lock()
			if p.invite != invite {
				p.mu.Unlock()
				return false
			}
			invite.request, invite.answer = &request, answer
			p.inviteStateLocked(invite, "request", "")
			p.mu.Unlock()
			select {
			case allowed := <-answer:
				return allowed
			case <-decide.Done():
				return false
			}
		}
		err := mobilelink.AcceptDevice(ctx, offer, host, approve, func(request mobilelink.DeviceRequest, paired mobilelink.PairedHost) error {
			pin := request.Pin
			// Commit trust in the same unlocked account epoch that issued the
			// code. A lock or logout must not race the on-disk enrollment.
			s.account.mu.Lock()
			defer s.account.mu.Unlock()
			p.mu.Lock()
			defer p.mu.Unlock()
			if ctx.Err() != nil || p.closed || p.invite != invite || p.unreadable || len(p.devices) >= deviceLimit {
				return errors.New("配对已结束")
			}
			record := deviceRecord{ID: newDeviceID(), Kind: "phone", AddedAt: time.Now().UTC().Truncate(time.Second), Seed: key.Seed(), Relay: origin, Pin: pin, Admin: input.Admin, Lookup: paired.Lookup}
			if request.ShortCode && devicePlatform(request.Platform) {
				// What the person approved; the device's first report may
				// change the name only.
				record.Platform, record.Kind = request.Platform, deviceKind(request.Platform)
				if deviceName(request.Name) {
					record.Name = request.Name
				}
			}
			records := make([]deviceRecord, 0, len(p.devices)+1)
			for _, d := range p.devices {
				records = append(records, d.deviceRecord)
			}
			if err := writeDevicesFile(s.devicesFile(), append(records, record)); err != nil {
				return err
			}
			s.attachDeviceLocked(record)
			p.inviteStateLocked(invite, "joined", record.ID)
			p.status = "手机已配对"
			return nil
		})
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.invite == invite && (invite.state == "waiting" || invite.state == "request" || invite.state == "confirming") {
			invite.request, invite.answer = nil, nil
			state, reason := inviteOutcome(ctx, invite, err)
			invite.reason = reason
			p.inviteStateLocked(invite, state, "")
			p.status = ""
			if err != nil && ctx.Err() == nil && state != "rejected" {
				p.status = "配对未完成，请重新生成二维码"
			}
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, map[string]any{"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), "expires": offer.Expires, "manual": offer.Manual, "short": offer.Code})
}

// inviteOutcome names how a code ended without a device joining. The relay
// ends a waiting room when the code expires, or just after.
func inviteOutcome(ctx context.Context, invite *deviceInvite, err error) (state, reason string) {
	switch {
	case errors.Is(err, mobilelink.ErrDeviceRejected):
		return "rejected", ""
	case errors.Is(err, mobilelink.ErrDeviceApprovalTimeout):
		return "expired", "没有及时允许，请重新生成。"
	case errors.Is(err, mobilelink.ErrDeviceCodeFailed), errors.Is(err, mobilelink.ErrDeviceJoinerLeft):
		return "failed", err.Error()
	case errors.Is(err, mobilelink.ErrDeviceCodeExpired):
		return "expired", ""
	case errors.Is(err, mobilelink.ErrDeviceNotConfirmed):
		return "failed", "对方没有确认核对数字，没有添加。"
	case ctx.Err() != nil || !time.Now().Before(invite.expires):
		return "expired", ""
	}
	return "failed", ""
}

// answerInvite is POST /api/devices/invite/approve {check} (允许) and
// POST /api/devices/invite/reject (拒绝) for the device that proved the short
// code. approve names the check number the window showed, so a late click
// never lands on a newer request.
func (s *Server) answerInvite(allow bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Check string `json:"check"`
		}
		if !readJSON(w, r, &input, 1024) {
			return
		}
		p := &s.phones
		p.mu.Lock()
		invite := p.invite
		if invite == nil || invite.state != "request" || invite.answer == nil || invite.request == nil || (allow && input.Check != invite.request.Check) {
			p.mu.Unlock()
			http.Error(w, "没有等待允许的设备，或核对数字已经变化", http.StatusConflict)
			return
		}
		invite.answer <- allow
		invite.answer = nil
		if allow {
			// The other computer's person now confirms the same number;
			// nothing is saved before that.
			p.inviteStateLocked(invite, "confirming", "")
		}
		p.mu.Unlock()
		jsonReply(w, map[string]bool{"ok": true})
	}
}

// inviteState is GET /api/devices/invite?wait=N: a held read that answers
// when the code's state changes or after N seconds (at most 25), so the
// dialog never polls on a timer.
func (s *Server) inviteState(w http.ResponseWriter, r *http.Request) {
	wait := 0
	if value := r.URL.Query().Get("wait"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 25 {
			http.Error(w, "等待时间无效", http.StatusBadRequest)
			return
		}
		wait = n
	}
	p := &s.phones
	p.mu.Lock()
	var changed chan struct{}
	if p.invite != nil && (p.invite.state == "waiting" || p.invite.state == "request" || p.invite.state == "confirming") && wait > 0 {
		changed = p.invite.changed
	}
	p.mu.Unlock()
	if changed != nil {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		select {
		case <-changed:
		case <-timer.C:
		case <-r.Context().Done():
		}
		timer.Stop()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	reply := map[string]any{"state": "none"}
	if invite := p.invite; invite != nil {
		reply = map[string]any{"state": invite.state, "expires": invite.expires}
		if at := slices.IndexFunc(p.devices, func(d *pairedDevice) bool { return d.ID == invite.device }); invite.state == "joined" && at >= 0 {
			reply["device"] = map[string]string{"id": invite.device, "name": p.devices[at].Name}
		}
		if request := invite.request; invite.state == "request" && request != nil {
			reply["request"] = map[string]string{"name": request.Name, "platform": request.Platform, "check": request.Check}
		}
		if invite.reason != "" {
			reply["error"] = invite.reason
		}
	}
	jsonReply(w, reply)
}

// cancelInvite is DELETE /api/devices/invite: the dialog closed.
func (s *Server) cancelInvite(w http.ResponseWriter, r *http.Request) {
	p := &s.phones
	p.mu.Lock()
	p.dropInviteLocked()
	p.mu.Unlock()
	jsonReply(w, map[string]string{"state": "none"})
}
