package workbench

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

// A mobile-paired-device.json exactly as earlier versions write it.
func legacyPhoneFixture(lookup byte, device string) string {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{lookup}, 32))
	pin := "sha256:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	record := `{"Seed":"` + seed + `","Pin":"` + pin + `","Relay":"` + mobilelink.OfficialRelay + `","Lookup":"` + token + `","Admin":""`
	if device != "" {
		record += `,"device":` + device
	}
	return record + "}"
}

func deviceStorePaths(t *testing.T) (string, string) {
	dir := t.TempDir()
	return filepath.Join(dir, devicesFileName), filepath.Join(dir, legacyPhoneFileName)
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func otherDevice(t *testing.T) deviceRecord {
	t.Helper()
	lookup, err := lookupToken()
	if err != nil {
		t.Fatal(err)
	}
	return deviceRecord{ID: newDeviceID(), Kind: "computer", Platform: "darwin", Name: "工作室 Mac", AddedAt: time.Now().UTC(),
		Seed: bytes.Repeat([]byte{3}, 32), Pin: "sha256:x", Relay: mobilelink.OfficialRelay, Lookup: lookup}
}

// Today's phone becomes one legacy device with the very same pairing, and the
// written list reads back unchanged. The legacy file is not touched.
func TestDevicesMigrateTodaysPhone(t *testing.T) {
	devices, legacy := deviceStorePaths(t)
	fixture := legacyPhoneFixture(1, `{"platform":"android","model":"Google Pixel 9 Pro"}`)
	writeFixture(t, legacy, fixture)
	got := loadDevices(devices, legacy, time.Now())
	if got.unreadable != nil || got.legacy != nil || !got.write || len(got.devices) != 1 {
		t.Fatalf("migration: %+v", got)
	}
	d := got.devices[0]
	var old phoneRecord
	if err := json.Unmarshal([]byte(fixture), &old); err != nil {
		t.Fatal(err)
	}
	if !d.Legacy || !d.sameCredentials(old) || d.Kind != "phone" || d.Platform != "android" || d.Name != "Google Pixel 9 Pro" || len(d.ID) != 32 || d.LastSeen != nil {
		t.Fatalf("migrated device: %+v", d)
	}
	if err := writeDevicesFile(devices, got.devices); err != nil {
		t.Fatal(err)
	}
	again := loadDevices(devices, legacy, time.Now())
	if again.write || again.unreadable != nil || len(again.devices) != 1 || again.devices[0].ID != d.ID {
		t.Fatalf("second start changed the list: %+v", again)
	}
	if b, _ := os.ReadFile(legacy); string(b) != fixture {
		t.Fatal("the legacy file was changed")
	}
	// A phone that never reported keeps no invented model.
	writeFixture(t, legacy, legacyPhoneFixture(1, ""))
	os.Remove(devices)
	bare := loadDevices(devices, legacy, time.Now()).devices
	if len(bare) != 1 || bare[0].Kind != "phone" || bare[0].Platform != "" || bare[0].Name != "" {
		t.Fatalf("unreported legacy phone: %+v", bare)
	}
}

// The legacy file stays authoritative for its entry.
func TestDevicesLegacyShadowRules(t *testing.T) {
	devices, legacy := deviceStorePaths(t)
	other := otherDevice(t)
	writeFixture(t, legacy, legacyPhoneFixture(1, ""))
	first := loadDevices(devices, legacy, time.Now())
	list := append(first.devices, other)
	if err := writeDevicesFile(devices, list); err != nil {
		t.Fatal(err)
	}
	legacyID := first.devices[0].ID

	// Removed on an older version: the entry is dropped, never resurrected.
	os.Remove(legacy)
	got := loadDevices(devices, legacy, time.Now())
	if !got.write || len(got.devices) != 1 || got.devices[0].ID != other.ID {
		t.Fatalf("missing legacy file kept its entry: %+v", got.devices)
	}

	// Re-paired on an older version: re-imported as a new device.
	writeFixture(t, legacy, legacyPhoneFixture(2, ""))
	got = loadDevices(devices, legacy, time.Now())
	if !got.write || len(got.devices) != 2 || got.devices[0].ID == legacyID || !got.devices[0].Legacy || got.devices[0].Lookup != base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) {
		t.Fatalf("re-paired legacy phone: %+v", got.devices)
	}

	// Paired on an older version after the list existed (no legacy entry).
	if err := writeDevicesFile(devices, []deviceRecord{other}); err != nil {
		t.Fatal(err)
	}
	got = loadDevices(devices, legacy, time.Now())
	if !got.write || len(got.devices) != 2 || got.devices[0].ID != other.ID || !got.devices[1].Legacy {
		t.Fatalf("legacy phone paired by an older version: %+v", got.devices)
	}

	// An older version recorded the phone's model: adopted, no other change.
	if err := writeDevicesFile(devices, got.devices); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, legacy, legacyPhoneFixture(2, `{"platform":"ios","model":"iPhone 16 Pro"}`))
	reported := loadDevices(devices, legacy, time.Now())
	if !reported.write || reported.devices[1].ID != got.devices[1].ID || reported.devices[1].Platform != "ios" || reported.devices[1].Name != "iPhone 16 Pro" {
		t.Fatalf("legacy report not adopted: %+v", reported.devices)
	}

	// An unreadable legacy file: its entry is not served, the others are, and
	// nothing is written.
	writeFixture(t, legacy, `{"Seed":"AAAA"}`)
	broken := loadDevices(devices, legacy, time.Now())
	if broken.legacy == nil || broken.write || len(broken.devices) != 1 || broken.devices[0].ID != other.ID {
		t.Fatalf("unreadable legacy file: %+v", broken)
	}
	os.Remove(devices)
	if alone := loadDevices(devices, legacy, time.Now()); alone.legacy == nil || len(alone.devices) != 0 || alone.write {
		t.Fatalf("unreadable legacy file alone: %+v", alone)
	}
}

