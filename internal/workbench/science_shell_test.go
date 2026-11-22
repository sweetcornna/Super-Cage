package workbench

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strconv"
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
//     settles it, the frame ends (the pane removes it). While the frame
//     goes, a failed call ends nothing more.
//   - detach: the frame goes in one order (round-2 N1). endfront: the front
//     ends the frame's session, so nothing the frame still sends reaches the
//     daemon; crash: each page-level guest frame's process is crashed, so
//     its document and workers stop while their overrides are still in
//     place (a removed frame's workers can run on for seconds without them:
//     TestScienceBrowserTeardownOutlivesNoWorker); remove: the frame element
//     goes. Auto-attach stays on until the last guest (and paused) target
//     has detached, since turning it off drops every override and lets
//     paused targets run. A second detach while the frame goes repeats
//     nothing.
//   - A target's detach ends every target under it: a nested worker's
//     session ends with its parent's without an event of its own (round-2
//     N2). listed (the page session's Target.getTargets) drops a page-level
//     frame whose target is gone, with everything under it.
//   - navigate (the window's top document is about to navigate): detach, and
//     the navigation is held until the targets are gone and auto-attach is
//     off. Letting it commit first leaves the old frame's document running
//     for a moment after its overrides were dropped
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
	// parents ("" for page level) and target ids of every guest and held
	// target.
	parents map[string]string
	targets map[string]string
}

// scienceAction is one thing the window does, in list order: a call on a
// session ("" is the page), ending the frame (the pane falls back), ending
// the front's session for the frame (EndFront), crashing a frame's process
// (Crash, on Session), removing the frame element, or letting the held
// navigation go.
type scienceAction struct {
	Session  string
	Call     cdpCall
	End      bool
	EndFront bool
	Crash    bool
	Remove   bool
	Proceed  bool
}

