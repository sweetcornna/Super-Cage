package workbench

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scienceFramePolicy is what the Windows window does with each DevTools
// target while host rendering may show a frame. ScienceFrame.cs
// (ScienceFramePolicy) implements the same rules; both replay the scenarios
// of testdata/science-cdp.json.
//
//   - arm: the page session auto-attaches (paused) to every new target.
//     Nothing on the workbench document itself changes.
//   - A page-level iframe while armed is the Science frame: it gets the
//     guest's identity (scienceFrameCalls) and only then runs. Unarmed, it
//     stays paused (and is armed if the window arms again).
//   - Any other page-level target is the workbench's own: resumed at once.
//   - Under a guest target: workers get scienceWorkerCalls, frames
//     scienceFrameCalls, anything else stays paused and the frame ends.
//   - A failed call never resumes its target. If the target detaches (it
//     died), nothing more happens; if it is still attached when the window
//     settles it, the frame ends (the pane removes it).
//   - detach: auto-attach stays on until the last guest (and paused) target
//     has detached, since turning it off drops every override and lets
//     paused targets run.
//   - navigate (the window's top document is about to navigate): while a
//     guest target is attached the navigation is held, the window removes
//     the frame, and the navigation goes only once the targets are gone and
//     auto-attach is off. Letting it commit first leaves the old frame's
//     document running for a moment after its overrides were dropped
//     (TestScienceBrowserFrameNeverSeesTheHost/top-level navigation). If
//     the targets do not go in time the navigation is dropped and the frame
//     ends: fail closed. Arming while a navigation is held is refused.
type scienceFramePolicy struct {
	identity   *scienceIdentity
	attached   bool
	detaching  bool
	navigating bool
	guests     map[string]scienceIdentity
	held       map[string]bool
	failed     map[string]bool
}

// scienceAction is one thing the window does: a call on a session ("" is
// the page), ending the frame, removing the frame element before a held
// navigation, or letting the held navigation go.
type scienceAction struct {
	Session string
	Call    cdpCall
	End     bool
	Remove  bool
	Proceed bool
}

func newScienceFramePolicy() *scienceFramePolicy {
	return &scienceFramePolicy{guests: map[string]scienceIdentity{}, held: map[string]bool{}, failed: map[string]bool{}}
}

func scienceActionsOn(session string, calls []cdpCall) []scienceAction {
	actions := make([]scienceAction, 0, len(calls))
	for _, call := range calls {
		actions = append(actions, scienceAction{Session: session, Call: call})
	}
	return actions
}

func (p *scienceFramePolicy) arm(identity scienceIdentity) []scienceAction {
	if p.navigating {
		return []scienceAction{{End: true}}
	}
	p.identity, p.detaching = &identity, false
	var actions []scienceAction
	if !p.attached {
		p.attached = true
		actions = append(actions, scienceActionsOn("", sciencePageAttachCalls())...)
	}
	for session := range p.held {
		delete(p.held, session)
		p.guests[session] = identity
		actions = append(actions, scienceActionsOn(session, scienceFrameCalls(identity))...)
	}
	return actions
}

func (p *scienceFramePolicy) detach() []scienceAction {
	p.identity = nil
	if !p.attached {
		return nil
	}
	p.detaching = true
	return p.maybeDetach()
}

func (p *scienceFramePolicy) maybeDetach() []scienceAction {
	if !p.detaching || p.identity != nil || len(p.guests) > 0 || len(p.held) > 0 {
		return nil
	}
	p.attached, p.detaching = false, false
	actions := scienceActionsOn("", sciencePageDetachCalls())
	if p.navigating {
		p.navigating = false
		actions = append(actions, scienceAction{Proceed: true})
	}
	return actions
}

func (p *scienceFramePolicy) navigate() []scienceAction {
	actions := p.detach()
	if !p.attached {
		return append(actions, scienceAction{Proceed: true})
	}
	p.navigating = true
	return append(actions, scienceAction{Remove: true})
}

// navigateTimedOut: the frame's targets outlived the wait (the shell waits
// scienceNavigateWait).
func (p *scienceFramePolicy) navigateTimedOut() []scienceAction {
	if !p.navigating {
		return nil
	}
	p.navigating = false
	return []scienceAction{{End: true}}
}

