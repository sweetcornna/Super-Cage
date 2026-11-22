package tenant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeIncus writes an incus CLI that logs every call and answers from script,
// a list of shell case branches matched against all arguments.
func fakeIncus(t *testing.T, branches string) (*IncusRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\ncase \"$*\" in\n" + branches + "\n*) exit 7 ;;\nesac\n"
	writeTestFile(t, filepath.Join(dir, "incus"), script, 0700)
	return &IncusRuntime{cfg: IncusConfig{Executable: filepath.Join(dir, "incus"), Project: "cage-tenant", Profile: "default", ControlPath: "/home/cage/.claude-cage/control.json"}}, calls
}

func readCalls(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestIncusMemoryStateReadsHostProfileAndGuests(t *testing.T) {
	dir := t.TempDir()
	running, frozen, silent, container := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32), strings.Repeat("4", 32)
	meminfo := "MemTotal:       15728640 kB\nMemFree:          400000 kB\nMemAvailable:    3250176 kB\nSwapTotal:       4194304 kB\n"
	guest := "MemTotal:        8189952 kB\nMemFree:          233472 kB\nMemAvailable:    1990000 kB\nBuffers:            1000 kB\n"
	oldMeminfo, oldPressure := hostMeminfo, hostMemoryPressure
	t.Cleanup(func() { hostMeminfo, hostMemoryPressure = oldMeminfo, oldPressure })
	hostMeminfo = writeTestFile(t, filepath.Join(dir, "meminfo"), meminfo, 0600)
	hostMemoryPressure = writeTestFile(t, filepath.Join(dir, "pressure"), "some avg10=12.50 avg60=3.00 avg300=1.00 total=1\nfull avg10=1.00 avg60=0.00 avg300=0.00 total=1\n", 0600)
	writeTestFile(t, filepath.Join(dir, "guest"), guest, 0600)
	inventory := `[
 {"name":"cage-` + running + `","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"` + running + `","limits.memory":"8GiB"},"expanded_config":{"limits.memory":"8GiB"}},
 {"name":"cage-` + frozen + `","type":"virtual-machine","status":"Frozen","config":{"user.cage-tenant.id":"` + frozen + `","volatile.vm.hotplug.memory":"{}"},"expanded_config":{"limits.memory":"10GiB"}},
 {"name":"cage-` + silent + `","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"` + silent + `"},"expanded_config":{"limits.memory":"10GiB"}},
 {"name":"cage-` + container + `","type":"container","status":"Running","config":{"user.cage-tenant.id":"` + container + `"},"expanded_config":{"limits.memory":"2GiB"}},
 {"name":"renamed","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"` + strings.Repeat("5", 32) + `"},"expanded_config":{"limits.memory":"2GiB"}},
 {"name":"cage-` + strings.Repeat("6", 32) + `","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"` + strings.Repeat("6", 32) + `"},"expanded_config":{"limits.memory":"50%"}}
]`
	writeTestFile(t, filepath.Join(dir, "inventory"), inventory, 0600)
	runtime, calls := fakeIncus(t, `"--force-local query local:/1.0/projects/cage-tenant") echo '{"name":"cage-tenant","config":{"features.profiles":"true"}}' ;;
"--force-local query local:/1.0/profiles/default?project=cage-tenant") echo '{"name":"default","config":{"limits.cpu":"4","limits.memory":"10GiB"},"devices":{"root":{"type":"disk","path":"/","pool":"tenant-pool","size":"10GiB"}}}' ;;
"--force-local query local:/1.0/instances?recursion=1&project=cage-tenant") cat '`+filepath.Join(dir, "inventory")+`' ;;
"--force-local --project cage-tenant exec local:cage-`+running+` --mode=non-interactive -- /bin/sh -c "*" sh /home/cage/.claude-cage/machine-memory.json") cat '`+filepath.Join(dir, "guest")+`' ;;
"--force-local --project cage-tenant exec local:cage-`+silent+` --mode=non-interactive -- /bin/sh -c "*) echo 'MemTotal: 1 kB' ;;`)
	state, err := runtime.MemoryState(context.Background())
	if err != nil {
		t.Fatal(err, readCalls(t, calls))
	}
	if state.HostTotal != 15*testGiB || state.HostAvailable != 3250176<<10 || state.HostStall != 12.5 || state.Ceiling != 10*testGiB {
		t.Fatalf("host state %+v", state)
	}
	want := map[string]InstanceMemory{
		running: {Status: "Running", Limit: 8 * testGiB, GuestTotal: 8189952 << 10, GuestAvailable: 1990000 << 10},
		frozen:  {Status: "Frozen", Limit: 10 * testGiB, Hotplugged: true},
		silent:  {Status: "Running", Limit: 10 * testGiB, GuestSilent: true},
	}
	if len(state.Instances) != len(want) {
		t.Fatalf("instances %+v", state.Instances)
	}
	for id, entry := range want {
		if state.Instances[id] != entry {
			t.Fatalf("instance %s = %+v, want %+v", id, state.Instances[id], entry)
		}
	}
	for _, call := range readCalls(t, calls) {
		if strings.Contains(call, "exec local:cage-"+frozen) || strings.Contains(call, "recursion=2") {
			t.Fatalf("unexpected Incus call %q", call)
		}
	}
	// Container runtimes are never resized.
	if (&IncusRuntime{cfg: IncusConfig{InstanceType: InstanceContainer}}).ElasticMemory() || !runtime.ElasticMemory() {
		t.Fatal("elastic memory offered for the wrong instance type")
	}
}

