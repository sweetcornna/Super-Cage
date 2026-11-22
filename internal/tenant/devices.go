package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// The portal's device table for the cloud entry (design 5.2): one record per
// computer of an account, holding the public half of that computer's device
// key (the same Ed25519 key its ssh uses for the entry). The private key never
// leaves the computer. A device is enrolled from the desktop's link to a
// workspace (POST /api/client/cloud-entry on the workspace's host, grant
// scoped), listed and revoked from the portal. Revoking ends every relay of
// the device at once (its context) and refuses its next admission.
//
// Nothing here runs or allocates while no device is used: the live state is
// made on the first admission and holds a context per device, no goroutine
// and no timer.

const (
	maxAccountDevices = 32
	maxDeviceName     = 64
)

type deviceRecord struct {
	ID        string     `json:"id"`
	Owner     string     `json:"owner"`
	Name      string     `json:"name"`
	PublicKey string     `json:"public_key"`
	Created   time.Time  `json:"created"`
	Revoked   *time.Time `json:"revoked,omitempty"`
}

// deviceLive is a device's state in memory: the context its relays hang on
// and when it last connected (not saved: a write per admission would fsync
// under the server lock).
type deviceLive struct {
	ctx      context.Context
	cancel   context.CancelFunc
	lastUsed time.Time
}

func validDeviceName(name string) bool {
	return name != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= maxDeviceName &&
		strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}

func validDeviceRecord(id string, record deviceRecord, owners map[string]bool) bool {
	_, _, keyOK := cloudentry.ParseAuthorizedKey(record.PublicKey)
	return id == record.ID && cloudentry.ValidDevice(id) && owners[record.Owner] && validDeviceName(record.Name) && keyOK && !record.Created.IsZero()
}

// deviceLiveLocked returns the live state of a device, made on first use.
// Caller holds s.mu.
func (s *Server) deviceLiveLocked(id string) *deviceLive {
	if s.entryState.devices == nil {
		s.entryState.devices = make(map[string]*deviceLive)
	}
	live := s.entryState.devices[id]
	if live == nil {
		ctx, cancel := context.WithCancel(s.context)
		live = &deviceLive{ctx: ctx, cancel: cancel}
		s.entryState.devices[id] = live
	}
	return live
}

// ownerActiveLocked: the account exists. Accounts cannot be suspended or
// removed yet; when they can, this is where it is checked. Caller holds s.mu.
func (s *Server) ownerActiveLocked(owner string) bool {
	for _, record := range s.store.Users {
		if record.ID == owner {
			return true
		}
	}
	return false
}