func newScienceFramePolicy() *scienceFramePolicy {
	return &scienceFramePolicy{guests: map[string]scienceIdentity{}, held: map[string]bool{}, failed: map[string]bool{},
		parents: map[string]string{}, targets: map[string]string{}}
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
	for _, session := range slices.Sorted(maps.Keys(p.held)) {
		if p.parents[session] != "" {
			continue
		}
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
	if p.detaching {
		return p.maybeDetach()
	}
	p.detaching = true
	actions := []scienceAction{{EndFront: true}}
	for _, session := range slices.Sorted(maps.Keys(p.guests)) {
		if parent, ok := p.parents[session]; ok && parent == "" {
			actions = append(actions, scienceAction{Session: session, Crash: true})
		}
	}
	actions = append(actions, scienceAction{Remove: true})
	return append(actions, p.maybeDetach()...)
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
	return actions
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

func (p *scienceFramePolicy) attachedTarget(parent, session, kind, target string) []scienceAction {
	if parent == "" {
		if kind != "iframe" {
			return scienceActionsOn(session, scienceResumeCalls())
		}
		p.parents[session], p.targets[session] = "", target
		if p.identity == nil {
			p.held[session] = true
			return nil
		}
		p.guests[session] = *p.identity
		return scienceActionsOn(session, scienceFrameCalls(*p.identity))
	}
	p.parents[session], p.targets[session] = parent, target
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
	if _, ok := p.guests[session]; !ok || p.detaching || p.navigating {
		return nil
	}
	return []scienceAction{{End: true}}
}

func (p *scienceFramePolicy) detachedTarget(session string) []scienceAction {
	p.forget(session)
	return p.maybeDetach()
}

// listed is the page session's Target.getTargets: a page-level frame whose
// target is not listed is gone, with everything under it.
func (p *scienceFramePolicy) listed(targets []string) []scienceAction {
	var gone []string
	for session, parent := range p.parents {
		if parent == "" && !slices.Contains(targets, p.targets[session]) {
			gone = append(gone, session)
		}
	}
	for _, session := range gone {
		p.forget(session)
	}
	return p.maybeDetach()
}

// forget drops a target and every target under it.
func (p *scienceFramePolicy) forget(session string) {
	gone := map[string]bool{session: true}
	for changed := true; changed; {
		changed = false
		for child, parent := range p.parents {
			if gone[parent] && !gone[child] {
				gone[child], changed = true, true
			}
		}
	}
	for session := range gone {
		delete(p.guests, session)
		delete(p.held, session)
		delete(p.failed, session)
		delete(p.parents, session)
		delete(p.targets, session)
	}
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

// scienceProxyLoopbackURL is the proof's second address (round-2 N3): any
// proxy that is down fails the first the same way, but only under the
// shell's bypass list does a loopback address other than the listed ones go
// to the proxy; under another proxy, or none, it goes direct and is refused.
// Both must end with net::ERR_PROXY_CONNECTION_FAILED. The pane reports the
// verdict to the server: the proof is the window's own word.
func scienceProxyLoopbackURL(nonce string) string {
	return scienceProxyLoopbackOrigin + "/" + nonce
}

const scienceProxyVerified = "net::ERR_PROXY_CONNECTION_FAILED"

func scienceProxyVerdict(success bool, netErrorName string) string {
	if !success && netErrorName == scienceProxyVerified {
		return "verified"
	}
	return "failed"
}

// proxyCheck is the window's check (ScienceFrameHost.ProxyCheck): both
// addresses, each with the verdict of one load.
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
	verdict, details := "verified", []string{}
	nonce := fmt.Sprintf("%x", time.Now().UnixNano())
	for _, url := range []string{scienceProxyCheckURL(nonce), scienceProxyLoopbackURL(nonce)} {
		var reply struct {
			Resource struct {
				Success        bool   `json:"success"`
				NetErrorName   string `json:"netErrorName"`
				HTTPStatusCode int    `json:"httpStatusCode"`
			} `json:"resource"`
		}
		err := s.browser.call(s.page, "Network.loadNetworkResource", map[string]any{
			"frameId": tree.FrameTree.Frame.ID,
			"url":     url,
			"options": map[string]any{"disableCache": true, "includeCredentials": false},
		}, &reply)
		if err != nil {
			verdict = "failed"
			details = append(details, err.Error())
			continue
		}
		details = append(details, fmt.Sprintf("success=%v error=%q status=%d", reply.Resource.Success, reply.Resource.NetErrorName, reply.Resource.HTTPStatusCode))
		if scienceProxyVerdict(reply.Resource.Success, reply.Resource.NetErrorName) != "verified" {
			// The first address decides already: the second is not loaded.
			verdict = "failed"
			break
		}
	}
	return verdict, strings.Join(details, "; ")
}

// scienceNavigateWait is how long a held navigation waits for the frame's
// targets (ScienceFrameHost.cs waits as long).
var scienceNavigateWait = 5 * time.Second

// scienceShellEndWait bounds the window's POST /_cage/end (the front itself
// waits up to scienceEndWait for the session's requests); scienceCrashWait
// bounds the wait for a crashed frame's Inspector.targetCrashed before the
// frame element is removed (Chrome 157 took about 220 ms, WebView2 157 up
// to about 1 s). Both fit in the held navigation's wait, with time left
// for the frame's targets to go (TestScienceTeardownFitsTheNavigationWait).
// While the frame goes, the window lists its targets every scienceListEvery.
var (
	scienceShellEndWait = 2500 * time.Millisecond
	scienceCrashWait    = 1500 * time.Millisecond
	scienceListEvery    = time.Second
)

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
	// current is the session origin of the frame the window attached for;
	// crashed the frames whose Inspector.targetCrashed is awaited.
	current string
	crashed map[string]chan struct{}
	// noEnd and noCrash leave one step out of the teardown (the tests show
	// each alone keeps host values from the daemon); fronts ended counts the
	// front ends the window asked for and what they answered.
	noEnd, noCrash bool
	endResults     []string
	// journal is where the window records each session origin before it
	// attaches (ScienceFrame.Record), so a start after a kill or a crash
	// clears what that frame stored ("": none).
	journal string
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
	s := &scienceShell{t: t, browser: browser, page: page, workers: true, policy: newScienceFramePolicy(), events: make(chan cdpMessage, 4096),
		origins: map[string]bool{}, crashed: map[string]chan struct{}{}}
	browser.onOrdered(func(message cdpMessage) {
		switch message.Method {
		case "Target.attachedToTarget", "Target.detachedFromTarget":
			s.events <- message
		case "Inspector.targetCrashed":
			s.mu.Lock()
			if done, ok := s.crashed[message.SessionID]; ok {
				delete(s.crashed, message.SessionID)
				close(done)
			}
			s.mu.Unlock()
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
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
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
		actions = s.policy.attachedTarget(parent, event.SessionID, event.TargetInfo.Type, event.TargetInfo.TargetID)
		if !s.workers && event.TargetInfo.Type == "worker" && len(actions) > 0 {
			// The probe's negative case: workers run without the identity.
			actions = scienceActionsOn(event.SessionID, scienceResumeCalls())
		}
	} else {
		actions = s.policy.detachedTarget(event.SessionID)
	}
	s.mu.Unlock()
	go s.run(actions, "")
}

// run performs actions in list order (ScienceFrameHost.Apply): ending the
// front for the frame of origin ending (the session origin the frame was
// attached for), crashing frames without waiting for an answer (Page.crash
// never answers), removing the frame once they crashed (or after
// scienceCrashWait), then the calls, in order per session; a failed call
// stops that session's calls and is settled later.
func (s *scienceShell) run(actions []scienceAction, ending string) {
	bySession := map[string][]cdpCall{}
	var order []string
	if scienceShellTrace {
		for _, action := range actions {
			if action.Session == "" || action.End || action.Crash {
				s.t.Logf("%s action %q end=%v endfront=%v crash=%v remove=%v", time.Now().Format("05.000"), action.Call.Method, action.End, action.EndFront, action.Crash, action.Remove)
			}
		}
	}
	// The origins to clear are the ones framed when the page detach was
	// produced: an attach that follows adds its own for the next one.
	var origins map[string]bool
	for _, action := range actions {
		if action.Session == "" && action.Call.Method == "Target.setAutoAttach" && action.Call.Params["autoAttach"] == false {
			s.mu.Lock()
			origins, s.origins = s.origins, map[string]bool{}
			s.mu.Unlock()
			break
		}
	}
	proceed := false
	var crashes []chan struct{}
	for _, action := range actions {
		switch {
		case action.End:
			s.end()
		case action.EndFront:
			s.endFront(ending)
		case action.Crash:
			crashes = append(crashes, s.crash(action.Session))
		case action.Remove:
			deadline := time.After(scienceCrashWait)
		wait:
			for _, crashed := range crashes {
				select {
				case <-crashed:
				case <-deadline:
					break wait
				}
			}
			_ = s.browser.call(s.page, "Runtime.evaluate", map[string]any{"expression": scienceRemoveFrameScript(ending)}, nil)
			go s.list()
		case action.Proceed:
			proceed = true
		default:
			if _, ok := bySession[action.Session]; !ok {
				order = append(order, action.Session)
			}
			bySession[action.Session] = append(bySession[action.Session], action.Call)
		}
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
					s.run(settled, "")
				})
				return
			}
		}(session, bySession[session])
	}
	wg.Wait()
	for origin := range origins {
		for _, call := range scienceClearOriginCalls(origin, s.site) {
			err := s.browser.call(s.page, call.Method, call.Params, nil)
			if scienceShellTrace {
				s.t.Logf("%s %s: %v", call.Method, origin, err)
			}
		}
		s.cleared.Add(1)
	}
	s.forget(origins)
	if proceed {
		s.mu.Lock()
		target := s.target
		s.mu.Unlock()
		_ = s.browser.call(s.page, "Page.navigate", map[string]any{"url": target}, nil)
		s.navigated.Add(1)
	}
}

