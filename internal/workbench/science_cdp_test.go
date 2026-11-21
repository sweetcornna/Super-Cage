package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A minimal Chrome DevTools Protocol client for the headless browser tests
// (science_browser_test.go). It drives Chromium the way the Windows shell
// drives WebView2 (CallDevToolsProtocolMethod / ForSession), so the override
// sequence tested here is the one desktop/windows/ScienceFrame.cs sends.

type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Message string `json:"message"`
}

type cdpClient struct {
	conn    *websocket.Conn
	write   sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[int]chan cdpMessage
	events  []func(cdpMessage)
}

func (c *cdpClient) call(session, method string, params any, out any) error {
	if params == nil {
		params = struct{}{}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	reply := make(chan cdpMessage, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	c.write.Lock()
	err = c.conn.WriteJSON(cdpMessage{ID: id, Method: method, Params: raw, SessionID: session})
	c.write.Unlock()
	if err != nil {
		return err
	}
	select {
	case message := <-reply:
		if message.Error != nil {
			return fmt.Errorf("%s: %s", method, message.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(message.Result, out)
		}
		return nil
	case <-time.After(15 * time.Second):
		return fmt.Errorf("%s: no reply", method)
	}
}

// on registers an event handler. Handlers run on their own goroutine, so
// they may call back into the browser.
func (c *cdpClient) on(handler func(cdpMessage)) {
	c.mu.Lock()
	c.events = append(c.events, handler)
	c.mu.Unlock()
}

func (c *cdpClient) read() {
	for {
		var message cdpMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			c.mu.Lock()
			for id, reply := range c.pending {
				reply <- cdpMessage{ID: id, Error: &cdpError{Message: "connection closed"}}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		if message.ID != 0 {
			if reply, ok := c.pending[message.ID]; ok {
				delete(c.pending, message.ID)
				reply <- message
			}
			c.mu.Unlock()
			continue
		}
		handlers := make([]func(cdpMessage), len(c.events))
		copy(handlers, c.events)
		c.mu.Unlock()
		for _, handler := range handlers {
			go handler(message)
		}
	}
}

type testBrowser struct {
	*cdpClient
	cmd    *exec.Cmd
	netLog string
}

var devToolsListening = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

// launchTestBrowser starts one headless Chromium (the brief allows one at a
// time; the tests using it are not parallel) and stops it by its own PID.
func launchTestBrowser(t *testing.T, env []string, args ...string) *testBrowser {
	t.Helper()
	return launchTestBrowserWithPreferences(t, nil, env, args...)
}

// launchTestBrowserWithPreferences seeds the profile's Preferences file
// first (the WebView2 user data folder has the same file).
func launchTestBrowserWithPreferences(t *testing.T, preferences map[string]any, env []string, args ...string) *testBrowser {
	t.Helper()
	path := os.Getenv("CAGE_TEST_BROWSER_PATH")
	if path == "" {
		t.Skip("CAGE_TEST_BROWSER_PATH is not set")
	}
	profile := t.TempDir()
	if preferences != nil {
		data, _ := json.Marshal(preferences)
		if err := os.MkdirAll(profile+"/Default", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(profile+"/Default/Preferences", data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	netLog := profile + "/netlog.json"
	base := []string{
		"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--disable-extensions",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--disable-features=OptimizationHints,MediaRouter,Translate", "--mute-audio",
		"--log-net-log=" + netLog, "--net-log-capture-mode=Default",
	}
	cmd := exec.Command(path, append(append(base, args...), "about:blank")...)
	cmd.Env = append(os.Environ(), env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	browser := &testBrowser{cmd: cmd, netLog: netLog}
	endpoint := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		sent := false
		for scanner.Scan() {
			if match := devToolsListening.FindStringSubmatch(scanner.Text()); match != nil && !sent {
				endpoint <- match[1]
				sent = true
			}
		}
	}()
	t.Cleanup(browser.stop)
	var url string
	select {
	case url = <-endpoint:
	case <-time.After(30 * time.Second):
		t.Fatal("Chromium did not start")
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(64 << 20)
	browser.cdpClient = &cdpClient{conn: conn, pending: map[int]chan cdpMessage{}}
	go browser.read()
	return browser
}

// stop closes the browser (so the net log is finished) and reaps it.
func (b *testBrowser) stop() {
	if b.cmd.ProcessState != nil {
		return
	}
	if b.cdpClient != nil {
		_ = b.call("", "Browser.close", nil, nil)
		_ = b.conn.Close()
	}
	done := make(chan struct{})
	go func() { _ = b.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = b.cmd.Process.Kill()
		<-done
	}
}

// page opens a tab and attaches to it.
func (b *testBrowser) page(t *testing.T) string {
	t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	return attached.SessionID
}

// cdpCall is one protocol call of the shell's override sequence.
type cdpCall struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// scienceWorkerPlatformPatch is what the shell evaluates in each worker
// before it runs: CDP has no navigator.platform override for workers.
const scienceWorkerPlatformPatch = "Object.defineProperty(WorkerNavigator.prototype, 'platform', { get() { return %s; }, configurable: true, enumerable: true }); true"

// scienceOverrideCalls is the page-level sequence (layer 1). The worker
// sequence is scienceWorkerCalls. ScienceFrame.cs builds the same calls;
// both are checked against testdata/science-cdp.json.
func scienceOverrideCalls(identity scienceIdentity) []cdpCall {
	return []cdpCall{
		{"Emulation.setTimezoneOverride", map[string]any{"timezoneId": identity.Timezone}},
		{"Emulation.setLocaleOverride", map[string]any{"locale": identity.Locale}},
		{"Emulation.setUserAgentOverride", scienceUserAgentParams(identity)},
		// No host file reaches the page (R5): the chooser never opens and
		// the page sees a cancel. Interception needs the Page domain.
		{"Page.enable", map[string]any{}},
		{"Page.setInterceptFileChooserDialog", map[string]any{"enabled": true, "cancel": true}},
		{"Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}},
	}
}

func scienceWorkerCalls(identity scienceIdentity) []cdpCall {
	platform, _ := json.Marshal(identity.Platform)
	return []cdpCall{
		{"Emulation.setUserAgentOverride", scienceUserAgentParams(identity)},
		{"Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(scienceWorkerPlatformPatch, platform)}},
		{"Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}},
		{"Runtime.runIfWaitingForDebugger", map[string]any{}},
	}
}

// scienceUserAgentParams: ua_metadata in the control file already has the
// CDP UserAgentMetadata shape; the shell passes it through.
func scienceUserAgentParams(identity scienceIdentity) map[string]any {
	return map[string]any{"userAgent": identity.UserAgent, "acceptLanguage": identity.AcceptLanguage, "platform": identity.Platform, "userAgentMetadata": identity.Metadata}
}

// scienceDisarmCalls clears the page-level overrides and stops attaching to
// new workers.
func scienceDisarmCalls() []cdpCall {
	return []cdpCall{
		{"Target.setAutoAttach", map[string]any{"autoAttach": false, "waitForDebuggerOnStart": false, "flatten": true}},
		{"Emulation.setTimezoneOverride", map[string]any{"timezoneId": ""}},
		{"Emulation.setLocaleOverride", map[string]any{}},
		{"Emulation.setUserAgentOverride", map[string]any{"userAgent": ""}},
		{"Page.setInterceptFileChooserDialog", map[string]any{"enabled": false}},
		{"Page.disable", map[string]any{}},
	}
}

// scienceBenignOverrideError: the locale override is renderer-process wide,
// so a second target in the same process reports it as already set.
func scienceBenignOverrideError(err error) bool {
	return strings.Contains(err.Error(), "Another locale override is already in effect")
}

// armIdentity applies layer 1 to a page session the way the shell does. With
// workers false only the page-level overrides are sent (to show that layer 2
// notices what that misses).
func (b *testBrowser) armIdentity(t *testing.T, session string, identity scienceIdentity, workers bool) *sync.Map {
	t.Helper()
	seen := &sync.Map{}
	calls := scienceOverrideCalls(identity)
	if !workers {
		calls = calls[:3]
	}
	if workers {
		b.on(func(message cdpMessage) {
			if message.Method != "Target.attachedToTarget" {
				return
			}
			var attached struct {
				SessionID  string `json:"sessionId"`
				TargetInfo struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"targetInfo"`
			}
			if json.Unmarshal(message.Params, &attached) != nil {
				return
			}
			seen.Store(attached.SessionID, attached.TargetInfo.Type)
			t.Logf("attached %s %s", attached.TargetInfo.Type, attached.TargetInfo.URL)
			calls := scienceWorkerCalls(identity)
			if attached.TargetInfo.Type == "iframe" {
				// A cross-site frame (never legitimate: the front's CSP
				// allows 'self' only, and the shell's frame policy refuses
				// it) still gets the page's overrides before it runs.
				calls = append(scienceOverrideCalls(identity), cdpCall{"Runtime.runIfWaitingForDebugger", map[string]any{}})
			}
			if attached.TargetInfo.Type == "service_worker" || attached.TargetInfo.Type == "shared_worker" {
				// These targets take no Emulation calls (they hang). The
				// front refuses their scripts, so resuming lets the
				// registration fail instead of hanging.
				calls = []cdpCall{{"Runtime.runIfWaitingForDebugger", map[string]any{}}}
			}
			for _, call := range calls {
				if err := b.call(attached.SessionID, call.Method, call.Params, nil); err != nil && !scienceBenignOverrideError(err) {
					t.Logf("%s %s: %v", attached.TargetInfo.Type, call.Method, err)
					return
				}
			}
		})
	}
	for _, call := range calls {
		if err := b.call(session, call.Method, call.Params, nil); err != nil {
			t.Fatal(err)
		}
	}
	return seen
}

// captureConsole records console messages, exceptions and browser log
// entries of the page and its frames (for failure reports).
func (b *testBrowser) captureConsole(t *testing.T, session string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	b.on(func(message cdpMessage) {
		switch message.Method {
		case "Runtime.consoleAPICalled", "Runtime.exceptionThrown", "Log.entryAdded":
			mu.Lock()
			text := string(message.Params)
			if len(text) > 600 {
				text = text[:600]
			}
			lines = append(lines, message.Method+" "+text)
			mu.Unlock()
		}
	})
	for _, method := range []string{"Runtime.enable", "Log.enable"} {
		if err := b.call(session, method, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// scienceNetLog is what the finished net log says reached the network:
// names looked up on the wire, TCP connections attempted, UDP datagrams
// sent (each as host or address).
type scienceNetLog struct {
	DNS, TCP, UDP []string
}

func readNetLog(path string) (scienceNetLog, error) {
	var result scienceNetLog
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	var log struct {
		Constants struct {
			EventTypes map[string]int `json:"logEventTypes"`
		} `json:"constants"`
		Events []struct {
			Type   int             `json:"type"`
			Params json.RawMessage `json:"params"`
		} `json:"events"`
	}
	if err = json.Unmarshal(data, &log); err != nil {
		// A log cut short (browser killed) lacks the closing brackets.
		trimmed := strings.TrimRight(strings.TrimSpace(string(data)), ",")
		if err = json.Unmarshal([]byte(trimmed+"]}"), &log); err != nil {
			return result, errors.New("net log unreadable")
		}
	}
	names := map[int]string{}
	for name, id := range log.Constants.EventTypes {
		names[id] = name
	}
	for _, event := range log.Events {
		var params map[string]any
		_ = json.Unmarshal(event.Params, &params)
		switch names[event.Type] {
		case "DNS_TRANSACTION":
			if params["hostname"] != nil {
				result.DNS = append(result.DNS, strings.TrimSuffix(fmt.Sprint(params["hostname"]), "."))
			}
		case "TCP_CONNECT_ATTEMPT":
			if params["address"] != nil {
				result.TCP = append(result.TCP, fmt.Sprint(params["address"]))
			}
		case "UDP_BYTES_SENT":
			if params["address"] != nil {
				result.UDP = append(result.UDP, fmt.Sprint(params["address"]))
			}
		}
	}
	return result, nil
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for !done() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
