package workbench

import (
	"bytes"
	"context"
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
