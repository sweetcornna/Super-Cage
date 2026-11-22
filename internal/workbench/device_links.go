package workbench

// The joining side of 设备互通 (multi-device): this computer joins
// another computer's Super Cage with today's long pairing code and opens that
// computer's phone surface in its own window. The other computer treats this
// one as a paired device (on a build released today, as its one phone).
//
// Joined computers live in joined-computers.json (0600) in the profile of the
// process that answers /api/client/: the desktop gateway, the client-only
// process, or a workload serving its own window. Each record keeps a device
// key made for that computer alone. Nothing runs for a saved record: a link
// exists only between 打开 and 返回本机 (or a lock, a removal, or quitting),
// and an open link that sees no request for a while closes its relay tunnel
// until the next request.

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/mobilelink/nativeclient"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

const (
	joinedComputersFile = "joined-computers.json"
	joinedComputersMax  = 16
	// deviceConnectionPrefix marks a window's connection to a joined computer:
	// device:<id>:<generation>, a new generation for every 打开.
	deviceConnectionPrefix = "device:"
	deviceLinkRest         = 5 * time.Minute
	deviceLockRecheck      = 2 * time.Second
	deviceDefaultName      = "另一台电脑"
	// deviceJoinLimit bounds a short-code join: the code's life plus the
	// other computer's approval (design §4.2).
	deviceJoinLimit = 3 * time.Minute
)

var (
	joinedComputerID = regexp.MustCompile("^[0-9a-f]{32}$")

	errDeviceWindowOnly = errors.New("加入和打开其他电脑只能在这台电脑的 Super Cage 窗口里操作")
	errDeviceLocked     = errors.New("Super Cage 已锁定，请先登录")
	errDeviceMobileOnly = errors.New("这台电脑以设备方式打开，只能使用手机版功能")
	errDeviceLinkClosed = errors.New("与那台电脑的连接已关闭，请返回本机重新打开")
)

type joinedComputer struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Platform string     `json:"platform,omitempty"`
	Relay    string     `json:"relay"`
	HostPin  string     `json:"host_pin"`
	Lookup   string     `json:"lookup_token"`
	Seed     []byte     `json:"seed"`
	AddedAt  time.Time  `json:"added_at"`
	LastOpen *time.Time `json:"last_open,omitempty"`
}

type joinedComputersRecord struct {
	Version   int              `json:"version"`
	Computers []joinedComputer `json:"computers"`
}

// joinedComputerView is what the window sees: never the key, the host pin
// or the lookup token.
type joinedComputerView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Platform string     `json:"platform"`
	Relay    string     `json:"relay"`
	AddedAt  time.Time  `json:"added_at"`
	LastOpen *time.Time `json:"last_open"`
	State    string     `json:"state"`
}

// deviceLink is one open link to a joined computer. Its context ends when
// the link closes; every request forwarded on it ends with it.
type deviceLink struct {
	id, generation string
	client         *nativeclient.Client
	api            http.Handler
	ctx            context.Context
	cancel         context.CancelFunc
	restAfter      time.Duration
	stopLock       func() bool

	mu       sync.Mutex
	inflight int
	idle     *time.Timer
	// wake orders resting (closing the relay tunnel while idle) against
	// waking for a request; resting is guarded by it.
	wake    sync.Mutex
	resting bool
}

type deviceLinks struct {
	mu    sync.Mutex
	links map[string]*deviceLink
	// The store's read-modify-write runs under this lock too.
	closed bool
	// unlockedAt is when the environment the desktop gateway shows last
	// answered that it is unlocked.
	unlockedAt time.Time
	// Tests shorten these; zero means the constants above.
	restAfter, recheck time.Duration
	// join is the short-code join in progress, or its outcome until the next
	// join; guarded by mu.
	join *deviceJoin
}

// deviceJoin is one short-code join. The window follows it with a held
// GET /api/client/devices/join; nothing runs for it once it has ended.
type deviceJoin struct {
	cancel  context.CancelFunc
	changed chan struct{} // closed and replaced on every change
	view    deviceJoinView
	// answer takes this window's 一致 / 不一致 once the other computer said
	// 允许 (state confirm); declined records 不一致 for the message.
	answer   chan bool
	declined bool
}