// scienceRemoveFrameScript removes the pane's frame elements of one session
// origin (all of them for ""), wherever the pane put them.
func scienceRemoveFrameScript(origin string) string {
	return fmt.Sprintf(`document.querySelectorAll("iframe[data-science-frame]").forEach((frame) => { try { if (%q === "" || new URL(frame.src).origin === %q) frame.remove(); } catch {} }); true`, origin, origin)
}

// endFront is the window's POST /_cage/end for the frame of a session
// origin: the front's port names the workload, whose control.json has the
// secret (here: the test front on that port).
func (s *scienceShell) endFront(origin string) {
	if s.noEnd || origin == "" {
		return
	}
	parsed, err := neturl.Parse(origin)
	if err != nil {
		return
	}
	port, _ := strconv.Atoi(parsed.Port())
	value, ok := scienceTestFronts.Load(port)
	if !ok {
		return
	}
	request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/_cage/end", port), nil)
	request.Header.Set("X-Cage-End", value.(*scienceFront).endSecret)
	request.Header.Set("X-Cage-End-Label", strings.TrimSuffix(parsed.Hostname(), ".localhost"))
	client := &http.Client{Timeout: scienceShellEndWait, Transport: &http.Transport{Proxy: nil}}
	result := "failed"
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		result = response.Header.Get("X-Cage-Ended")
	}
	s.mu.Lock()
	s.endResults = append(s.endResults, result)
	s.mu.Unlock()
}