// A list that cannot be read entirely is refused as a whole and never written
// over: neither its devices nor the legacy phone are served from it.
func TestDevicesMalformedListIsRefusedAndKept(t *testing.T) {
	valid := otherDevice(t)
	encode := func(edit func(*deviceRecord)) string {
		d := valid
		edit(&d)
		b, _ := json.Marshal(devicesFile{Version: 1, Devices: []deviceRecord{d}})
		return string(b)
	}
	cases := map[string]string{
		"not json":         "{",
		"empty":            "",
		"newer version":    `{"version":2,"devices":[]}`,
		"no devices":       `{"version":1}`,
		"bad id":           encode(func(d *deviceRecord) { d.ID = "x" }),
		"upper-case id":    encode(func(d *deviceRecord) { d.ID = strings.ToUpper(d.ID) }),
		"short seed":       encode(func(d *deviceRecord) { d.Seed = d.Seed[:8] }),
		"bad relay":        encode(func(d *deviceRecord) { d.Relay = "http://relay.example" }),
		"unknown platform": encode(func(d *deviceRecord) { d.Platform = "plan9" }),
		"kind mismatch":    encode(func(d *deviceRecord) { d.Kind = "phone" }),
		"control in name":  encode(func(d *deviceRecord) { d.Name = "a\nb" }), // no build wrote it: refused, not sanitised
		"name unreported":  encode(func(d *deviceRecord) { d.Platform, d.Kind = "", "phone" }),
		"no added_at":      encode(func(d *deviceRecord) { d.AddedAt = time.Time{} }),
	}
	twice, _ := json.Marshal(devicesFile{Version: 1, Devices: []deviceRecord{valid, valid}})
	cases["duplicate id"] = string(twice)
	second := valid
	second.ID, second.Legacy = newDeviceID(), true
	first := valid
	first.Legacy = true
	two, _ := json.Marshal(devicesFile{Version: 1, Devices: []deviceRecord{first, second}})
	cases["two legacy entries"] = string(two)
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			devices, legacy := deviceStorePaths(t)
			writeFixture(t, devices, content)
			writeFixture(t, legacy, legacyPhoneFixture(1, ""))
			got := loadDevices(devices, legacy, time.Now())
			if got.unreadable == nil || len(got.devices) != 0 || got.write {
				t.Fatalf("accepted: %+v", got)
			}
			if b, _ := os.ReadFile(devices); string(b) != content {
				t.Fatal("unreadable list was changed")
			}
		})
	}
	// The valid form of the same record reads.
	devices, legacy := deviceStorePaths(t)
	writeFixture(t, devices, encode(func(*deviceRecord) {}))
	if got := loadDevices(devices, legacy, time.Now()); got.unreadable != nil || len(got.devices) != 1 {
		t.Fatalf("valid list refused: %+v", got)
	}
}

// The limit is applied when a device is added, never when a list is read: a
// list written with more devices (by hand, or a later version) is served whole.
func TestDevicesLimitAppliesOnlyToAdding(t *testing.T) {
	devices, legacy := deviceStorePaths(t)
	var list []deviceRecord
	for range deviceLimit + 1 {
		d := otherDevice(t)
		list = append(list, d)
	}
	if err := writeDevicesFile(devices, list); err != nil {
		t.Fatal(err)
	}
	if got := loadDevices(devices, legacy, time.Now()); got.unreadable != nil || len(got.devices) != deviceLimit+1 {
		t.Fatalf("list over the limit: %+v", got)
	}
	if deviceLimit != 5 {
		t.Fatal("the device limit is 5")
	}
}