// deviceJoinView is what the window sees of a short-code join. State is
// "finding", "approval" (Check is the number both screens show, waiting for
// 允许 there), "confirm" (允许 arrived: this window must confirm the same
// number before anything is saved), "joined" (ID and Name of the saved
// computer) or "failed" (Error); "none" when no join has run since the last
// cancel.
type deviceJoinView struct {
	State string `json:"state"`
	Check string `json:"check,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error,omitempty"`
}

func (s *Server) joinedComputersPath() string { return filepath.Join(s.profile, joinedComputersFile) }

func (s *Server) loadJoinedComputers() ([]joinedComputer, error) {
	b, err := os.ReadFile(s.joinedComputersPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record joinedComputersRecord
	if json.Unmarshal(b, &record) != nil || record.Version != 1 {
		return nil, errors.New("无法读取已加入的电脑记录")
	}
	for _, c := range record.Computers {
		if !joinedComputerID.MatchString(c.ID) || len(c.Seed) != ed25519.SeedSize || !mobilelink.ValidRelayOrigin(c.Relay) || c.HostPin == "" || c.Lookup == "" {
			return nil, errors.New("无法读取已加入的电脑记录")
		}
	}
	return record.Computers, nil
}

func (s *Server) saveJoinedComputers(computers []joinedComputer) error {
	if computers == nil {
		computers = []joinedComputer{}
	}
	return writePrivate(s.joinedComputersPath(), joinedComputersRecord{Version: 1, Computers: computers})
}

// deviceRelaySetting is this computer's relay choice (mobile_relay, the same
// setting its phone pairing uses). A workload holds it in its preferences;
// the desktop gateway and the client-only process read the workbench.json
// beside the configuration they were started for.
func (s *Server) deviceRelaySetting() string {
	relay := ""
	if !s.desktopEntry && !s.clientOnly {
		s.mu.Lock()
		relay = s.preferences.MobileRelay
		s.mu.Unlock()
	} else if config := s.appUpdateConfigPath; config != "" {
		var saved struct {
			MobileRelay string `json:"mobile_relay"`
		}
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(config), "workbench.json")); err == nil && json.Unmarshal(b, &saved) == nil {
			relay = strings.TrimSuffix(strings.TrimSpace(saved.MobileRelay), "/")
		}
	}
	if relay == "" || !mobilelink.ValidRelayOrigin(relay) {
		return mobilelink.OfficialRelay
	}
	return relay
}

// deviceWindow is the gate of every joining-side route and of a window's
// requests to a joined computer: the SSH entry's own pair of checks
// (entryLocal), kept in every process kind. An agent session carries the
// trusted-view cookie too, so its header is what tells it apart.
func (s *Server) deviceWindow(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(agentRequestHeader) != "" || !s.hasTrustedView(r) {
		http.Error(w, errDeviceWindowOnly.Error(), http.StatusForbidden)
		return false
	}
	return true
}

// deviceUnlocked checks that this computer's own account allows opening or
// using a joined computer, where one exists, and returns the context a lock
// ends. A workload's account epoch ends on any lock. The desktop gateway asks
// the environment it shows, as the local terminal does, and closes every link
// when that environment answers locked or a lock passes through it. A
// client-only process has no account.
func (s *Server) deviceUnlocked(ctx context.Context) (context.Context, int, error) {
	switch {
	case s.remote != nil && s.remote.desktop != nil:
		current := s.remote.context()
		if current.Mode == "none" || current.ConnectionID == "" {
			return context.Background(), 0, nil
		}
		unlocked, err := s.localTerminalUnlocked(ctx, current.ConnectionID)
		if err != nil {
			return nil, http.StatusConflict, errors.New(localTerminalUnanswered)
		}
		if !unlocked {
			return nil, http.StatusLocked, errDeviceLocked
		}
		return context.Background(), 0, nil
	case s.desktopEntry || s.clientOnly:
		return context.Background(), 0, nil
	}
	epoch, ok := s.account.access()
	if !ok {
		return nil, http.StatusLocked, errDeviceLocked
	}
	return epoch, 0, nil
}