// crash sends Page.crash to a frame's session without waiting for its
// answer (there is none while the frame lives); the channel closes on the
// frame's Inspector.targetCrashed.
func (s *scienceShell) crash(session string) chan struct{} {
	done := make(chan struct{})
	if s.noCrash {
		close(done)
		return done
	}
	s.mu.Lock()
	s.crashed[session] = done
	s.mu.Unlock()
	go func() { _ = s.browser.call(session, "Page.crash", map[string]any{}, nil) }()
	return done
}

// list is the window's reconciliation while the frame goes: the page
// session's Target.getTargets, every scienceListEvery, until auto-attach
// has ended (at most five times).
func (s *scienceShell) list() {
	for range 5 {
		time.Sleep(scienceListEvery)
		s.mu.Lock()
		going := s.policy.attached && s.policy.detaching
		s.mu.Unlock()
		if !going {
			return
		}
		var reply struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
			} `json:"targetInfos"`
		}
		if s.browser.call(s.page, "Target.getTargets", map[string]any{}, &reply) != nil {
			continue
		}
		var targets []string
		for _, info := range reply.TargetInfos {
			targets = append(targets, info.TargetID)
		}
		s.mu.Lock()
		actions := s.policy.listed(targets)
		s.mu.Unlock()
		s.run(actions, "")
	}
}

// end is the shell telling the pane its frame ended: the pane falls back
// and lets the window detach.
func (s *scienceShell) end() {
	s.endings.Add(1)
	s.detach()
}

func (s *scienceShell) arm() {
	s.mu.Lock()
	actions := s.policy.arm(testScienceIdentity)
	s.mu.Unlock()
	s.run(actions, "")
}

// detach is the window's detach op: it returns once the frame's teardown
// (end, crash, removal) has run.
func (s *scienceShell) detach() {
	s.mu.Lock()
	actions := s.policy.detach()
	ending := s.current
	s.current = ""
	s.mu.Unlock()
	s.run(actions, ending)
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
	return fmt.Sprintf(`(() => { const f = document.createElement("iframe"); f.id = "science"; f.setAttribute("sandbox", %q); f.setAttribute("allow", "clipboard-write"); f.setAttribute("referrerpolicy", "no-referrer"); f.dataset.scienceFrame = ""; f.style.cssText = "width:900px;height:600px;border:0"; f.src = %q; document.getElementById("pane").append(f); return true })()`, scienceFrameSandbox, url)
}

// open is the pane opening its frame: arm, then the entry URL.
func (s *scienceShell) open(t *testing.T, url string) {
	t.Helper()
	s.mu.Lock()
	s.origins[scienceSessionOrigin(url)] = true
	s.current = scienceSessionOrigin(url)
	s.mu.Unlock()
	s.record(t, scienceSessionOrigin(url))
	s.arm()
	s.eval(t, scienceAddFrame(url))
}

// scienceParkFrame is the pane's park step (claude-science.jsx): before
// React removes the frame's wrapper, the frame element moves out of it with
// moveBefore, which keeps the frame's document and workers (and their
// overrides), hidden, until the window's teardown removes it.
const scienceParkFrame = `(() => { const frame = document.querySelector("iframe[data-science-frame]:not([hidden])"); if (!frame) return false; frame.hidden = true; document.body.moveBefore(frame, null); return frame.isConnected })()`

