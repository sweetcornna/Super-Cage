package workbench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// testdata/science-cdp.json is the contract between this package's headless
// proof and the Windows shell: the bridge contract version, the exact CDP
// calls for the page, the Science frame and its workers, the policy's
// scenarios (which target gets which calls, what a failure does, when
// auto-attach may end) and the shell's switches. desktop/windows.tests
// checks ScienceFrame.cs against the same file. CAGE_UPDATE_GOLDEN=1
// rewrites it.
func TestScienceCDPGolden(t *testing.T) {
	control, _ := json.Marshal(controlFile{Port: 17777, SciencePort: 17840, ScienceIdentity: &testScienceIdentity, ScienceEnd: "SECRET"})
	golden := map[string]any{
		"contract":         scienceBridgeContract,
		"control":          json.RawMessage(control),
		"page_attach":      sciencePageAttachCalls(),
		"page_detach":      sciencePageDetachCalls(),
		"frame":            scienceFrameCalls(testScienceIdentity),
		"worker":           scienceWorkerCalls(testScienceIdentity),
		"resume":           scienceResumeCalls(),
		"settle_ms":        2000,
		"navigate_wait_ms": scienceNavigateWait.Milliseconds(),
		// How long the window waits for the front's end and for a crashed
		// frame's Inspector.targetCrashed before it removes the frame.
		"end_wait_ms": scienceShellEndWait.Milliseconds(),
		// The shell's end of a frame's session (control.json science_end).
		"end": map[string]any{
			"path":          scienceEndPath,
			"secret_header": scienceEndSecretHeader,
			"label_header":  scienceEndLabelHeader,
			"ended_header":  scienceEndedHeader,
		},
		"crash_wait_ms": scienceCrashWait.Milliseconds(),
		// The proxy proof prepare runs (F5) and the preferences the shell
		// writes before the WebView starts with the switches.
		"proxy_check": map[string]any{
			"url":          scienceProxyCheckURL("NONCE"),
			"loopback_url": scienceProxyLoopbackURL("NONCE"),
			"options":      map[string]any{"disableCache": true, "includeCredentials": false},
			"verified":     scienceProxyVerified,
		},
		"preferences": sciencePreferences(),
		// After auto-attach ends, per session origin the window framed.
		"clear_origin": scienceClearOriginCalls("ORIGIN", "SITE"),
		"scenarios":    scienceScenarios(),
		// The shell's switches; the browser tests replace port 9 (nothing
		// listens) with a recording proxy.
		"backstop": scienceBackstopArgs(9),
	}
	want, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("testdata", "science-cdp.json")
	if os.Getenv("CAGE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatalf("testdata/science-cdp.json is stale; run with CAGE_UPDATE_GOLDEN=1\n%s", want)
	}
}

// A step's target id is "T" + its session; listed gives the target ids
// Target.getTargets returned and the page-level frames known when it was
// asked.
type scienceScenarioStep struct {
	Do      string   `json:"do"`
	Parent  string   `json:"parent,omitempty"`
	Session string   `json:"session,omitempty"`
	Type    string   `json:"type,omitempty"`
	Targets []string `json:"targets,omitempty"`
	Known   []string `json:"known,omitempty"`
	Expect  []string `json:"expect"`
}

type scienceScenario struct {
	Name  string                `json:"name"`
	Steps []scienceScenarioStep `json:"steps"`
}

// scienceExpect renders calls as "<session> <method>" ("page" for the page
// session).
func scienceExpect(session string, calls []cdpCall) []string {
	if session == "" {
		session = "page"
	}
	out := []string{}
	for _, call := range calls {
		out = append(out, session+" "+call.Method)
	}
	return out
}