// deviceStillUnlocked runs before a request on an open link. In the desktop
// gateway it asks again at most every deviceLockRecheck; a locked answer
// closes every link.
func (s *Server) deviceStillUnlocked(ctx context.Context) (int, error) {
	if s.remote == nil || s.remote.desktop == nil {
		_, status, err := s.deviceUnlocked(ctx)
		return status, err
	}
	d := &s.devices
	d.mu.Lock()
	fresh := time.Since(d.unlockedAt) < cmp.Or(d.recheck, deviceLockRecheck)
	d.mu.Unlock()
	if fresh {
		return 0, nil
	}
	_, status, err := s.deviceUnlocked(ctx)
	if status == http.StatusLocked {
		d.closeAll()
	}
	if err == nil {
		d.mu.Lock()
		d.unlockedAt = time.Now()
		d.mu.Unlock()
	}
	return status, err
}

// noteDeviceLock closes every link when a lock of the environment the
// desktop shows passes through this gateway.
func (s *Server) noteDeviceLock(r *http.Request) {
	if r.Method == http.MethodPost && (r.URL.Path == "/api/account/lock" || r.URL.Path == "/api/account/logout" || r.URL.Path == "/api/account/suspend") {
		s.devices.closeAll()
	}
}

func (s *Server) clientDevicesRequest(w http.ResponseWriter, r *http.Request, path string) {
	if !s.deviceWindow(w, r) {
		return
	}
	rest, _ := strings.CutPrefix(path, "devices/")
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case r.Method == http.MethodGet && path == "devices":
		s.listJoinedComputers(w)
	case r.Method == http.MethodPost && path == "devices/join":
		s.joinComputer(w, r)
	case r.Method == http.MethodGet && path == "devices/join":
		s.joinComputerState(w, r)
	case r.Method == http.MethodDelete && path == "devices/join":
		s.devices.cancelJoin()
		jsonReply(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && (path == "devices/join/confirm" || path == "devices/join/reject"):
		s.answerJoin(w, r, path == "devices/join/confirm")
	case r.Method == http.MethodPost && joinedComputerID.MatchString(id) && action == "open":
		s.openJoinedComputer(w, r, id)
	case r.Method == http.MethodPost && joinedComputerID.MatchString(id) && action == "close":
		s.devices.close(id)
		jsonReply(w, map[string]bool{"ok": true})
	case r.Method == http.MethodDelete && joinedComputerID.MatchString(id) && action == "" && !strings.Contains(rest, "/"):
		s.forgetJoinedComputer(w, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listJoinedComputers(w http.ResponseWriter) {
	d := &s.devices
	d.mu.Lock()
	computers, err := s.loadJoinedComputers()
	open := map[string]bool{}
	for id := range d.links {
		open[id] = true
	}
	d.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	views := make([]joinedComputerView, 0, len(computers))
	for _, c := range computers {
		state := "closed"
		if open[c.ID] {
			state = "open"
		}
		views = append(views, joinedComputerView{ID: c.ID, Name: c.Name, Platform: c.Platform, Relay: c.Relay, AddedAt: c.AddedAt, LastOpen: c.LastOpen, State: state})
	}
	jsonReply(w, map[string]any{"computers": views})
}

func joinedComputerName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deviceDefaultName, nil
	}
	if utf8.RuneCountInString(name) > 40 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", errors.New("电脑名称最多 40 个字，且不能包含控制字符")
	}
	return name, nil
}

