package workbench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testdata/science-cdp.json is the contract between this package's headless
// proof and the Windows shell: for the identity a control file carries, the
// exact CDP calls to arm the frame, to prepare each worker and to disarm.
// desktop/windows.tests checks ScienceFrame.cs against the same file.
// CAGE_UPDATE_GOLDEN=1 rewrites it.
func TestScienceCDPGolden(t *testing.T) {
	control, _ := json.Marshal(controlFile{Port: 17777, SciencePort: 17840, ScienceIdentity: &testScienceIdentity})
	golden := map[string]any{
		"control": json.RawMessage(control),
		"arm":     scienceOverrideCalls(testScienceIdentity),
		"worker":  scienceWorkerCalls(testScienceIdentity),
		"disarm":  scienceDisarmCalls(),
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

// Arming changes the whole page (the workbench document shares the frame's
// renderer); disarming must give the workbench back the host's values.
func TestScienceBrowserArmDisarmRestoresTheHost(t *testing.T) {
	workbench, _ := newScienceWorkbenchPage(t, "'none'", func() string { return "about:blank" })
	browser := launchTestBrowser(t, scienceHostEnv, "--lang=fr-FR")
	session := browser.page(t)
	if err := browser.call(session, "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := browser.call(session, "Page.navigate", map[string]any{"url": workbench + "/"}, nil); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		var result struct {
			Result struct {
				Value map[string]any `json:"value"`
			} `json:"result"`
		}
		expression := "({ tz: Intl.DateTimeFormat().resolvedOptions().timeZone, locale: Intl.DateTimeFormat().resolvedOptions().locale, offset: new Date().getTimezoneOffset(), language: navigator.language, ua: navigator.userAgent, platform: navigator.platform })"
		if err := browser.call(session, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &result); err != nil {
			t.Fatal(err)
		}
		return result.Result.Value
	}
	waitFor(t, 10*time.Second, "the workbench page", func() bool { return read()["tz"] != nil })
	host := read()
	if host["tz"] != "America/Los_Angeles" || host["language"] == testScienceIdentity.Language || host["ua"] == testScienceIdentity.UserAgent {
		t.Fatalf("host values: %v", host)
	}
	browser.armIdentity(t, session, testScienceIdentity, true)
	armed := read()
	if armed["tz"] != "Asia/Tokyo" || armed["language"] != "ja-JP" || armed["ua"] != testScienceIdentity.UserAgent {
		t.Fatalf("armed values: %v", armed)
	}
	for _, call := range scienceDisarmCalls() {
		if err := browser.call(session, call.Method, call.Params, nil); err != nil {
			t.Fatal(err)
		}
	}
	disarmed := read()
	t.Logf("host %v\narmed %v\ndisarmed %v", host, armed, disarmed)
	for _, key := range []string{"tz", "offset", "locale", "language", "ua", "platform"} {
		if disarmed[key] != host[key] {
			t.Errorf("%s after disarm: %v, host %v", key, disarmed[key], host[key])
		}
	}
}