// scienceScenarios is the frame policy, written out by hand.
func scienceScenarios() []scienceScenario {
	id := testScienceIdentity
	frame, worker, resume := scienceFrameCalls(id), scienceWorkerCalls(id), scienceResumeCalls()
	none := []string{}
	end := []string{"end"}
	attach, detach := scienceExpect("", sciencePageAttachCalls()), scienceExpect("", sciencePageDetachCalls())
	// teardown is detach's order for the page-level guest frames given.
	teardown := func(frames ...string) []string {
		out := []string{"endfront"}
		for _, frame := range frames {
			out = append(out, frame+" crash")
		}
		return append(out, "remove")
	}
	return []scienceScenario{
		{"the frame, its worker and its nested frame get the guest's identity", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "attached", Parent: "F", Session: "N", Type: "iframe", Expect: scienceExpect("N", frame)},
			{Do: "attached", Parent: "W", Session: "V", Type: "worker", Expect: scienceExpect("V", worker)},
		}},
		{"the workbench's own targets run at once and unchanged", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "B", Type: "worker", Expect: scienceExpect("B", resume)},
			{Do: "attached", Session: "S", Type: "service_worker", Expect: scienceExpect("S", resume)},
		}},
		{"a target that dies while it is prepared is left alone", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "failed", Session: "W", Expect: none},
			{Do: "detached", Session: "W", Expect: none},
			{Do: "settle", Session: "W", Expect: none},
		}},
		{"a live target whose call failed stays paused and ends the frame", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "failed", Session: "W", Expect: none},
			{Do: "settle", Session: "W", Expect: end},
		}},
		{"the frame whose own call failed ends", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "failed", Session: "F", Expect: none},
			{Do: "settle", Session: "F", Expect: end},
		}},
		{"any other target in the frame stays paused and ends the frame", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "X", Type: "shared_worker", Expect: end},
		}},
		{"the frame goes in order (end the front, crash, remove); auto-attach ends after every guest target", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "detached", Session: "W", Expect: none},
			{Do: "detached", Session: "F", Expect: detach},
		}},
		{"a frame that appears while detaching stays paused and holds auto-attach", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "detached", Session: "F", Expect: none},
			{Do: "detached", Session: "G", Expect: detach},
		}},
		{"arming again prepares a paused frame and keeps auto-attach", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "detach", Expect: append(teardown(), detach...)},
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "arm", Expect: scienceExpect("G", frame)},
			{Do: "detached", Session: "F", Expect: none},
		}},
		{"a top-level navigation waits until the frame's targets are gone", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "navigate", Expect: teardown("F")},
			{Do: "arm", Expect: end},
			{Do: "detached", Session: "W", Expect: none},
			{Do: "detached", Session: "F", Expect: append(detach, "proceed")},
			{Do: "navigate", Expect: []string{"proceed"}},
		}},
		{"a navigation without a frame goes at once", []scienceScenarioStep{
			{Do: "navigate", Expect: []string{"proceed"}},
			{Do: "arm", Expect: attach},
			{Do: "navigate", Expect: append(append(teardown(), detach...), "proceed")},
		}},
		{"a navigation whose frame does not go in time is dropped", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "navigate", Expect: teardown("F")},
			{Do: "navigate_timeout", Expect: end},
			{Do: "navigate_timeout", Expect: none},
			{Do: "detached", Session: "F", Expect: detach},
		}},
		{"a nested worker's session ends with its parent's", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "attached", Parent: "W", Session: "V", Type: "worker", Expect: scienceExpect("V", worker)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "detached", Session: "F", Expect: detach},
		}},
		{"a paused target under a paused frame goes with it", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "attached", Parent: "G", Session: "H", Type: "worker", Expect: none},
			{Do: "detached", Session: "F", Expect: none},
			{Do: "detached", Session: "G", Expect: detach},
		}},
		{"a frame Target.getTargets no longer lists is gone with everything under it", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "listed", Targets: []string{"TF", "TW"}, Known: []string{"F"}, Expect: none},
			{Do: "listed", Targets: []string{"TW"}, Known: []string{"F"}, Expect: detach},
		}},
		{"a frame that attaches while the targets are listed stays", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "listed", Targets: []string{}, Known: []string{"F"}, Expect: none},
			{Do: "detached", Session: "G", Expect: detach},
		}},
		{"a call that fails while the frame goes ends nothing", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "failed", Session: "W", Expect: none},
			{Do: "settle", Session: "W", Expect: none},
			{Do: "detached", Session: "F", Expect: detach},
		}},
		{"a second detach or a navigation while the frame goes repeats nothing", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: teardown("F")},
			{Do: "detach", Expect: none},
			{Do: "navigate", Expect: none},
			{Do: "detached", Session: "F", Expect: append(detach, "proceed")},
		}},
	}
}