func (s *Server) joinComputer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input, 16<<10) {
		return
	}
	unlocked, status, err := s.deviceUnlocked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	name, err := joinedComputerName(input.Name)
	if err != nil {
		requestError(w, err)
		return
	}
	relay := s.deviceRelaySetting()
	// Eight digits are the short code; anything else is today's long code,
	// which names its relay.
	short, shortErr := mobilelink.ParseDeviceCode(input.Code)
	if shortErr != nil {
		codeRelay, err := mobilelink.PairCodeRelay(input.Code)
		if err != nil {
			requestError(w, errors.New("请输入那台电脑在“添加设备”里显示的 8 位数字，或粘贴配对码"))
			return
		}
		if codeRelay != relay {
			http.Error(w, fmt.Sprintf("这个配对码使用中继 %s，这台电脑设置的是 %s。请在设置里选择同一个中继，再加入。", relayDisplay(codeRelay), relayDisplay(relay)), http.StatusConflict)
			return
		}
	}
	d := &s.devices
	d.mu.Lock()
	existing, err := s.loadJoinedComputers()
	d.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	if len(existing) >= joinedComputersMax {
		http.Error(w, fmt.Sprintf("最多加入 %d 台电脑，请先移除不用的电脑", joinedComputersMax), http.StatusConflict)
		return
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		requestError(w, err)
		return
	}
	identity, err := remotelink.IdentityFromKey(key)
	if err != nil {
		requestError(w, err)
		return
	}
	if shortErr == nil {
		s.joinComputerByCode(w, r, unlocked, short, relay, name, key, identity)
		return
	}
	// The other computer saves its side as soon as it answers. Finish and
	// save this side even if the window gives up waiting; the code's own
	// expiry still bounds the exchange.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	credentials, err := mobilelink.RedeemPair(ctx, input.Code, relay, identity.Pin)
	if err != nil {
		diagnostics.Log("warn", "devices", "join failed: "+err.Error())
		http.Error(w, joinFailure(err), http.StatusConflict)
		return
	}
	var host mobilelink.PairedHost
	if err := json.Unmarshal([]byte(credentials), &host); err != nil {
		requestError(w, err)
		return
	}
	record, err := s.saveJoinedComputer(name, host, key)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]string{"id": record.ID, "name": record.Name})
}

func (s *Server) saveJoinedComputer(name string, host mobilelink.PairedHost, key ed25519.PrivateKey) (joinedComputer, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return joinedComputer{}, err
	}
	record := joinedComputer{ID: hex.EncodeToString(raw[:]), Name: name, Relay: host.Relay, HostPin: host.HostPin, Lookup: host.Lookup, Seed: key.Seed(), AddedAt: time.Now().UTC()}
	d := &s.devices
	d.mu.Lock()
	computers, err := s.loadJoinedComputers()
	if err == nil {
		err = s.saveJoinedComputers(append(computers, record))
	}
	d.mu.Unlock()
	return record, err
}

// joinComputerByCode joins with the other computer's 8-digit code (design
// §4.2): the room is found by the code, both computers prove it inside the
// TLS session, and the other computer's person clicks 允许 after comparing a
// four-digit check number. This reply carries that number as soon as both
// screens can show it ({state:"approval", check}); the wait for 允许 goes on
// here, and the window follows it with GET /api/client/devices/join?wait=25.
// The device key is proven by the TLS session. A lock, a new join or
// DELETE /api/client/devices/join cancels it.
func (s *Server) joinComputerByCode(w http.ResponseWriter, r *http.Request, unlocked context.Context, code, relay, name string, key ed25519.PrivateKey, identity *remotelink.Identity) {
	ctx, cancel := context.WithTimeout(unlocked, deviceJoinLimit)
	j := &deviceJoin{cancel: cancel, changed: make(chan struct{}), view: deviceJoinView{State: "finding"}}
	d := &s.devices
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel()
		http.Error(w, "Super Cage 正在退出", http.StatusServiceUnavailable)
		return
	}
	previous := d.join
	d.join = j
	changed := j.changed
	if previous != nil {
		previous.cancel()
		close(previous.changed)
	}
	d.mu.Unlock()
	go func() {
		defer cancel()
		hello := mobilelink.DeviceHello{Name: deviceSelfName(), Platform: runtime.GOOS}
		if !remotelink.ValidPairName(hello.Name) {
			hello.Name = "Super Cage"
		}
		host, err := mobilelink.RedeemDeviceCode(ctx, code, relay, identity, hello, func(check string) {
			d.updateJoin(j, deviceJoinView{State: "approval", Check: check})
		}, func(asked context.Context, check string) bool {
			// 允许 arrived. Nothing is saved, here or there, until this
			// window confirms the same number the other computer shows.
			answer := make(chan bool, 1)
			d.mu.Lock()
			if d.join != j {
				d.mu.Unlock()
				return false
			}
			j.answer = answer
			d.mu.Unlock()
			d.updateJoin(j, deviceJoinView{State: "confirm", Check: check})
			select {
			case same := <-answer:
				return same
			case <-asked.Done():
				return false
			}
		})
		if err == nil {
			var record joinedComputer
			if record, err = s.saveJoinedComputer(name, host, key); err == nil {
				d.updateJoin(j, deviceJoinView{State: "joined", ID: record.ID, Name: record.Name})
				return
			}
		}
		diagnostics.Log("warn", "devices", "join by code failed: "+err.Error())
		message := codeJoinFailure(err)
		if errors.Is(err, mobilelink.ErrDeviceNotConfirmed) {
			d.mu.Lock()
			declined := j.declined
			d.mu.Unlock()
			message = "没有及时确认核对数字，没有加入，也没有保存任何内容。"
			if declined {
				message = "两台电脑的核对数字不一致，没有加入，也没有保存任何内容。"
			}
		}
		d.updateJoin(j, deviceJoinView{State: "failed", Error: message})
	}()
	select {
	case <-changed:
	case <-r.Context().Done():
	case <-time.After(30 * time.Second):
	}
	view, _ := d.joinView()
	if view.State == "failed" {
		http.Error(w, view.Error, http.StatusConflict)
		return
	}
	jsonReply(w, view)
}