func TestIncusSetMemoryChecksStateAndReportsIncusErrors(t *testing.T) {
	running, stopped, slow := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	record := func(id, status string) string {
		return `echo '{"name":"cage-` + id + `","type":"virtual-machine","status":"` + status + `","config":{"user.cage-tenant.id":"` + id + `"}}'`
	}
	runtime, calls := fakeIncus(t, `"--force-local query local:/1.0/instances/cage-`+running+`?project=cage-tenant") `+record(running, "Running")+` ;;
"--force-local query local:/1.0/instances/cage-`+stopped+`?project=cage-tenant") `+record(stopped, "Stopped")+` ;;
"--force-local query local:/1.0/instances/cage-`+slow+`?project=cage-tenant") `+record(slow, "Running")+` ;;
"--force-local --project cage-tenant config set local:cage-`+running+` limits.memory=4096MiB") exit 0 ;;
"--force-local --project cage-tenant config set local:cage-`+slow+` limits.memory=4096MiB") echo 'Error: Failed setting memory to 4096MiB (currently 6144MiB) as it was taking too long' >&2; exit 1 ;;`)
	ctx := context.Background()
	if err := runtime.SetMemory(ctx, running, 4*testGiB); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetMemory(ctx, stopped, 4*testGiB); !errors.Is(err, errMemoryNotRunning) {
		t.Fatalf("stopped instance: %v", err)
	}
	if err := runtime.SetMemory(ctx, slow, 4*testGiB); err == nil || !strings.Contains(err.Error(), "taking too long") || strings.Contains(err.Error(), "Error:") {
		t.Fatalf("Incus failure reason lost: %v", err)
	}
	before := len(readCalls(t, calls))
	for _, size := range []int64{512 * testMiB, 4*testGiB + 1} {
		if runtime.SetMemory(ctx, running, size) == nil {
			t.Fatalf("size %d accepted", size)
		}
	}
	if runtime.SetMemory(ctx, "../other", 4*testGiB) == nil || len(readCalls(t, calls)) != before {
		t.Fatal("invalid request reached Incus")
	}
	calls1 := readCalls(t, calls)
	if !strings.Contains(strings.Join(calls1, "\n"), "config set local:cage-"+running+" limits.memory=4096MiB") || strings.Contains(strings.Join(calls1, "\n"), "config set local:cage-"+stopped) {
		t.Fatalf("unexpected calls %q", calls1)
	}
}