// close is the pane unmounting: the frame parks, then the release (to the
// workbench) and the window's detach go together; it returns once both
// have, as the pane's next start() waits for them.
func (s *scienceShell) close(t *testing.T) {
	t.Helper()
	s.park(t)
	s.mu.Lock()
	ending := s.current
	s.mu.Unlock()
	released := make(chan struct{})
	go func() {
		defer close(released)
		if parsed, err := neturl.Parse(ending); err == nil {
			port, _ := strconv.Atoi(parsed.Port())
			if value, ok := scienceTestFronts.Load(port); ok {
				value.(*scienceFront).endSession("", scienceEndWait)
			}
		}
	}()
	s.detach()
	<-released
}

// vanish is the pane gone without its release (the request failed or never
// left): the window's own end has to do.
func (s *scienceShell) vanish(t *testing.T) {
	t.Helper()
	s.park(t)
	s.detach()
}

func (s *scienceShell) park(t *testing.T) {
	t.Helper()
	if s.eval(t, scienceParkFrame) != true {
		t.Fatal("the frame did not park")
	}
}

// reticket is the pane's one re-ticket (start() in claude-science.jsx):
// the frame goes as on unmount, and start() awaits the release and the
// detach before it asks for the new ticket and attaches for its label.
func (s *scienceShell) reticket(t *testing.T, ticket func() string) {
	t.Helper()
	s.close(t)
	s.open(t, ticket())
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
	ending := s.current
	s.current = ""
	s.mu.Unlock()
	time.AfterFunc(scienceNavigateWait, func() {
		s.mu.Lock()
		timedOut := s.policy.navigateTimedOut()
		s.mu.Unlock()
		s.run(timedOut, "")
	})
	s.run(actions, ending)
	waitFor(t, scienceNavigateWait+time.Second, "the held navigation to go", func() bool { return s.navigated.Load() > before })
}

// scienceJournalEntry is one framed session origin and the window's site
// (ScienceFrame.Record writes the same file).
type scienceJournalEntry struct {
	Origin string `json:"origin"`
	Site   string `json:"site"`
}

func readScienceJournal(path string) []scienceJournalEntry {
	var journal struct {
		Framed []scienceJournalEntry `json:"framed"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &journal) != nil {
		return nil
	}
	return journal.Framed
}

func writeScienceJournal(path string, entries []scienceJournalEntry) error {
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, _ := json.Marshal(map[string]any{"framed": entries})
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// record is the window writing a session origin to its journal before it
// attaches for it.
func (s *scienceShell) record(t *testing.T, origin string) {
	t.Helper()
	if s.journal == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := readScienceJournal(s.journal)
	if !slices.ContainsFunc(entries, func(entry scienceJournalEntry) bool { return entry.Origin == origin }) {
		entries = append(entries, scienceJournalEntry{Origin: origin, Site: s.site})
	}
	if err := writeScienceJournal(s.journal, entries); err != nil {
		t.Fatal(err)
	}
}

// forget drops cleared origins from the journal.
func (s *scienceShell) forget(origins map[string]bool) {
	if s.journal == "" || len(origins) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := slices.DeleteFunc(readScienceJournal(s.journal), func(entry scienceJournalEntry) bool { return origins[entry.Origin] })
	_ = writeScienceJournal(s.journal, entries)
}

// sweep is the window's start: it clears the storage of every origin in
// its journal (frames a killed or crashed window never cleared), then
// forgets them. Without a journal it reads nothing else.
func (s *scienceShell) sweep(t *testing.T) {
	t.Helper()
	if s.journal == "" {
		return
	}
	entries := readScienceJournal(s.journal)
	cleared := map[string]bool{}
	for _, entry := range entries {
		for _, call := range scienceClearOriginCalls(entry.Origin, entry.Site) {
			if err := s.browser.call(s.page, call.Method, call.Params, nil); err != nil {
				t.Logf("%s %s: %v", call.Method, entry.Origin, err)
			}
		}
		cleared[entry.Origin] = true
	}
	s.forget(cleared)
}

// ended counts the frames the shell told the pane to end.
func (s *scienceShell) ended() int { return int(s.endings.Load()) }

// pageAttached reports whether the window's auto-attach is still on.
func (s *scienceShell) pageAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy.attached
}