// answerJoin is POST /api/client/devices/join/confirm {check} (一致) and
// POST /api/client/devices/join/reject (不一致) once the other computer said
// 允许. confirm must name the number this window shows; a different number is
// refused and the question stays open. The reply is the join's next state.
func (s *Server) answerJoin(w http.ResponseWriter, r *http.Request, same bool) {
	var input struct {
		Check string `json:"check"`
	}
	if !readJSON(w, r, &input, 1024) {
		return
	}
	d := &s.devices
	d.mu.Lock()
	j := d.join
	if j == nil || j.view.State != "confirm" || j.answer == nil {
		d.mu.Unlock()
		http.Error(w, "没有等待确认的加入", http.StatusConflict)
		return
	}
	if same && input.Check != j.view.Check {
		d.mu.Unlock()
		http.Error(w, "核对数字不一致：只有两台电脑显示相同的数字时才能确认", http.StatusConflict)
		return
	}
	j.answer <- same
	j.answer, j.declined = nil, !same
	changed := j.changed
	d.mu.Unlock()
	select {
	case <-changed:
	case <-r.Context().Done():
	case <-time.After(15 * time.Second):
	}
	view, _ := d.joinView()
	jsonReply(w, view)
}

// joinComputerState is the held read of the short-code join: with wait=N
// (at most 25 seconds) it answers when the state changes.
func (s *Server) joinComputerState(w http.ResponseWriter, r *http.Request) {
	view, changed := s.devices.joinView()
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait = min(max(wait, 0), 25); wait > 0 && changed != nil && (view.State == "finding" || view.State == "approval" || view.State == "confirm") {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-changed:
		case <-timer.C:
		case <-r.Context().Done():
		}
		view, _ = s.devices.joinView()
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, view)
}

func (d *deviceLinks) joinView() (deviceJoinView, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.join == nil {
		return deviceJoinView{State: "none"}, nil
	}
	return d.join.view, d.join.changed
}

// updateJoin records a change of j unless a newer join or a cancel replaced it.
func (d *deviceLinks) updateJoin(j *deviceJoin, view deviceJoinView) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.join != j {
		return
	}
	j.view = view
	close(j.changed)
	j.changed = make(chan struct{})
}

// cancelJoin stops a short-code join and forgets its state.
func (d *deviceLinks) cancelJoin() {
	d.mu.Lock()
	j := d.join
	d.join = nil
	if j != nil {
		close(j.changed)
	}
	d.mu.Unlock()
	if j != nil {
		j.cancel()
	}
}

// codeJoinFailure names the usual causes in the joining window's words.
func codeJoinFailure(err error) string {
	switch {
	case errors.Is(err, mobilelink.ErrDeviceCodeUnknown):
		return "没有找到这个配对码：请核对数字，并确认两台电脑使用同一个中继。"
	case errors.Is(err, mobilelink.ErrDeviceCodeUsed):
		return "配对码已被使用，请在那台电脑上重新生成。"
	case errors.Is(err, context.Canceled):
		return "已取消加入。"
	case errors.Is(err, context.DeadlineExceeded):
		return "等待超时，请在那台电脑上重新生成配对码。"
	}
	return joinFailure(err)
}

func relayDisplay(relay string) string {
	if relay == mobilelink.OfficialRelay {
		return "官方中继"
	}
	return relay
}

