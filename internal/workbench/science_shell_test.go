package workbench

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scienceShell plays the Windows window and the pane for the browser tests:
// what desktop/windows/ScienceFrameHost.cs does with the DevTools protocol
// and what claude-science.jsx does with the frame element.
//
// This is a transcription of the shell and pane as they are at a566772
// (page-wide overrides; a failed worker call resumes the worker and disarms
// the whole WebView; navigation and re-ticket disarm at once), kept only to
// record the review-3 tests failing before the redesign.
type scienceShell struct {
	browser *testBrowser
	page    string
	mu      sync.Mutex
	armed   bool
	endings atomic.Int32
}

// scienceFrameSandbox is the pane's sandbox attribute (claude-science.jsx).
const scienceFrameSandbox = "allow-scripts allow-same-origin allow-forms allow-modals"

func newScienceShell(t *testing.T, browser *testBrowser, page string) *scienceShell {
	t.Helper()
	s := &scienceShell{browser: browser, page: page}
	browser.on(s.attached)
	return s
}

func (s *scienceShell) attached(message cdpMessage) {
	if message.Method != "Target.attachedToTarget" {
		return
	}
	var attached struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	if json.Unmarshal(message.Params, &attached) != nil {
		return
	}
	s.mu.Lock()
	armed := s.armed
	s.mu.Unlock()
	resume := []cdpCall{{"Runtime.runIfWaitingForDebugger", map[string]any{}}}
	calls := resume
	if armed && attached.TargetInfo.Type == "worker" {
		calls = scienceWorkerCalls(testScienceIdentity)
	}
	for _, call := range calls {
		err := s.browser.call(attached.SessionID, call.Method, call.Params, nil)
		if err == nil || scienceBenignOverrideError(err) {
			continue
		}
		// ScienceFrameHost.Attached: resume, disarm the WebView, then tell the pane.
		_ = s.browser.call(attached.SessionID, "Runtime.runIfWaitingForDebugger", map[string]any{}, nil)
		if armed && attached.TargetInfo.Type == "worker" {
			s.disarm()
			s.endings.Add(1)
		}
		return
	}
}

func (s *scienceShell) arm(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
	for _, call := range scienceOverrideCalls(testScienceIdentity) {
		if err := s.browser.call(s.page, call.Method, call.Params, nil); err != nil && !scienceBenignOverrideError(err) {
			t.Fatal(err)
		}
	}
}

func (s *scienceShell) disarm() {
	s.mu.Lock()
	s.armed = false
	s.mu.Unlock()
	for _, call := range scienceDisarmCalls() {
		_ = s.browser.call(s.page, call.Method, call.Params, nil)
	}
}

func (s *scienceShell) eval(t *testing.T, expression string) any {
	t.Helper()
	var result struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := s.browser.call(s.page, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &result); err != nil {
		t.Fatal(err)
	}
	return result.Result.Value
}

// load opens the workbench document in the window.
func (s *scienceShell) load(t *testing.T, url string) {
	t.Helper()
	if err := s.browser.call(s.page, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the workbench document", func() bool { return s.eval(t, "!!document.getElementById('pane')") == true })
}

func (s *scienceShell) addFrame(t *testing.T, url string) {
	t.Helper()
	s.eval(t, fmt.Sprintf(`(() => { const f = document.createElement("iframe"); f.id = "science"; f.setAttribute("sandbox", %q); f.setAttribute("allow", "clipboard-write"); f.setAttribute("referrerpolicy", "no-referrer"); f.style.cssText = "width:900px;height:600px;border:0"; f.src = %q; document.getElementById("pane").append(f); return true })()`, scienceFrameSandbox, url))
}

func (s *scienceShell) frameGone(t *testing.T) bool {
	return s.eval(t, "!document.getElementById('science')") == true
}

// open is the pane opening its frame: prepare and arm, then the ticket URL.
func (s *scienceShell) open(t *testing.T, url string) {
	t.Helper()
	s.arm(t)
	s.addFrame(t, url)
}

// close is the pane unmounting: React removes the frame, then the cleanup
// disarms.
func (s *scienceShell) close(t *testing.T) {
	t.Helper()
	s.eval(t, "document.getElementById('science').remove(); true")
	s.disarm()
}

// reticket is the pane's one re-ticket: it clears the frame URL (React
// renders it a moment later), prepares (disarm), arms and sets the new URL.
func (s *scienceShell) reticket(t *testing.T, url string) {
	t.Helper()
	s.eval(t, "setTimeout(() => document.getElementById('science').remove(), 300); true")
	s.disarm()
	waitFor(t, 5*time.Second, "the old frame to go", func() bool { return s.frameGone(t) })
	s.arm(t)
	s.addFrame(t, url)
}

// navigateTop is the window's top document navigating (MainWindow's
// NavigationStarting disarms first).
func (s *scienceShell) navigateTop(t *testing.T, url string) {
	t.Helper()
	s.disarm()
	if err := s.browser.call(s.page, "Page.navigate", map[string]any{"url": url}, nil); err != nil && !strings.Contains(err.Error(), "no reply") {
		t.Fatal(err)
	}
}

// ended counts the frames the shell told the pane to end.
func (s *scienceShell) ended() int { return int(s.endings.Load()) }
