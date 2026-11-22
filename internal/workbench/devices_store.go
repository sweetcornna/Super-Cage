package workbench

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

// The device list: phones and computers this computer serves over the phone
// link. paired-devices.json holds every device. mobile-paired-device.json, the
// single phone of earlier versions, is kept beside it and stays authoritative
// for its entry (Legacy), so that an older version after a rollback still
// serves that phone, and a phone it removed or re-paired is never resurrected.

const (
	devicesFileName     = "paired-devices.json"
	devicesAsideName    = "paired-devices.unreadable.json"
	legacyPhoneFileName = "mobile-paired-device.json"
	devicesVersion      = 1
	// deviceLimit counts phones and computers together. Each paired device
	// keeps one waiting room on a relay whose room cap all its users share.
	deviceLimit = 5
)

var (
	errDevicesUnreadable = errors.New("设备列表无法读取。请先移除全部设备，再重新添加。")
	errLegacyUnreadable  = errors.New("无法恢复手机配对，请移除旧记录后重试")
)

// phoneRecord is the record of earlier versions: one UI-managed phone per host.
// Its JSON shape is read and written by those versions and must not change.
type phoneRecord struct {
	Seed                      []byte
	Pin, Relay, Lookup, Admin string
	Device                    *mobilelink.DeviceInfo `json:"device,omitempty"`
}

