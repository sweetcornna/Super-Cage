package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// The reviewer's case: a joiner names itself with 61 CJK characters (183
// bytes), which the approval prompt used to accept and the device list does
// not keep. Approving it wrote a computer without a name and the whole list
// was refused at the next start. Now nothing on the way accepts it, nothing is
// written, and the list still loads with the device it had.
func TestDevicesLongJoinerNameNeverPoisonsTheList(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	rig.pair(t)
	long := strings.Repeat("电", 61)
	if remotelink.ValidPairName(long) || deviceName(long) {
		t.Fatalf("a %d-byte name: ValidPairName %v, deviceName %v", len(long), remotelink.ValidPairName(long), deviceName(long))
	}
	// The host never shows it: the joiner refuses to send it, and a joiner
	// that sends it anyway fails the host's check of its hello
	// (remotelink TestPairHostRefusesANameTheListCannotKeep). Its own gate:
	if shortCodeRequestListable(mobilelink.DeviceRequest{ShortCode: true, Name: long, Platform: "darwin"}) ||
		shortCodeRequestListable(mobilelink.DeviceRequest{ShortCode: true, Name: "MacBook Air", Platform: "plan9"}) ||
		!shortCodeRequestListable(mobilelink.DeviceRequest{ShortCode: true, Name: "MacBook Air", Platform: "darwin"}) {
		t.Fatal("the host's own gate before the prompt")
	}
	short, _ := rig.mintShort(t)
	identity, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, err = mobilelink.RedeemDeviceCode(rig.ctx, short, rig.relay, identity, mobilelink.DeviceHello{Name: long, Platform: "darwin"},
		func(string) { t.Error("a check number for a name the list cannot keep") }, func(context.Context, string) bool { return true })
	if err == nil {
		t.Fatal("joined with a name the list cannot keep")
	}
	if view := rig.inviteView(t, ""); view.State != "waiting" || view.Request != nil {
		t.Fatalf("host after the refused join: %+v", view)
	}

	// A writer never puts a record on disk that the next start could not read.
	before, err := os.ReadFile(rig.s.devicesFile())
	if err != nil {
		t.Fatal(err)
	}
	rig.s.phones.mu.Lock()
	records := []deviceRecord{rig.s.phones.devices[0].deviceRecord}
	rig.s.phones.mu.Unlock()
	nameless := deviceRecord{ID: newDeviceID(), Kind: "computer", Platform: "darwin", AddedAt: time.Now().UTC(), Seed: bytes.Repeat([]byte{5}, 32), Relay: rig.relay}
	if err := writeDevicesFile(rig.s.devicesFile(), append(records, nameless)); err == nil {
		t.Fatal("wrote a computer without a name")
	}
	if after, _ := os.ReadFile(rig.s.devicesFile()); !bytes.Equal(before, after) {
		t.Fatal("the refused write changed the list")
	}
	got := loadDevices(rig.s.devicesFile(), rig.s.phoneFile(), time.Now())
	if got.unreadable != nil || len(got.devices) != 1 || got.devices[0].ID != records[0].ID {
		t.Fatalf("next start: unreadable %v, %d devices", got.unreadable, len(got.devices))
	}
}

// writeDevicesFile applies readDevicesFile's rules: a record that would not
// read back, a repeated id or a second legacy entry is an error, and the file
// on disk is left as it was.
func TestDevicesWriterRefusesWhatItCouldNotRead(t *testing.T) {
	devices, _ := deviceStorePaths(t)
	valid := otherDevice(t)
	if err := writeDevicesFile(devices, []deviceRecord{valid}); err != nil {
		t.Fatal(err)
	}
	kept, _ := os.ReadFile(devices)
	edit := func(change func(*deviceRecord)) deviceRecord {
		d := valid
		change(&d)
		return d
	}
	legacy := edit(func(d *deviceRecord) { d.Legacy = true })
	second := edit(func(d *deviceRecord) { d.ID, d.Legacy = newDeviceID(), true })
	for name, list := range map[string][]deviceRecord{
		"computer without a name": {edit(func(d *deviceRecord) { d.Name = "" })},
		"name over 160 bytes":     {edit(func(d *deviceRecord) { d.Name = strings.Repeat("电", 61) })},
		"bidi override in name":   {edit(func(d *deviceRecord) { d.Name = "Mac\u202Eexe" })},
		"bad id":                  {edit(func(d *deviceRecord) { d.ID = "x" })},
		"duplicate id":            {valid, valid},
		"two legacy entries":      {legacy, second},
	} {
		if err := writeDevicesFile(devices, list); err == nil {
			t.Errorf("%s: written", name)
		}
		if after, _ := os.ReadFile(devices); !bytes.Equal(kept, after) {
			t.Fatalf("%s: the list on disk changed", name)
		}
	}
}

