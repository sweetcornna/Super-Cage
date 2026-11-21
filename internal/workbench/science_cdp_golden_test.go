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
	control, _ := json.Marshal(controlFile{Port: 17777, SciencePort: 17840, ScienceIdentity: &testScienceIdentity})
	golden := map[string]any{
		"contract":    scienceBridgeContract,
		"control":     json.RawMessage(control),
		"page_attach": sciencePageAttachCalls(),
		"page_detach": sciencePageDetachCalls(),
		"frame":       scienceFrameCalls(testScienceIdentity),
		"worker":      scienceWorkerCalls(testScienceIdentity),
		"resume":      scienceResumeCalls(),
		"settle_ms":   2000,
		"scenarios":   scienceScenarios(),
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

// scienceBridgeContract is the scienceFrame bridge version the pane needs
// (claude-science-state.mjs hostRenderContract).
const scienceBridgeContract = 2

type scienceScenarioStep struct {
	Do      string   `json:"do"`
	Parent  string   `json:"parent,omitempty"`
	Session string   `json:"session,omitempty"`
	Type    string   `json:"type,omitempty"`
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
		{"auto-attach ends only after every guest target is gone", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "attached", Parent: "F", Session: "W", Type: "worker", Expect: scienceExpect("W", worker)},
			{Do: "detach", Expect: none},
			{Do: "detached", Session: "W", Expect: none},
			{Do: "detached", Session: "F", Expect: detach},
		}},
		{"a frame that appears while detaching stays paused and holds auto-attach", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: none},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "detached", Session: "F", Expect: none},
			{Do: "detached", Session: "G", Expect: detach},
		}},
		{"arming again prepares a paused frame and keeps auto-attach", []scienceScenarioStep{
			{Do: "arm", Expect: attach},
			{Do: "detach", Expect: detach},
			{Do: "arm", Expect: attach},
			{Do: "attached", Session: "F", Type: "iframe", Expect: scienceExpect("F", frame)},
			{Do: "detach", Expect: none},
			{Do: "attached", Session: "G", Type: "iframe", Expect: none},
			{Do: "arm", Expect: scienceExpect("G", frame)},
			{Do: "detached", Session: "F", Expect: none},
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
				actions = policy.attachedTarget(step.Parent, step.Session, step.Type)
			case "failed":
				actions = policy.callFailed(step.Session)
			case "settle":
				actions = policy.settle(step.Session)
			case "detached":
				actions = policy.detachedTarget(step.Session)
			default:
				t.Fatalf("%s: step %d: unknown %q", scenario.Name, i, step.Do)
			}
			got := []string{}
			for _, action := range actions {
				if action.End {
					got = append(got, "end")
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