// joinFailure keeps the pairing code's own Chinese messages and names the
// usual causes for the relay's.
func joinFailure(err error) string {
	message := err.Error()
	for _, r := range message {
		if r > unicode.MaxASCII {
			return message
		}
	}
	return "没有连上那台电脑：配对码可能已被使用或已过期，请在那台电脑上重新生成，并确认两台电脑使用同一个中继。"
}

func (s *Server) openJoinedComputer(w http.ResponseWriter, r *http.Request, id string) {
	lockCtx, status, err := s.deviceUnlocked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	d := &s.devices
	d.mu.Lock()
	computers, err := s.loadJoinedComputers()
	d.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	var record *joinedComputer
	for i := range computers {
		if computers[i].ID == id {
			record = &computers[i]
		}
	}
	if record == nil {
		http.Error(w, "没有找到这台电脑，请重新加入", http.StatusNotFound)
		return
	}
	d.close(id)
	client, err := nativeclient.OpenSavedKey(record.Relay, record.Lookup, record.HostPin, ed25519.NewKeyFromSeed(record.Seed))
	if err != nil {
		diagnostics.Log("warn", "devices", "open failed: "+err.Error())
		if errors.Is(err, mobilelink.ErrNoInvitation) {
			http.Error(w, "那台电脑现在连不上：请确认它开着 Super Cage，并且没有在“设备”里移除这台电脑。", http.StatusConflict)
		} else {
			http.Error(w, "没有连上那台电脑，请检查网络和中继后重试", http.StatusBadGateway)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	link := &deviceLink{id: id, generation: randomToken(), client: client, api: mobilelink.Operations(client), ctx: ctx, cancel: cancel, restAfter: cmp.Or(d.restAfter, deviceLinkRest)}
	d.mu.Lock()
	if d.closed || lockCtx.Err() != nil {
		d.mu.Unlock()
		link.close()
		http.Error(w, errDeviceLocked.Error(), http.StatusLocked)
		return
	}
	if old := d.links[id]; old != nil {
		old.close()
	}
	if d.links == nil {
		d.links = map[string]*deviceLink{}
	}
	d.links[id] = link
	// The open itself is a positive answer from the shown environment.
	d.unlockedAt = time.Now()
	now := time.Now().UTC()
	if computers, err := s.loadJoinedComputers(); err == nil {
		for i := range computers {
			if computers[i].ID == id {
				computers[i].LastOpen = &now
			}
		}
		_ = s.saveJoinedComputers(computers)
	}
	d.mu.Unlock()
	// Registered once the link is listed, so a lock at any moment of the open
	// (even before this line) removes it from the list too.
	stop := context.AfterFunc(lockCtx, func() { d.drop(link) })
	link.mu.Lock()
	link.stopLock = stop
	link.mu.Unlock()
	link.arm()
	go link.report()
	jsonReply(w, map[string]string{"connection": deviceConnectionPrefix + id + ":" + link.generation, "url": "/mobile.html"})
}

func (s *Server) forgetJoinedComputer(w http.ResponseWriter, id string) {
	d := &s.devices
	d.close(id)
	d.mu.Lock()
	computers, err := s.loadJoinedComputers()
	if err == nil {
		kept := computers[:0]
		for _, c := range computers {
			if c.ID != id {
				kept = append(kept, c)
			}
		}
		err = s.saveJoinedComputers(kept)
	}
	d.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

// routeDevice forwards a window's request on a device: connection. It runs
// after the session cookie and before every target and account gate. Only the
// phone surface (/api/mobile/, filtered again by the link) reaches the joined
// computer; /api/client/ stays with this process so 返回本机 can close the
// link, and every other API request is refused.
func (s *Server) routeDevice(w http.ResponseWriter, r *http.Request) bool {
	connection := requestConnection(r)
	if !strings.HasPrefix(connection, deviceConnectionPrefix) || !strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/api/client/") {
		return false
	}
	switch r.URL.Path {
	case "/api/session", "/api/launch", "/api/handoff":
		return false
	}
	if !s.deviceWindow(w, r) {
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/mobile/") {
		http.Error(w, errDeviceMobileOnly.Error(), http.StatusConflict)
		return true
	}
	link := s.devices.lookup(strings.TrimPrefix(connection, deviceConnectionPrefix))
	if link == nil {
		http.Error(w, errDeviceLinkClosed.Error(), http.StatusConflict)
		return true
	}
	if status, err := s.deviceStillUnlocked(r.Context()); err != nil {
		http.Error(w, err.Error(), status)
		return true
	}
	ctx, finish, err := s.beginActivity(r.Context())
	if err != nil {
		http.Error(w, "后台正在更新，请重新打开 Super Cage。", http.StatusServiceUnavailable)
		return true
	}
	defer finish()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(link.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.WithContext(ctx)
	if r.URL.RawQuery != "" && r.Header.Get(connectionHeader) == "" {
		// The link checks the query strictly; the connection is ours.
		var kept []string
		for _, field := range strings.Split(r.URL.RawQuery, "&") {
			if !strings.HasPrefix(field, "connection=") {
				kept = append(kept, field)
			}
		}
		u := *r.URL
		u.RawQuery = strings.Join(kept, "&")
		r.URL = &u
	}
	link.serve(w, r)
	return true
}

func (d *deviceLinks) lookup(value string) *deviceLink {
	id, generation, ok := strings.Cut(value, ":")
	if !ok || !joinedComputerID.MatchString(id) {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	link := d.links[id]
	if link == nil || link.ctx.Err() != nil || subtle.ConstantTimeCompare([]byte(link.generation), []byte(generation)) != 1 {
		return nil
	}
	return link
}

// close cuts one computer's link at once: its tunnel and every request on it.
func (d *deviceLinks) close(id string) {
	d.mu.Lock()
	link := d.links[id]
	delete(d.links, id)
	d.mu.Unlock()
	if link != nil {
		link.close()
	}
}

// drop closes link and forgets it, unless a newer open replaced it.
func (d *deviceLinks) drop(link *deviceLink) {
	d.mu.Lock()
	if d.links[link.id] == link {
		delete(d.links, link.id)
	}
	d.mu.Unlock()
	link.close()
}

func (d *deviceLinks) closeAll() {
	d.mu.Lock()
	links := d.links
	d.links = nil
	d.unlockedAt = time.Time{}
	d.mu.Unlock()
	for _, link := range links {
		link.close()
	}
	d.cancelJoin()
}

// shutdown closes every link for good, when the process stops.
func (d *deviceLinks) shutdown() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.closeAll()
}

func (l *deviceLink) close() {
	l.cancel()
	l.mu.Lock()
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
	stop := l.stopLock
	l.stopLock = nil
	l.mu.Unlock()
	if stop != nil {
		stop()
	}
	_ = l.client.Close()
}

// arm closes the relay tunnel after a quiet period; the next request reopens
// it (only reads are ever retried, as on the phone).
func (l *deviceLink) arm() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight == 0 && l.ctx.Err() == nil && l.idle == nil {
		l.idle = time.AfterFunc(l.restAfter, l.rest)
	}
}

func (l *deviceLink) rest() {
	l.wake.Lock()
	defer l.wake.Unlock()
	l.mu.Lock()
	quiet := l.inflight == 0 && l.ctx.Err() == nil
	l.idle = nil
	l.mu.Unlock()
	if quiet && !l.resting {
		l.client.Suspend()
		l.resting = true
	}
}

func (l *deviceLink) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.inflight++
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
	l.mu.Unlock()
	l.wake.Lock()
	if l.resting {
		l.resting = false
		l.client.Resume()
	}
	l.wake.Unlock()
	defer func() {
		l.mu.Lock()
		l.inflight--
		l.mu.Unlock()
		l.arm()
	}()
	l.api.ServeHTTP(w, r)
}

// report tells the other computer this computer's platform and name for its
// device list. A host released before device lists accepts only phones and
// refuses this; the link works the same either way.
func (l *deviceLink) report() {
	body, err := json.Marshal(mobilelink.DeviceInfo{Platform: runtime.GOOS, Model: deviceSelfName()})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, 5*time.Second)
	defer cancel()
	_, _ = l.client.Do(ctx, http.MethodPost, "/api/mobile/device", body)
}

// deviceSelfName is this computer's name as other computers list it.
func deviceSelfName() string {
	name, _ := os.Hostname()
	name = strings.TrimSuffix(strings.TrimSpace(name), ".local")
	if name == "" || len(name) > 160 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		name = "Super Cage"
	}
	return name
}