func (p *scienceFramePolicy) attachedTarget(parent, session, kind string) []scienceAction {
	if parent == "" {
		if kind != "iframe" {
			return scienceActionsOn(session, scienceResumeCalls())
		}
		if p.identity == nil {
			p.held[session] = true
			return nil
		}
		p.guests[session] = *p.identity
		return scienceActionsOn(session, scienceFrameCalls(*p.identity))
	}
	identity, ok := p.guests[parent]
	if !ok {
		// Under a paused or unknown target: stays paused.
		p.held[session] = true
		return nil
	}
	switch kind {
	case "worker":
		p.guests[session] = identity
		return scienceActionsOn(session, scienceWorkerCalls(identity))
	case "iframe":
		p.guests[session] = identity
		return scienceActionsOn(session, scienceFrameCalls(identity))
	}
	p.held[session] = true
	return []scienceAction{{End: true}}
}

func (p *scienceFramePolicy) callFailed(session string) []scienceAction {
	if _, ok := p.guests[session]; ok {
		p.failed[session] = true
	}
	return nil
}

func (p *scienceFramePolicy) settle(session string) []scienceAction {
	if !p.failed[session] {
		return nil
	}
	delete(p.failed, session)
	if _, ok := p.guests[session]; !ok {
		return nil
	}
	return []scienceAction{{End: true}}
}

func (p *scienceFramePolicy) detachedTarget(session string) []scienceAction {
	delete(p.guests, session)
	delete(p.held, session)
	delete(p.failed, session)
	return p.maybeDetach()
}

var scienceShellTrace = os.Getenv("CAGE_TEST_SHELL_TRACE") == "1"

func short(session string) string {
	if len(session) > 6 {
		return session[:6]
	}
	return session
}

// The window's proxy proof (F5): prepare asks the WebView to load an
// address that cannot exist (.invalid never resolves) under the window's
// own document, with no cookies. Under the shell's switch the proxy at
// 127.0.0.1:9 refuses the connection: net::ERR_PROXY_CONNECTION_FAILED.
// Without it the request fails to resolve, or a system proxy answers. The
// guest cannot take part: the check runs on the page session before the
// frame exists. A dead system proxy fails the same way (see the report).
func scienceProxyCheckURL(nonce string) string {
	return scienceProxyCheckOrigin + "/" + nonce
}

const scienceProxyVerified = "net::ERR_PROXY_CONNECTION_FAILED"

func scienceProxyVerdict(success bool, netErrorName string) string {
	if !success && netErrorName == scienceProxyVerified {
		return "verified"
	}
	return "failed"
}

// proxyCheck is the window's check (ScienceFrameHost.ProxyCheck).
func (s *scienceShell) proxyCheck(t *testing.T) (string, string) {
	t.Helper()
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := s.browser.call(s.page, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Resource struct {
			Success        bool   `json:"success"`
			NetErrorName   string `json:"netErrorName"`
			HTTPStatusCode int    `json:"httpStatusCode"`
		} `json:"resource"`
	}
	err := s.browser.call(s.page, "Network.loadNetworkResource", map[string]any{
		"frameId": tree.FrameTree.Frame.ID,
		"url":     scienceProxyCheckURL(fmt.Sprintf("%x", time.Now().UnixNano())),
		"options": map[string]any{"disableCache": true, "includeCredentials": false},
	}, &reply)
	if err != nil {
		return "failed", err.Error()
	}
	return scienceProxyVerdict(reply.Resource.Success, reply.Resource.NetErrorName), fmt.Sprintf("success=%v error=%q status=%d", reply.Resource.Success, reply.Resource.NetErrorName, reply.Resource.HTTPStatusCode)
}

// scienceNavigateWait is how long a held navigation waits for the frame's
// targets (ScienceFrameHost.cs waits as long).
var scienceNavigateWait = 5 * time.Second

// scienceSettleAfter is how long a target whose call failed may take to
// detach before the window ends the frame (the shell waits as long).
var scienceSettleAfter = time.Second

// scienceShell plays the Windows window and the pane for the browser tests:
// ScienceFrameHost.cs drives scienceFramePolicy with the DevTools protocol,
// claude-science.jsx owns the frame element.
type scienceShell struct {
	t       *testing.T
	browser *testBrowser
	page    string
	workers bool
	mu      sync.Mutex
	policy  *scienceFramePolicy
	events  chan cdpMessage
	endings atomic.Int32
	failed  atomic.Int32
	// The held top-level navigation and whether it went.
	target    string
	navigated atomic.Int32
	// origins are the session origins framed since auto-attach began: their
	// storage is cleared once it ends.
	origins map[string]bool
	cleared atomic.Int32
	// site is the window document's site (scheme and host).
	site string
}