// deviceRecord is one paired device. Seed through Admin are exactly a
// phoneRecord's, so a migrated phone keeps its host identity, lookup hash and
// relay: its app notices nothing.
type deviceRecord struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Platform string     `json:"platform"`
	Name     string     `json:"name"`
	AddedAt  time.Time  `json:"added_at"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Legacy   bool       `json:"legacy,omitempty"`
	Seed     []byte     `json:"seed"`
	Pin      string     `json:"pin"`
	Relay    string     `json:"relay"`
	Lookup   string     `json:"lookup"`
	Admin    string     `json:"admin,omitempty"`
}

type devicesFile struct {
	Version int            `json:"version"`
	Devices []deviceRecord `json:"devices"`
}

func (s *Server) devicesFile() string { return filepath.Join(s.profile, devicesFileName) }
func (s *Server) phoneFile() string   { return filepath.Join(s.profile, legacyPhoneFileName) }

// deviceKind is what a reported platform makes a device. An unreported device
// is shown as a phone, as earlier versions show anything that pairs.
func deviceKind(platform string) string {
	switch platform {
	case "darwin", "windows", "linux":
		return "computer"
	}
	return "phone"
}

func devicePlatform(platform string) bool {
	switch platform {
	case "android", "ios", "darwin", "windows", "linux":
		return true
	}
	return false
}

// deviceName is a model or machine name a device may report about itself.
func deviceName(name string) bool {
	return name != "" && len(name) <= 160 && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

func newDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (r phoneRecord) valid() bool {
	return len(r.Seed) == ed25519.SeedSize && mobilelink.ValidRelayOrigin(r.Relay)
}

// sameCredentials reports whether a device entry is the pairing r describes.
func (d deviceRecord) sameCredentials(r phoneRecord) bool {
	return string(d.Seed) == string(r.Seed) && d.Pin == r.Pin && d.Relay == r.Relay && d.Lookup == r.Lookup && d.Admin == r.Admin
}

func (d deviceRecord) phoneRecord() phoneRecord {
	r := phoneRecord{Seed: d.Seed, Pin: d.Pin, Relay: d.Relay, Lookup: d.Lookup, Admin: d.Admin}
	if d.Platform != "" {
		r.Device = &mobilelink.DeviceInfo{Platform: d.Platform, Model: d.Name}
	}
	return r
}

// The legacy record's report, when there is one, describes the device.
func (d *deviceRecord) adoptReport(r phoneRecord) {
	if r.Device != nil && devicePlatform(r.Device.Platform) && deviceName(r.Device.Model) {
		d.Platform, d.Kind, d.Name = r.Device.Platform, deviceKind(r.Device.Platform), r.Device.Model
	}
}

func importLegacyPhone(r phoneRecord, now time.Time) deviceRecord {
	d := deviceRecord{ID: newDeviceID(), Kind: "phone", AddedAt: now.UTC(), Legacy: true, Seed: r.Seed, Pin: r.Pin, Relay: r.Relay, Lookup: r.Lookup, Admin: r.Admin}
	d.adoptReport(r)
	return d
}

// valid applies the same credential rules as the legacy restore, so that a
// record this file imported is never refused when the file is read back.
func (d deviceRecord) valid() bool {
	id, err := hex.DecodeString(d.ID)
	if err != nil || len(id) != 16 || hex.EncodeToString(id) != d.ID || d.AddedAt.IsZero() {
		return false
	}
	if d.Platform == "" {
		if d.Kind != "phone" || d.Name != "" {
			return false
		}
	} else if !devicePlatform(d.Platform) || d.Kind != deviceKind(d.Platform) || !deviceName(d.Name) {
		return false
	}
	return phoneRecord{Seed: d.Seed, Relay: d.Relay}.valid()
}

// readLegacyPhone returns nil, nil when the file is absent.
func readLegacyPhone(path string) (*phoneRecord, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r phoneRecord
	if json.Unmarshal(b, &r) != nil || !r.valid() {
		return nil, errLegacyUnreadable
	}
	return &r, nil
}

// readDevicesFile returns nil, nil when the file is absent. A file it cannot
// read entirely (another version, a damaged or edited file) is refused as a
// whole and left untouched: nothing in it is served and nothing is written over it.
func readDevicesFile(path string) (*devicesFile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f devicesFile
	if json.Unmarshal(b, &f) != nil || f.Version != devicesVersion || f.Devices == nil {
		return nil, errDevicesUnreadable
	}
	ids, legacy := map[string]bool{}, 0
	for _, d := range f.Devices {
		if !d.valid() || ids[d.ID] {
			return nil, errDevicesUnreadable
		}
		ids[d.ID] = true
		if d.Legacy {
			legacy++
		}
	}
	if legacy > 1 {
		return nil, errDevicesUnreadable
	}
	return &f, nil
}

func writeDevicesFile(path string, devices []deviceRecord) error {
	if devices == nil {
		devices = []deviceRecord{}
	}
	return writePrivate(path, devicesFile{Version: devicesVersion, Devices: devices})
}

// deviceRestore is what loadDevices found on disk.
type deviceRestore struct {
	devices []deviceRecord
	// write: the list differs from paired-devices.json (a first migration or a
	// legacy rule applied) and has to be written.
	write bool
	// unreadable: paired-devices.json could not be read. Nothing is served.
	unreadable error
	// legacy: mobile-paired-device.json exists but could not be read. Its
	// entry is not served; the other devices are.
	legacy error
}

// loadDevices reads both files and applies the migration and legacy rules:
//   - no list yet and a valid legacy phone: import it as the legacy entry;
//   - legacy file missing: drop the legacy entry (an older version removed it);
//   - legacy credentials differ from the entry: re-import (an older version re-paired);
//   - legacy file present and no legacy entry: import it (an older version paired
//     a phone after the list existed);
//   - otherwise the legacy record's device report, if any, describes the entry.
func loadDevices(devicesPath, legacyPath string, now time.Time) deviceRestore {
	var out deviceRestore
	file, err := readDevicesFile(devicesPath)
	if err != nil {
		out.unreadable = err
		return out
	}
	legacy, legacyErr := readLegacyPhone(legacyPath)
	if file != nil {
		out.devices = slices.Clone(file.Devices)
	}
	at := slices.IndexFunc(out.devices, func(d deviceRecord) bool { return d.Legacy })
	switch {
	case legacyErr != nil:
		out.legacy = legacyErr
		if at >= 0 {
			// Not served while its authority is unreadable; kept on disk.
			out.devices = slices.Delete(out.devices, at, at+1)
		}
		return out
	case legacy == nil:
		if at >= 0 {
			out.devices = slices.Delete(out.devices, at, at+1)
			out.write = true
		}
	case at < 0:
		out.devices = append(out.devices, importLegacyPhone(*legacy, now))
		out.write = true
	case !out.devices[at].sameCredentials(*legacy):
		out.devices[at] = importLegacyPhone(*legacy, now)
		out.write = true
	default:
		before := out.devices[at]
		out.devices[at].adoptReport(*legacy)
		out.write = before.Platform != out.devices[at].Platform || before.Name != out.devices[at].Name
	}
	return out
}

// lookupToken makes a lookup capability for a new pairing.
func lookupToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}