func TestIncusStopReturnsTheNextBootToTheCeiling(t *testing.T) {
	id := strings.Repeat("1", 32)
	listing := `echo '[{"name":"cage-` + id + `","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"` + id + `","limits.memory":"6144MiB"}}]'`
	runtime, calls := fakeIncus(t, `"--force-local --project cage-tenant list local: --fast --format=json name=cage-`+id+`") `+listing+` ;;
"--force-local --project cage-tenant stop local:cage-`+id+` --timeout=60") exit 0 ;;
"--force-local --project cage-tenant config unset local:cage-`+id+` limits.memory") exit 0 ;;`)
	if err := runtime.Stop(context.Background(), LaunchSpec{VMID: id, SystemID: defaultSystemID}); err != nil {
		t.Fatal(err)
	}
	got := readCalls(t, calls)
	if len(got) != 3 || !strings.HasSuffix(got[1], "stop local:cage-"+id+" --timeout=60") || !strings.HasSuffix(got[2], "config unset local:cage-"+id+" limits.memory") {
		t.Fatalf("calls %q", got)
	}
	// Containers and VMs without an instance-local size need no reset.
	containers := &IncusRuntime{cfg: runtime.cfg}
	containers.cfg.InstanceType = InstanceContainer
	containers.resetMemory(context.Background(), id, map[string]string{"limits.memory": "1GiB"})
	runtime.resetMemory(context.Background(), id, map[string]string{})
	if len(readCalls(t, calls)) != 3 {
		t.Fatal("needless memory reset")
	}
}

func TestMeminfoAndStallParsing(t *testing.T) {
	total, available, err := parseMeminfo([]byte("MemTotal:       1000 kB\nMemAvailable:    250 kB\n"))
	if err != nil || total != 1000<<10 || available != 250<<10 {
		t.Fatal(total, available, err)
	}
	for _, invalid := range []string{"", "MemTotal: 1000 kB\n", "MemTotal: 10 kB\nMemAvailable: 20 kB\n", "MemTotal: x kB\nMemAvailable: 1 kB\n",
		"MemTotal: 0 kB\nMemAvailable: 0 kB\n", "MemTotal: 99999999999999 kB\nMemAvailable: 1 kB\n"} {
		if _, _, err := parseMeminfo([]byte(invalid)); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	for text, want := range map[string]float64{"some avg10=4.20 avg60=1 total=3\n": 4.2, "full avg10=9.00\n": 0, "some avg10=NaN\n": 0, "some avg10=101\n": 0, "": 0} {
		if got := parseMemoryStall([]byte(text)); got != want {
			t.Errorf("%q = %v, want %v", text, got, want)
		}
	}
}

func TestGuestMemoryReadingToleratesMissingOptionalFiles(t *testing.T) {
	now := time.Unix(1e9, 0).UTC()
	meminfo := "MemTotal:        8189952 kB\nMemAvailable:    1990000 kB\n"
	// An old kernel without PSI and a machine without a nested guest: only
	// meminfo is there, which is enough.
	reading, err := parseGuestMemory([]byte(meminfo+"#cage-psi\n#cage-vmstat\n#cage-nested\n"), now)
	if err != nil || reading.GuestTotal != 8189952<<10 || reading.GuestStall != 0 || reading.GuestSwapKnown || reading.NestedShort != 0 {
		t.Fatalf("minimal reading %+v %v", reading, err)
	}
	nested := "{\"version\":1,\"updated_at\":\"" + now.Add(-10*time.Second).Format(time.RFC3339) + "\",\"short_bytes\":3221225472,\"pressure\":true}"
	full := meminfo + "#cage-psi\nsome avg10=12.50 avg60=1.00 avg300=0.00 total=1\nfull avg10=1.00 avg60=0 avg300=0 total=1\n#cage-vmstat\npswpin 4242\n#cage-nested\n" + nested + "\n"
	reading, err = parseGuestMemory([]byte(full), now)
	if err != nil || reading.GuestStall != 12.5 || reading.GuestSwapIns != 4242 || !reading.GuestSwapKnown || reading.NestedShort != 3*testGiB || !reading.NestedPressure {
		t.Fatalf("full reading %+v %v", reading, err)
	}
	// A stale or implausible nested demand is ignored; a broken meminfo is not a reading.
	for _, nested := range []string{"{\"version\":1,\"updated_at\":\"2001-01-01T00:00:00Z\",\"short_bytes\":1073741824}", "{\"version\":2,\"short_bytes\":1}", "garbage"} {
		if reading, err := parseGuestMemory([]byte(meminfo+"#cage-nested\n"+nested+"\n"), now); err != nil || reading.NestedShort != 0 {
			t.Fatalf("nested %q: %+v %v", nested, reading, err)
		}
	}
	if _, err := parseGuestMemory([]byte("#cage-psi\nsome avg10=1.00\n"), now); err == nil {
		t.Fatal("a reading without meminfo was accepted")
	}
}