type deviceView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Created     time.Time  `json:"created"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
	Revoked     bool       `json:"revoked"`
}

// GET /api/tenant/devices: the signed-in account's devices.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	if !cloudentry.Open() {
		notFound(w)
		return
	}
	s.mu.Lock()
	session := s.current(r)
	if session == nil {
		s.mu.Unlock()
		unauthenticated(w)
		return
	}
	views := []deviceView{}
	for _, record := range s.store.Devices {
		if record.Owner != session.user.ID {
			continue
		}
		_, blob, _ := cloudentry.ParseAuthorizedKey(record.PublicKey)
		view := deviceView{ID: record.ID, Name: record.Name, Fingerprint: cloudentry.Fingerprint(blob), Created: record.Created, Revoked: record.Revoked != nil}
		if live := s.entryState.devices[record.ID]; live != nil && !live.lastUsed.IsZero() {
			used := live.lastUsed
			view.LastUsed = &used
		}
		views = append(views, view)
	}
	s.mu.Unlock()
	slices.SortFunc(views, func(a, b deviceView) int { return a.Created.Compare(b.Created) })
	jsonReply(w, http.StatusOK, map[string]any{"devices": views})
}

// POST /api/tenant/devices/{id}/revoke: the device is refused from now on and
// its relays end at once.
func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request, id string) {
	if !cloudentry.Open() {
		notFound(w)
		return
	}
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.expectedSessionLocked(w, r)
	if session == nil {
		return
	}
	record, exists := s.store.Devices[id]
	if !exists || record.Owner != session.user.ID {
		notFound(w)
		return
	}
	if record.Revoked == nil {
		now := s.now().UTC()
		record.Revoked = &now
		updated := s.store.database
		updated.Devices = maps.Clone(updated.Devices)
		updated.Devices[id] = record
		if s.store.save(updated) != nil {
			unavailable(w)
			return
		}
	}
	if live := s.entryState.devices[id]; live != nil {
		live.cancel()
		delete(s.entryState.devices, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// enrolDeviceLocked finds the account's device with this key or adds one.
// A key that was revoked stays refused: the desktop makes a new key. Caller
// holds s.mu.
func (s *Server) enrolDeviceLocked(owner, name, publicKey string, now time.Time) (deviceRecord, string) {
	_, blob, ok := cloudentry.ParseAuthorizedKey(publicKey)
	if !ok || !validDeviceName(name) {
		return deviceRecord{}, "invalid_input"
	}
	count := 0
	for _, record := range s.store.Devices {
		if record.Owner != owner {
			continue
		}
		if _, other, _ := cloudentry.ParseAuthorizedKey(record.PublicKey); string(other) == string(blob) {
			if record.Revoked != nil {
				return deviceRecord{}, "revoked"
			}
			return record, ""
		}
		if record.Revoked == nil {
			count++
		}
	}
	if count >= maxAccountDevices {
		return deviceRecord{}, "device_limit"
	}
	if s.store.blocked {
		return deviceRecord{}, "runtime_unavailable"
	}
	record := deviceRecord{ID: secret(16), Owner: owner, Name: name, PublicKey: "ssh-ed25519 " + strings.Fields(publicKey)[1], Created: now.UTC()}
	updated := s.store.database
	updated.Devices = maps.Clone(updated.Devices)
	if updated.Devices == nil {
		updated.Devices = make(map[string]deviceRecord)
	}
	updated.Devices[record.ID] = record
	if s.store.save(updated) != nil {
		return deviceRecord{}, "runtime_unavailable"
	}
	return record, ""
}

// POST /api/client/cloud-entry on a workspace's host, with the workspace's
// grant: enrol this computer's key for the account (or find it) and add it
// to the workspace's own list, which answers with the endpoint's host key.
// The desktop pins that key; from then on the gateway can neither read nor
// silently intercept the session (design 5.2 step 7).
func (s *Server) clientCloudEntry(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	s.mu.Lock()
	session := s.clientSessionLocked(w, r, id)
	if session == nil {
		s.mu.Unlock()
		return
	}
	now := s.now()
	if !s.allowRate("cloud-entry-enrol", session.user.ID, 30, now) {
		s.mu.Unlock()
		rateLimited(w)
		return
	}
	device, code := s.enrolDeviceLocked(session.user.ID, input.Name, input.PublicKey, now)
	workspace := s.workspaces[id]
	ready := workspace != nil && !workspace.checkedAt.IsZero() && !s.coolingLocked(id)
	s.mu.Unlock()
	switch code {
	case "":
	case "revoked":
		entryRefusal(w, http.StatusForbidden, "revoked", "这台电脑的钥匙已在门户撤销：生成新钥匙后再开启")
		return
	case "device_limit":
		entryRefusal(w, http.StatusConflict, "device_limit", "这个账户登记的电脑已达上限：先在门户撤销不用的电脑")
		return
	case "runtime_unavailable":
		unavailable(w)
		return
	default:
		invalidInput(w)
		return
	}
	if !ready {
		entryRefusal(w, http.StatusConflict, "not_running", "工作区没有在运行：打开工作区后再开启")
		return
	}
	hostKey, err := workspace.enrolEntryDevice(r.Context(), device)
	if err != nil {
		entryRefusal(w, http.StatusServiceUnavailable, "workspace", "工作区没有接受这台电脑：确认工作区已更新到支持本机终端的版本")
		return
	}
	jsonReply(w, http.StatusOK, map[string]string{
		"device_id": device.ID, "host_key": hostKey, "vm_id": id, "host": strings.TrimPrefix(strings.TrimPrefix(s.site.vmOrigin(id), "https://"), "http://"),
	})
}

// enrolEntryDevice adds the device to the workspace's own list over the
// gateway's control connection (never through the proxy, which refuses
// /api/entry/) and returns the endpoint's host key.
func (w *Workspace) enrolEntryDevice(ctx context.Context, device deviceRecord) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"id": device.ID, "name": device.Name, "public_key": device.PublicKey})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.origin.String()+cloudentry.DevicesPath, bytes.NewReader(body))
	if err != nil {
		return "", errController
	}
	request.Header.Set("Origin", w.origin.String())
	request.Header.Set("Content-Type", "application/json")
	if w.cookie != nil {
		request.AddCookie(w.cookie)
	}
	response, err := w.control.RoundTrip(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var reply struct {
		HostKey string `json:"host_key"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || response.StatusCode != http.StatusOK || len(data) > 4096 || json.Unmarshal(data, &reply) != nil {
		return "", errController
	}
	if _, _, ok := cloudentry.ParseAuthorizedKey(reply.HostKey); !ok {
		return "", errController
	}
	return strings.Join(strings.Fields(reply.HostKey)[:2], " "), nil
}

// entryRefusal answers in the entry's own shape, {code, message}, which the
// proxy prints to the owner (entryproxy.HTTPRefusal).
func entryRefusal(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