// The Go policy (the browser tests' window) follows the scenarios; the C#
// policy replays the same ones in desktop/windows.tests.
func TestScienceFramePolicyScenarios(t *testing.T) {
	for _, scenario := range scienceScenarios() {
		policy := newScienceFramePolicy()
		for i, step := range scenario.Steps {
			var actions []scienceAction
			switch step.Do {
			case "arm":
				actions = policy.arm(testScienceIdentity)
			case "detach":
				actions = policy.detach()
			case "attached":
				actions = policy.attachedTarget(step.Parent, step.Session, step.Type, "T"+step.Session)
			case "listed":
				actions = policy.listed(step.Targets, step.Known)
			case "failed":
				actions = policy.callFailed(step.Session)
			case "settle":
				actions = policy.settle(step.Session)
			case "detached":
				actions = policy.detachedTarget(step.Session)
			case "navigate":
				actions = policy.navigate()
			case "navigate_timeout":
				actions = policy.navigateTimedOut()
			default:
				t.Fatalf("%s: step %d: unknown %q", scenario.Name, i, step.Do)
			}
			got := []string{}
			for _, action := range actions {
				switch {
				case action.End:
					got = append(got, "end")
					continue
				case action.EndFront:
					got = append(got, "endfront")
					continue
				case action.Crash:
					got = append(got, action.Session+" crash")
					continue
				case action.Remove:
					got = append(got, "remove")
					continue
				case action.Proceed:
					got = append(got, "proceed")
					continue
				}
				got = append(got, scienceExpect(action.Session, []cdpCall{action.Call})...)
			}
			if !reflect.DeepEqual(got, step.Expect) {
				t.Errorf("%s: step %d (%s %s): got %q, want %q", scenario.Name, i, step.Do, step.Session, got, step.Expect)
			}
		}
	}
}

// The workbench document keeps the host's values while the Science frame
// is armed, shown and closed: only the frame's targets are changed.
func TestScienceBrowserWorkbenchKeepsTheHost(t *testing.T) {
	env := newTestScienceFront(t)
	harness := newScienceFrameHarness(t, scienceTestFrameSources(env))
	env.front.hooks.ancestor = func() string { return harness.origin }
	proxyPort, _ := newScienceCanaryProxy(t)
	browser := launchTestBrowser(t, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	page := browser.page(t)
	shell := newScienceShell(t, browser, page)
	shell.load(t, harness.origin+"/")
	read := func() map[string]any {
		value, _ := shell.eval(t, "({ tz: Intl.DateTimeFormat().resolvedOptions().timeZone, locale: Intl.DateTimeFormat().resolvedOptions().locale, offset: new Date().getTimezoneOffset(), language: navigator.language, ua: navigator.userAgent, platform: navigator.platform })").(map[string]any)
		return value
	}
	host := read()
	if host["tz"] != "America/Los_Angeles" || host["language"] == testScienceIdentity.Language || host["ua"] == testScienceIdentity.UserAgent {
		t.Fatalf("host values: %v", host)
	}
	shell.open(t, env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	waitFor(t, 30*time.Second, "the session", func() bool { return env.front.snapshot().Session })
	armed := read()
	shell.close(t)
	waitFor(t, 10*time.Second, "auto-attach to end", func() bool { return !shell.pageAttached() })
	closed := read()
	t.Logf("host %v\narmed %v\nclosed %v", host, armed, closed)
	for _, key := range []string{"tz", "offset", "locale", "language", "ua", "platform"} {
		if armed[key] != host[key] || closed[key] != host[key] {
			t.Errorf("%s changed: host %v, frame open %v, closed %v", key, host[key], armed[key], closed[key])
		}
	}
}