// A paired device may rename itself after approval, but only with what the
// approval prompt would show: no bidirectional overrides, isolates,
// zero-width or line-separator characters, at most 160 bytes. A refused name
// keeps the previous one, and a report that changes nothing writes nothing.
func TestDevicesRenameKeepsPrintableNames(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	phone := rig.pair(t)
	client := rig.connect(t, phone)
	report := func(body string) int {
		t.Helper()
		reply, err := client.Do(rig.ctx, "POST", "/api/mobile/device", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return reply.Status
	}
	if status := report(`{"platform":"android","model":"Pixel 9 测试机"}`); status != http.StatusOK {
		t.Fatalf("first report: HTTP %d", status)
	}
	stat := func() os.FileInfo {
		t.Helper()
		info, err := os.Stat(rig.s.devicesFile())
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	written := stat()
	if status := report(`{"platform":"android","model":"Pixel 9 测试机"}`); status != http.StatusOK {
		t.Fatalf("the same report: HTTP %d", status)
	}
	for _, model := range []string{"Pixel\u202E9", "\u2066Pixel\u2069", "Pix\u200Bel", "Pix\u2028el", "Pixel\u00AD9", strings.Repeat("电", 54)} {
		body, _ := json.Marshal(mobilelink.DeviceInfo{Platform: "android", Model: model})
		if status := report(string(body)); status != http.StatusBadRequest {
			t.Errorf("rename to %q: HTTP %d", model, status)
		}
	}
	if !os.SameFile(written, stat()) {
		t.Fatal("paired-devices.json was rewritten by reports that changed nothing")
	}
	if d := rig.device(t, phone); d.Name != "Pixel 9 测试机" || d.Platform != "android" {
		t.Fatalf("after refused renames: %q %q", d.Platform, d.Name)
	}
	if status := report(`{"platform":"android","model":"Pixel 9 Pro"}`); status != http.StatusOK || rig.device(t, phone).Name != "Pixel 9 Pro" || os.SameFile(written, stat()) {
		t.Fatalf("a printable rename: HTTP %d, %q", status, rig.device(t, phone).Name)
	}
}

// A list written by an older build, whose names only had to be free of
// control characters, still loads when a name has characters the stricter
// rule refuses: they are left out (a name of nothing else becomes a plain
// one), and the other records and the list stay as they were. A name an
// older build never wrote (a control character) still makes the file
// unreadable, as any edited file.
func TestDevicesOlderNamesStillLoad(t *testing.T) {
	devices, legacy := deviceStorePaths(t)
	named := func(platform, kind, name string) deviceRecord {
		d := otherDevice(t)
		d.ID, d.Platform, d.Kind, d.Name = newDeviceID(), platform, kind, name
		return d
	}
	records := []deviceRecord{
		named("darwin", "computer", "Studio\u202Egpj.exe"),
		named("windows", "computer", "\u200B\u2066\u2069"),
		named("ios", "phone", " 小明的 iPhone\u200D "),
		named("linux", "computer", "实验室工作站"),
		named("", "phone", ""),
	}
	content, _ := json.Marshal(devicesFile{Version: 1, Devices: records})
	writeFixture(t, devices, string(content))
	got := loadDevices(devices, legacy, time.Now())
	if got.unreadable != nil || len(got.devices) != len(records) {
		t.Fatalf("older names: unreadable %v, %d devices", got.unreadable, len(got.devices))
	}
	want := []string{"Studiogpj.exe", deviceDefaultName, "小明的 iPhone", "实验室工作站", ""}
	for i, d := range got.devices {
		if d.ID != records[i].ID || d.Name != want[i] || !d.valid() {
			t.Errorf("device %d: %q, want %q (valid %v)", i, d.Name, want[i], d.valid())
		}
	}
	if b, _ := os.ReadFile(devices); string(b) != string(content) {
		t.Fatal("loading changed the file")
	}
	if err := writeDevicesFile(devices, got.devices); err != nil {
		t.Fatalf("the loaded list cannot be written back: %v", err)
	}
	if again := loadDevices(devices, legacy, time.Now()); again.unreadable != nil || again.devices[0].Name != want[0] {
		t.Fatalf("written back: %+v", again)
	}
	if deviceName("Mac\u202Eexe") || deviceName("a\u2028b") || deviceName(strings.Repeat("a", 161)) || !deviceName(strings.Repeat("a", 160)) || !deviceName("MacBook Air") {
		t.Fatal("deviceName")
	}
}