// scienceClearOriginCalls is what the window sends for each session origin
// once the frame's targets are gone: each session has its own origin, so
// what it stored would never be read again, only pile up. The frame's
// storage is partitioned under the window's site (the storage key
// "<origin>/^0<site>", Chromium's serialization, read with
// Storage.getStorageKeyForFrame in Chrome 157); clearDataForOrigin alone
// leaves it. The origin's own key is cleared too, for a browser that does
// not partition.
func scienceClearOriginCalls(origin, site string) []cdpCall {
	return []cdpCall{
		{Method: "Storage.clearDataForStorageKey", Params: map[string]any{"storageKey": origin + "/^0" + site, "storageTypes": "all"}},
		{Method: "Storage.clearDataForOrigin", Params: map[string]any{"origin": origin, "storageTypes": "all"}},
	}
}

// scienceSessionOrigin is the origin of an entry address.
func scienceSessionOrigin(entry string) string { return strings.TrimSuffix(entry, "/_cage/enter") }

// scienceFrameSandbox is the pane's sandbox attribute (claude-science.jsx).
const scienceFrameSandbox = "allow-scripts allow-same-origin allow-forms"

func newScienceShell(t *testing.T, browser *testBrowser, page string) *scienceShell {
	t.Helper()
	s := &scienceShell{t: t, browser: browser, page: page, workers: true, policy: newScienceFramePolicy(), events: make(chan cdpMessage, 4096), origins: map[string]bool{}}
	browser.onOrdered(func(message cdpMessage) {
		if message.Method == "Target.attachedToTarget" || message.Method == "Target.detachedFromTarget" {
			s.events <- message
		}
	})
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case message := <-s.events:
				s.handle(message)
			}
		}
	}()
	return s
}

func (s *scienceShell) handle(message cdpMessage) {
	var event struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	if json.Unmarshal(message.Params, &event) != nil {
		return
	}
	parent := message.SessionID
	if parent == s.page {
		parent = ""
	}
	if scienceShellTrace {
		s.t.Logf("%s %s %s parent=%q type=%s", time.Now().Format("05.000"), message.Method, short(event.SessionID), short(parent), event.TargetInfo.Type)
	}
	s.mu.Lock()
	var actions []scienceAction
	if message.Method == "Target.attachedToTarget" {
		actions = s.policy.attachedTarget(parent, event.SessionID, event.TargetInfo.Type)
		if !s.workers && event.TargetInfo.Type == "worker" && len(actions) > 0 {
			// The probe's negative case: workers run without the identity.
			actions = scienceActionsOn(event.SessionID, scienceResumeCalls())
		}
	} else {
		actions = s.policy.detachedTarget(event.SessionID)
	}
	s.mu.Unlock()
	go s.run(actions)
}

// run performs actions in order per session; a failed call stops that
// session's calls and is settled later.
func (s *scienceShell) run(actions []scienceAction) {
	bySession := map[string][]cdpCall{}
	var order []string
	if scienceShellTrace {
		for _, action := range actions {
			if action.Session == "" || action.End {
				s.t.Logf("%s action %q end=%v", time.Now().Format("05.000"), action.Call.Method, action.End)
			}
		}
	}
	proceed := false
	for _, action := range actions {
		if action.End {
			s.end()
			continue
		}
		if action.Remove {
			_ = s.browser.call(s.page, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('science')?.remove(); true"}, nil)
			continue
		}
		if action.Proceed {
			proceed = true
			continue
		}
		if _, ok := bySession[action.Session]; !ok {
			order = append(order, action.Session)
		}
		bySession[action.Session] = append(bySession[action.Session], action.Call)
	}
	var wg sync.WaitGroup
	for _, session := range order {
		wg.Add(1)
		go func(session string, calls []cdpCall) {
			defer wg.Done()
			target := session
			if target == "" {
				target = s.page
			}
			for _, call := range calls {
				err := s.browser.call(target, call.Method, call.Params, nil)
				if err == nil || scienceBenignOverrideError(err) {
					continue
				}
				s.failed.Add(1)
				s.mu.Lock()
				s.policy.callFailed(session)
				s.mu.Unlock()
				time.AfterFunc(scienceSettleAfter, func() {
					s.mu.Lock()
					settled := s.policy.settle(session)
					s.mu.Unlock()
					s.run(settled)
				})
				return
			}
		}(session, bySession[session])
	}
	wg.Wait()
	for _, action := range actions {
		if action.Session == "" && action.Call.Method == "Target.setAutoAttach" && action.Call.Params["autoAttach"] == false {
			s.mu.Lock()
			origins := s.origins
			s.origins = map[string]bool{}
			s.mu.Unlock()
			for origin := range origins {
				for _, call := range scienceClearOriginCalls(origin, s.site) {
					err := s.browser.call(s.page, call.Method, call.Params, nil)
					if scienceShellTrace {
						s.t.Logf("%s %s: %v", call.Method, origin, err)
					}
				}
				s.cleared.Add(1)
			}
		}
	}
	if proceed {
		s.mu.Lock()
		target := s.target
		s.mu.Unlock()
		_ = s.browser.call(s.page, "Page.navigate", map[string]any{"url": target}, nil)
		s.navigated.Add(1)
	}
}

// end is the shell telling the pane: the pane removes its frame and lets
// the window detach.
func (s *scienceShell) end() {
	s.endings.Add(1)
	_ = s.browser.call(s.page, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('science')?.remove(); true"}, nil)
	s.detach()
}

func (s *scienceShell) arm() {
	s.mu.Lock()
	actions := s.policy.arm(testScienceIdentity)
	s.mu.Unlock()
	s.run(actions)
}

func (s *scienceShell) detach() {
	s.mu.Lock()
	actions := s.policy.detach()
	s.mu.Unlock()
	s.run(actions)
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
	if parsed, err := neturl.Parse(url); err == nil {
		s.mu.Lock()
		s.site = parsed.Scheme + "://" + parsed.Hostname()
		s.mu.Unlock()
	}
	if err := s.browser.call(s.page, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the workbench document", func() bool { return s.eval(t, "!!document.getElementById('pane')") == true })
}

func scienceAddFrame(url string) string {
	return fmt.Sprintf(`(() => { const f = document.createElement("iframe"); f.id = "science"; f.setAttribute("sandbox", %q); f.setAttribute("allow", "clipboard-write"); f.setAttribute("referrerpolicy", "no-referrer"); f.style.cssText = "width:900px;height:600px;border:0"; f.src = %q; document.getElementById("pane").append(f); return true })()`, scienceFrameSandbox, url)
}

// open is the pane opening its frame: arm, then the entry URL.
func (s *scienceShell) open(t *testing.T, url string) {
	t.Helper()
	s.mu.Lock()
	s.origins[scienceSessionOrigin(url)] = true
	s.mu.Unlock()
	s.arm()
	s.eval(t, scienceAddFrame(url))
}

// close is the pane unmounting: the frame goes, then the window detaches
// (which waits for the frame's targets to be gone).
func (s *scienceShell) close(t *testing.T) {
	t.Helper()
	s.eval(t, "document.getElementById('science')?.remove(); true")
	s.detach()
}

// reticket is the pane's one re-ticket: the frame is keyed on its URL, so
// React replaces the element in one commit; the window stays armed.
func (s *scienceShell) reticket(t *testing.T, url string) {
	t.Helper()
	s.mu.Lock()
	s.origins[scienceSessionOrigin(url)] = true
	s.mu.Unlock()
	s.eval(t, "document.getElementById('science')?.remove(); "+scienceAddFrame(url))
}

// navigateTop is the window's top document navigating. Its frame targets
// detach when the new document commits, while the old frame's document may
// still run for a moment (without its overrides): so the window holds the
// navigation, removes the frame, waits for its targets to be gone, and only
// then lets the navigation go (MainWindow.cs cancels and re-issues it).
func (s *scienceShell) navigateTop(t *testing.T, url string) {
	t.Helper()
	before := s.navigated.Load()
	s.mu.Lock()
	s.target = url
	actions := s.policy.navigate()
	s.mu.Unlock()
	time.AfterFunc(scienceNavigateWait, func() {
		s.mu.Lock()
		timedOut := s.policy.navigateTimedOut()
		s.mu.Unlock()
		s.run(timedOut)
	})
	s.run(actions)
	waitFor(t, scienceNavigateWait+time.Second, "the held navigation to go", func() bool { return s.navigated.Load() > before })
}

// ended counts the frames the shell told the pane to end.
func (s *scienceShell) ended() int { return int(s.endings.Load()) }

// pageAttached reports whether the window's auto-attach is still on.
func (s *scienceShell) pageAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy.attached
}
