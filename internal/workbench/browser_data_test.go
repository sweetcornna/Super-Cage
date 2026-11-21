package workbench

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

type browserDataGuestCall struct {
	Path string
	Body map[string]any
}

// A Linux guest controller that records what reached it and answers with
// the given function.
func browserDataServer(t *testing.T, answer func(path string, body map[string]any) (int, string)) (*Server, func() []browserDataGuestCall) {
	t.Helper()
	s := testServer(t)
	s.ready = true
	s.credentials.Token = "guest-token"
	var mu sync.Mutex
	var calls []browserDataGuestCall
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer guest-token" {
			t.Error("guest authentication missing")
		}
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		mu.Lock()
		calls = append(calls, browserDataGuestCall{r.URL.Path, body})
		mu.Unlock()
		status, text := answer(r.URL.Path, body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(text)), Header: make(http.Header)}, nil
	}))
	return s, func() []browserDataGuestCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]browserDataGuestCall(nil), calls...)
	}
}

func TestBrowserDataRouteBoundary(t *testing.T) {
	s, calls := browserDataServer(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true}` })
	body := `{"mode":"summary","range":"day"}`
	if w := request(s, "POST", "/api/browser/data", s.origin, body, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if w := request(s, "POST", "/api/browser/data", "https://attacker.example", body, true); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", w.Code)
	}
	for _, input := range []string{
		`{"mode":"drop-profile"}`,
		`{"mode":"summary","session":"login"}`,
		`{"mode":"summary","range":"decade"}`,
		`{"mode":"clear","types":[]}`,
		`{"mode":"clear","types":["history","passwords"]}`,
		`{"mode":"clear-sites","sites":["exa mple.com"]}`,
		`{"mode":"clear-sites","sites":[]}`,
		`{"mode":"sites","sort":"constructor"}`,
		`{"mode":"site","site":"../etc"}`,
		`{"mode":"cookie","site":"example.com","key":"short","action":"value"}`,
		`{"mode":"cookie","site":"example.com","key":"` + strings.Repeat("a", 64) + `","action":"export"}`,
		`{"mode":"history","limit":101}`,
		`{"mode":"history-delete","urls":["javascript:alert(1)"]}`,
		`{"mode":"history-delete","start":5,"end":5}`,
		`{"mode":"job","id":"not-a-job"}`,
		`{"mode":"sites","query":"` + strings.Repeat("q", 201) + `"}`,
	} {
		if w := request(s, "POST", "/api/browser/data", s.origin, input, true); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid input accepted: %s: %d", input, w.Code)
		}
	}
	if len(calls()) != 0 {
		t.Fatal("refused requests reached the guest")
	}
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != http.StatusConflict {
		t.Fatalf("not ready: %d", w.Code)
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.setJobForTest(Job{ID: "job", Kind: "stop", Running: true})
	if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != http.StatusConflict {
		t.Fatalf("while a job runs: %d", w.Code)
	}
	s.setJobForTest(Job{})
	s.account.lock()
	if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != http.StatusLocked {
		t.Fatalf("locked account: %d", w.Code)
	}
	if w := request(s, "GET", "/api/browser/data-settings", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked settings: %d", w.Code)
	}
	if len(calls()) != 0 {
		t.Fatal("unready, busy or locked requests reached the guest")
	}
}

// The calls of one mode, in order (a clear is followed by its job polls).
func browserDataCallsOf(calls []browserDataGuestCall, mode string) []browserDataGuestCall {
	var found []browserDataGuestCall
	for _, call := range calls {
		if call.Body["mode"] == mode {
			found = append(found, call)
		}
	}
	return found
}

func TestBrowserDataForwardsOnlyWhatTheGuestNeeds(t *testing.T) {
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		switch body["mode"] {
		case "clear":
			return 200, `{"job":{"id":"0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11","state":"running"}}`
		case "job":
			return 200, `{"job":{"id":"0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11","state":"done"}}`
		}
		return 200, `{"ok":true}`
	})
	for _, action := range []string{`{"action":"keep-add","domain":"https://www.GitHub.com/login"}`, `{"action":"keep-add","domain":"*.example.org:8443"}`} {
		if w := request(s, "POST", "/api/browser/data-settings", s.origin, action, true); w.Code != 200 {
			t.Fatalf("keep-add: %d %s", w.Code, w.Body.String())
		}
	}
	before := time.Now().Add(-24 * time.Hour).UnixMilli()
	w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"day","types":["site","history"],"keep":["attacker.example"],"since":1,"remember":true}`, true)
	if w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	awaitJobIdle(t, &s.jobs)
	got := browserDataCallsOf(calls(), "clear")
	if len(got) != 1 || got[0].Path != "/data" {
		t.Fatalf("guest calls: %+v", got)
	}
	body := got[0].Body
	keep, _ := json.Marshal(body["keep"])
	if string(keep) != `["github.com","example.org"]` {
		t.Fatalf("kept sites must come from the host settings: %s", keep)
	}
	if since := int64(body["since"].(float64)); since < before || since > time.Now().UnixMilli() {
		t.Fatalf("range not converted: %d", since)
	}
	if body["keepEnabled"] != true {
		t.Fatalf("keepEnabled default: %v", body["keepEnabled"])
	}
	settings := mustBrowserDataSettings(t, s)
	if settings.Defaults.Range != "day" || strings.Join(settings.Defaults.Types, ",") != "history,site" {
		t.Fatalf("defaults not remembered: %+v", settings.Defaults)
	}
	w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"all","types":["site"],"protect":"none","session":"private"}`, true)
	awaitJobIdle(t, &s.jobs)
	if second := browserDataCallsOf(calls(), "clear")[1].Body; w.Code != 200 || second["keepEnabled"] != false || second["since"].(float64) != 0 || second["session"] != "private" {
		t.Fatalf("unticked keep or private session lost: %d %+v", w.Code, second)
	}
	if mustBrowserDataSettings(t, s).Defaults.Range != "day" {
		t.Fatal("a private clear must not change the remembered defaults")
	}
	w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"sites","query":"git","page":2,"limit":5}`, true)
	sites := browserDataCallsOf(calls(), "sites")[0].Body
	if w.Code != 200 || sites["sort"] != "size" || sites["query"] != "git" {
		t.Fatalf("sites: %+v", sites)
	}
	if _, leaked := sites["keep"]; leaked {
		t.Fatal("only a clear carries kept sites")
	}
}

func TestBrowserDataCapabilities(t *testing.T) {
	s, _ := browserDataServer(t, func(string, map[string]any) (int, string) {
		return 400, `{"error":"Unknown browser action"}`
	})
	w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"capabilities"}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reason":"update"`) || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("older guest controller: %d %s", w.Code, w.Body.String())
	}
	if w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"summary"}`, true); w.Code != http.StatusConflict {
		t.Fatalf("older guest controller, other modes: %d", w.Code)
	}
	s, _ = browserDataServer(t, func(string, map[string]any) (int, string) {
		return 400, `{"error":"Cookie 已变化，请刷新列表"}`
	})
	if w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"site","site":"example.com"}`, true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Cookie 已变化") {
		t.Fatalf("guest sentence: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowserDataSettingsValidateAndWorkOffline(t *testing.T) {
	s := testServer(t)
	for _, bad := range []string{
		`{"action":"keep-add","domain":"not a domain"}`,
		`{"action":"keep-add","domain":"com"}`,
		`{"action":"defaults","range":"year","types":["site"]}`,
		`{"action":"auto","enabled":true,"trigger":"hourly","days":7,"types":["site"]}`,
		`{"action":"auto","enabled":true,"trigger":"days","days":3,"types":["site"]}`,
		`{"action":"auto","enabled":true,"trigger":"stop","days":7,"types":[]}`,
		`{"action":"wipe"}`,
	} {
		if w := request(s, "POST", "/api/browser/data-settings", s.origin, bad, true); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d", bad, w.Code)
		}
	}
	if w := request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"auto","enabled":true,"trigger":"days","days":30,"types":["cache","history"]}`, true); w.Code != 200 {
		t.Fatalf("auto: %d %s", w.Code, w.Body.String())
	}
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"keep-add","domain":"localhost"}`, true)
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"keep-add","domain":"LOCALHOST"}`, true)
	w := request(s, "GET", "/api/browser/data-settings", "", "", true)
	var settings browserDataSettings
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil {
		t.Fatalf("settings while stopped: %d", w.Code)
	}
	if !settings.Auto.Enabled || settings.Auto.Trigger != "days" || settings.Auto.Days != 30 || strings.Join(settings.Auto.Types, ",") != "history,cache" || len(settings.Keep) != 1 {
		t.Fatalf("settings: %+v", settings)
	}
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"keep-remove","domain":"localhost"}`, true)
	if len(mustBrowserDataSettings(t, s).Keep) != 0 {
		t.Fatal("keep-remove")
	}
	info, err := os.Stat(s.browserDataSettingsPath())
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("settings file must be private: %v %v", info, err)
	}
	if err := os.WriteFile(s.browserDataSettingsPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Review 3, finding 4: only a missing file is the defaults.
	if _, err := s.loadBrowserDataSettings(); !errors.Is(err, errBrowserDataSettingsUnreadable) {
		t.Fatalf("a damaged file must be reported, not read as the defaults: %v", err)
	}
}

func TestBrowserDataAutoCleanRunsOnlyWhenDue(t *testing.T) {
	jobID := "0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11"
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		if body["mode"] == "clear" {
			return 200, `{"job":{"id":"` + jobID + `","state":"running"}}`
		}
		return 200, `{"job":{"id":"` + jobID + `","state":"done","result":{"freedBytes":4096}}}`
	})
	s.browserDataAutoClean("stop")
	if len(calls()) != 0 {
		t.Fatal("automatic cleaning is off by default")
	}
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"keep-add","domain":"github.com"}`, true)
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"auto","enabled":true,"trigger":"stop","days":7,"types":["site","cache"]}`, true)
	s.browserDataAutoClean("start")
	if len(calls()) != 0 {
		t.Fatal("a stop trigger does not run at start")
	}
	s.browserDataAutoClean("stop")
	got := calls()
	if len(got) != 2 || got[0].Body["mode"] != "clear" || got[0].Body["since"].(float64) != 0 || got[1].Body["mode"] != "job" {
		t.Fatalf("stop clean: %+v", got)
	}
	if keep, _ := json.Marshal(got[0].Body["keep"]); string(keep) != `["github.com"]` || got[0].Body["keepEnabled"] != true {
		t.Fatalf("automatic cleaning must honour kept sites: %s", keep)
	}
	settings := mustBrowserDataSettings(t, s)
	if settings.Auto.Result != "done" || settings.Auto.Last.IsZero() {
		t.Fatalf("result not recorded: %+v", settings.Auto)
	}
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"auto","enabled":true,"trigger":"days","days":1,"types":["history"]}`, true)
	s.browserDataAutoClean("start")
	if len(calls()) != 2 {
		t.Fatal("every day: not due again within the day")
	}
	_ = s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
		settings.Auto.Last = time.Now().Add(-25 * time.Hour)
		return nil
	})
	s.browserDataAutoClean("start")
	if len(calls()) != 4 {
		t.Fatalf("every day: due after a day, at start: %d calls", len(calls()))
	}
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	_ = s.updateBrowserDataSettings(func(settings *browserDataSettings) error {
		settings.Auto.Last = time.Time{}
		return nil
	})
	s.browserDataAutoClean("stop")
	if len(calls()) != 4 {
		t.Fatal("no guest call while the environment is not ready")
	}
}

func TestBrowserResetRefusesWhileSigningIn(t *testing.T) {
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		if path == "/tabs" {
			return 200, `{"tabs":[{"id":"a","login":false},{"id":"b","login":true}]}`
		}
		return 200, `{"ok":true}`
	})
	w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"reset"}`, true)
	if w.Code != 200 {
		t.Fatalf("reset submission: %d %s", w.Code, w.Body.String())
	}
	job := awaitJobIdle(t, &s.jobs)
	if job.Kind != "browser-reset" || !strings.Contains(job.Error, "账号授权") {
		t.Fatalf("reset with a sign-in page: %+v", job)
	}
	for _, call := range calls() {
		if call.Path == "/data" {
			t.Fatal("reset reached the guest while a sign-in page was open")
		}
	}
}

func TestBrowserKeptDomain(t *testing.T) {
	for input, want := range map[string]string{
		"github.com": "github.com", "https://www.github.com/x?y": "github.com", "*.Example.org": "example.org",
		"example.org:8443": "example.org", ".bbc.co.uk": "bbc.co.uk", "localhost": "localhost", "127.0.0.1": "127.0.0.1",
	} {
		if got, err := browserKeptDomain(input); err != nil || got != want {
			t.Errorf("%q: %q %v", input, got, err)
		}
	}
	for _, input := range []string{"", "com", "a b.com", "http://", strings.Repeat("a", 254) + ".com"} {
		if _, err := browserKeptDomain(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
}

// The bodies browser-data.jsx sends, as it sends them, through the real
// routes: what reaches the guest and what the UI reads back.
func TestBrowserDataAcceptsTheUIRequests(t *testing.T) {
	const jobID = "0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11"
	key := strings.Repeat("ab", 32)
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		switch body["mode"] {
		case "capabilities":
			return 200, `{"available":true,"version":1,"ranges":true,"history":true,"incognito":true,"reset":true}`
		case "clear", "clear-sites":
			return 200, `{"job":{"id":"` + jobID + `","state":"running","step":0,"steps":4,"elapsed":0}}`
		case "job":
			return 200, `{"job":{"id":"` + jobID + `","state":"done","step":4,"steps":4,"elapsed":180,"result":{"cleared":["history","site"],"freedBytes":2048,"reloaded":1,"kept":["https://github.com"]}}}`
		}
		return 200, `{"ok":true}`
	})
	post := func(path, body string) map[string]any {
		t.Helper()
		w := request(s, "POST", path, s.origin, body, true)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", path, body, w.Code, w.Body.String())
		}
		var reply map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatalf("%s %s: %v", path, body, err)
		}
		return reply
	}
	if reply := post("/api/browser/data", `{"mode":"capabilities"}`); reply["available"] != true {
		t.Fatalf("capabilities: %v", reply)
	}
	for _, body := range []string{
		`{"mode":"summary","session":"normal","range":"all"}`,
		`{"mode":"summary","session":"private","range":"all"}`,
		`{"mode":"sites","session":"normal","query":"","sort":"size","page":0}`,
		`{"mode":"sites","session":"private","query":"gi","sort":"recent","page":2}`,
		`{"mode":"site","session":"normal","site":"github.com","page":0,"query":"sess"}`,
		`{"mode":"cookie","session":"normal","site":"github.com","key":"` + key + `","action":"value"}`,
		`{"mode":"cookie","session":"private","site":"github.com","key":"` + key + `","action":"delete"}`,
		`{"mode":"history","query":"","limit":100}`,
		`{"mode":"history","query":"docs","before":1763700000000,"limit":100}`,
		`{"mode":"history-delete","urls":["https://github.com/login"]}`,
		`{"mode":"history-delete","host":"github.com"}`,
		`{"mode":"history-delete","start":1763683200000,"end":1763769600000}`,
	} {
		post("/api/browser/data", body)
	}
	// The dialog: a week, keep off, remembered; then a preset that is not.
	if reply := post("/api/browser/data", `{"mode":"clear","session":"normal","range":"week","types":["history","site","cache","downloads"],"protect":"none","remember":true}`); reply["job"].(map[string]any)["id"] != jobID {
		t.Fatalf("clear reply: %v", reply)
	}
	awaitJobIdle(t, &s.jobs)
	for _, body := range []string{
		`{"mode":"clear","session":"normal","range":"hour","types":["history"],"remember":false}`,
		`{"mode":"clear","session":"private","range":"all","types":["site","cache"],"remember":false}`,
		`{"mode":"clear-sites","session":"normal","sites":["github.com","127.0.0.1"]}`,
	} {
		post("/api/browser/data", body)
		awaitJobIdle(t, &s.jobs)
	}
	job := post("/api/browser/data", `{"mode":"job","id":"`+jobID+`"}`)["job"].(map[string]any)
	result := job["result"].(map[string]any)
	if job["state"] != "done" || job["steps"] != float64(4) || result["freedBytes"] != float64(2048) || len(result["kept"].([]any)) != 1 {
		t.Fatalf("job reply: %v", job)
	}
	got := calls()
	byMode := map[string][]map[string]any{}
	for _, call := range got {
		byMode[call.Body["mode"].(string)] = append(byMode[call.Body["mode"].(string)], call.Body)
	}
	if body := byMode["site"][0]; body["query"] != "sess" || body["session"] != "normal" {
		t.Fatalf("site body: %v", body)
	}
	if body := byMode["sites"][1]; body["session"] != "private" || body["sort"] != "recent" || body["page"] != float64(2) {
		t.Fatalf("sites body: %v", body)
	}
	if body := byMode["history"][1]; body["before"] != float64(1763700000000) || body["limit"] != float64(100) {
		t.Fatalf("history body: %v", body)
	}
	clears := byMode["clear"]
	if len(clears) != 3 || clears[0]["keepEnabled"] != false || clears[1]["keepEnabled"] != true || clears[2]["session"] != "private" || clears[2]["since"] != float64(0) {
		t.Fatalf("clear bodies: %v", clears)
	}
	if since := int64(clears[0]["since"].(float64)); since > time.Now().Add(-7*24*time.Hour+time.Minute).UnixMilli() {
		t.Fatalf("week not converted: %d", since)
	}
	settings := mustBrowserDataSettings(t, s)
	if settings.Defaults.Range != "week" || len(settings.Defaults.Types) != 4 {
		t.Fatalf("remembered defaults (the preset and the private clear must not change them): %+v", settings.Defaults)
	}

	// Settings: what the privacy section sends.
	get := request(s, "GET", "/api/browser/data-settings", "", "", true)
	if get.Code != 200 {
		t.Fatalf("settings: %d", get.Code)
	}
	for _, body := range []string{
		`{"action":"keep-add","domain":"https://www.kept.localhost:5238/login"}`,
		`{"action":"keep-add","domain":"github.com"}`,
		`{"action":"keep-remove","domain":"github.com"}`,
		`{"action":"auto","enabled":true,"trigger":"days","days":7,"types":["history","site","cache"]}`,
		`{"action":"auto","enabled":false,"trigger":"stop","days":7,"types":["history"]}`,
	} {
		post("/api/browser/data-settings", body)
	}
	settings = mustBrowserDataSettings(t, s)
	if len(settings.Keep) != 1 || settings.Keep[0].Domain != "kept.localhost" || settings.Auto.Enabled || settings.Auto.Trigger != "stop" || len(settings.Auto.Types) != 1 {
		t.Fatalf("settings after the privacy section: %+v", settings)
	}
	// The settings reply carries what the UI reads back.
	var reply map[string]any
	_ = json.Unmarshal(request(s, "GET", "/api/browser/data-settings", "", "", true).Body.Bytes(), &reply)
	for _, field := range []string{"defaults", "keep", "auto"} {
		if reply[field] == nil {
			t.Fatalf("settings reply lacks %s: %v", field, reply)
		}
	}
	auto := reply["auto"].(map[string]any)
	// last and result are omitted until a first run; the UI reads them as optional.
	for _, field := range []string{"enabled", "trigger", "days", "types"} {
		if _, ok := auto[field]; !ok {
			t.Fatalf("auto lacks %s: %v", field, auto)
		}
	}
}

// Through the agent transport the browser-data routes are open, except a
// reset and a clear that ignores the kept list: those need the person.
func TestAgentBodyRulesKeepResetAndUnprotectedClearsForThePerson(t *testing.T) {
	refusal := func(body string) *AgentError {
		if refused := agentRouteRefusal("POST", "/api/browser/data"); refused != nil {
			return refused
		}
		return agentBodyRefusal(agentBodyRulesFor("POST", "/api/browser/data"), []byte(body))
	}
	for _, body := range []string{`{"mode":"reset"}`, `{"Mode":"reset"}`, `{"mode":"clear","types":["site"],"protect":"none"}`, `{"mode":"clear","PROTECT":"none"}`} {
		if refused := refusal(body); refused == nil || refused.Code != "native_transport_required" || refused.NextAction != "open_desktop" {
			t.Errorf("%s reached the handler: %+v", body, refused)
		}
	}
	for _, body := range []string{`{"mode":"capabilities"}`, `{"mode":"clear","types":["site","cache"],"range":"day"}`, `{"mode":"clear","types":["history"],"protect":"kept"}`, `{"mode":"clear-sites","sites":["example.com"]}`} {
		if refused := refusal(body); refused != nil {
			t.Errorf("%s refused: %+v", body, refused)
		}
	}
	if refused := agentRouteRefusal("POST", "/api/browser/data-settings"); refused != nil {
		t.Errorf("data-settings refused: %+v", refused)
	}
}

// Review 3, findings 3 and 4: a settings file that cannot be read is left as
// it is and reported; nothing that depends on the kept list runs meanwhile.
func TestBrowserDataSettingsUnreadableAreKeptAndReported(t *testing.T) {
	const jobID = "0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11"
	valid := `{"defaults":{"range":"day","types":["history"]},"keep":[{"domain":"github.com","added":"2026-11-01T00:00:00Z"}],"auto":{"enabled":true,"trigger":"stop","days":7,"types":["site"]}}`
	cases := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"unreadable", valid, 0},
		{"oversized", valid + strings.Repeat(" ", 300<<10), 0o600},
		{"wrong type", `{"keep":"github.com"}`, 0o600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.mode == 0 && os.Geteuid() == 0 {
				t.Skip("root reads any file")
			}
			s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
				if body["mode"] == "clear" {
					return 200, `{"job":{"id":"` + jobID + `","state":"running"}}`
				}
				return 200, `{"job":{"id":"` + jobID + `","state":"done"}}`
			})
			path := s.browserDataSettingsPath()
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(path, 0o600)
			if w := request(s, "GET", "/api/browser/data-settings", "", "", true); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "设置暂时读不出来") {
				t.Fatalf("GET: %d %s", w.Code, w.Body.String())
			}
			if w := request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"keep-add","domain":"example.org"}`, true); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("a write over an unreadable file: %d %s", w.Code, w.Body.String())
			}
			for _, body := range []string{`{"mode":"clear","range":"all","types":["site"]}`, `{"mode":"clear","range":"all","types":["history","site"],"remember":true}`} {
				if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "设置暂时读不出来") {
					t.Fatalf("a clear that needs the kept list: %d %s", w.Code, w.Body.String())
				}
			}
			s.browserDataAutoClean("stop")
			if len(calls()) != 0 {
				t.Fatalf("clears that need the kept list reached the guest: %+v", calls())
			}
			if w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"all","types":["history","cache"],"remember":true}`, true); w.Code != 200 {
				t.Fatalf("history and cache clears do not need the kept list: %d %s", w.Code, w.Body.String())
			}
			awaitJobIdle(t, &s.jobs)
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != c.content {
				t.Fatalf("the unreadable file was changed: %v", err)
			}
		})
	}
	s := testServer(t)
	if w := request(s, "GET", "/api/browser/data-settings", "", "", true); w.Code != 200 {
		t.Fatalf("a missing file reads as the defaults: %d", w.Code)
	}
}

// Review 3, findings 3 and 8: automatic cleaning stores only values the host
// made, and cleaning at stop leaves the pages alone and says what it does.
func TestBrowserDataAutoCleanStoresHostMadeValues(t *testing.T) {
	const jobID = "0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11"
	var server *Server
	var message string
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		if body["mode"] == "clear" {
			message = server.jobs.snapshot().Message
			return 200, `{"job":{"id":"` + jobID + `","state":"running"}}`
		}
		return 200, `{"job":{"id":"` + jobID + `","state":"` + strings.Repeat("<", 45<<10) + `","result":{"freedBytes":1e30}}}`
	})
	server = s
	request(s, "POST", "/api/browser/data-settings", s.origin, `{"action":"auto","enabled":true,"trigger":"stop","days":7,"types":["site","cache"]}`, true)
	if _, err := s.submitJob("stop", func() error { s.browserDataAutoClean("stop"); return nil }); err != nil {
		t.Fatal(err)
	}
	awaitJobIdle(t, &s.jobs)
	got := calls()
	if len(got) == 0 || got[0].Body["reload"] != false {
		t.Fatalf("cleaning at stop must not reload pages: %+v", got)
	}
	if !strings.Contains(message, "自动清理") {
		t.Fatalf("no progress message during cleaning at stop: %q", message)
	}
	info, err := os.Stat(s.browserDataSettingsPath())
	if err != nil || info.Size() > 4096 {
		t.Fatalf("settings file grew with guest text: %v %v", info.Size(), err)
	}
	settings, err := s.loadBrowserDataSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Auto.Result != "failed" || settings.Auto.Attempted.IsZero() || !settings.Auto.Last.IsZero() || settings.Auto.Freed != 0 {
		t.Fatalf("stored result: %+v", settings.Auto)
	}
	reply := request(s, "GET", "/api/browser/data-settings", "", "", true).Body.String()
	if !strings.Contains(reply, `"attempted"`) || strings.Contains(reply, "<") || strings.Contains(reply, "\u003c") {
		t.Fatalf("settings reply: %.200s", reply)
	}
}

// Review 3, findings 2 and 11: hosts Chromium cannot take as an origin and
// public suffixes are refused; IP addresses and IPv6 sites work.
func TestBrowserKeptDomainRefusesNumericHostsAndPublicSuffixes(t *testing.T) {
	for _, input := range []string{"192.168.1.10", "10.0.0.1", "127.0.0.1", "bbc.co.uk", "someone.github.io", "example.com.cn"} {
		if _, err := browserKeptDomain(input); err != nil {
			t.Errorf("%q refused: %v", input, err)
		}
	}
	for _, input := range []string{"1.2.3", "foo.123", "1.2.3.4.5", "999.1.1.1"} {
		if _, err := browserKeptDomain(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
	for _, input := range []string{"co.uk", "github.io", "com.cn", "vercel.app", "*.co.jp"} {
		if _, err := browserKeptDomain(input); err == nil || !strings.Contains(err.Error(), "公共后缀") {
			t.Errorf("%q: %v", input, err)
		}
	}
	s, calls := browserDataServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"job":{"id":"0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11","state":"done"}}`
	})
	for _, body := range []string{`{"mode":"clear-sites","sites":["1.2.3"]}`, `{"mode":"clear-sites","sites":["foo.123"]}`, `{"mode":"site","site":"foo.123"}`} {
		if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if len(calls()) != 0 {
		t.Fatal("refused sites reached the guest")
	}
	for _, body := range []string{`{"mode":"site","site":"::1"}`, `{"mode":"clear-sites","sites":["192.168.1.10","::1"]}`} {
		if w := request(s, "POST", "/api/browser/data", s.origin, body, true); w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		awaitJobIdle(t, &s.jobs)
	}
}

// Review 3, finding 9: a clear is a host job. A second window joins it,
// every window can follow it while it runs, reset waits, and a stop that
// comes in the middle waits for the clear instead of stopping Chromium.
func TestBrowserClearIsAHostJob(t *testing.T) {
	const jobID = "0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11"
	release := make(chan struct{})
	var clearDone time.Time
	var doneMu sync.Mutex
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		switch body["mode"] {
		case "clear":
			return 200, `{"job":{"id":"` + jobID + `","state":"running","step":1,"steps":4}}`
		case "job":
			select {
			case <-release:
				doneMu.Lock()
				if clearDone.IsZero() {
					clearDone = time.Now()
				}
				doneMu.Unlock()
				return 200, `{"job":{"id":"` + jobID + `","state":"done","step":4,"steps":4,"result":{"cleared":["site"],"freedBytes":10}}}`
			default:
				return 200, `{"job":{"id":"` + jobID + `","state":"running","step":2,"steps":4}}`
			}
		}
		return 200, `{"ok":true}`
	})
	first := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"all","types":["site"]}`, true)
	if first.Code != 200 || !strings.Contains(first.Body.String(), jobID) {
		t.Fatalf("clear: %d %s", first.Code, first.Body.String())
	}
	if job := s.jobs.snapshot(); !job.Running || job.Kind != "browser-clear" {
		t.Fatalf("a clear must be a host job: %+v", job)
	}
	second := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"hour","types":["history"]}`, true)
	if second.Code != 200 || !strings.Contains(second.Body.String(), jobID) {
		t.Fatalf("a second window joins the running clear: %d %s", second.Code, second.Body.String())
	}
	poll := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"job","id":"`+jobID+`"}`, true)
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"running"`) {
		t.Fatalf("the job is followed while the host is busy with it: %d %s", poll.Code, poll.Body.String())
	}
	if w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"reset"}`, true); w.Code != http.StatusConflict {
		t.Fatalf("reset during a clear: %d", w.Code)
	}
	for _, kind := range []string{"safety", "network-recovery"} {
		if _, err := s.submitJob(kind, func() error { return nil }); !errors.Is(err, errJobBusy) {
			t.Fatalf("%s started during a clear: %v", kind, err)
		}
	}
	stopRan := make(chan time.Time, 1)
	if _, err := s.submitJob("stop", func() error { stopRan <- time.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case <-stopRan:
		t.Fatal("a stop ran in the middle of a clear")
	default:
	}
	close(release)
	ran := <-stopRan
	doneMu.Lock()
	finished := clearDone
	doneMu.Unlock()
	if finished.IsZero() || ran.Before(finished) {
		t.Fatalf("the stop did not wait for the clear: clear %v stop %v", finished, ran)
	}
	awaitJobIdle(t, &s.jobs)
	poll = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"job","id":"`+jobID+`"}`, true)
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"done"`) {
		t.Fatalf("the outcome after the job: %d %s", poll.Code, poll.Body.String())
	}
	clears := 0
	for _, call := range calls() {
		if call.Body["mode"] == "clear" {
			clears++
		}
	}
	if clears != 1 {
		t.Fatalf("two windows, one clear: %d", clears)
	}
}

// Review 3, findings 9 and 10: when the guest stops answering in the middle
// of a clear, the job and every window following it get the host's own
// account instead of a guest error, and a network lease that runs out while
// the clear waits is said in the job's message.
func TestBrowserClearReportsWhatTheHostSaw(t *testing.T) {
	const jobID = "6f1d2a7e-3c4b-4e8a-9b0c-5d6e7f8a9b0c"
	var mu sync.Mutex
	var server *Server
	var messages []string
	polls := 0
	s, _ := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		switch body["mode"] {
		case "clear":
			return 200, `{"job":{"id":"` + jobID + `","state":"running","step":1,"steps":4}}`
		case "job":
			mu.Lock()
			defer mu.Unlock()
			polls++
			messages = append(messages, server.jobs.snapshot().Message)
			if polls <= 4 {
				return 200, `{"job":{"id":"` + jobID + `","state":"running","step":2,"steps":4}}`
			}
			return 502, `{"error":"down"}`
		}
		return 200, `{"ok":true}`
	})
	server = s
	s.mu.Lock()
	s.safety.Gate = &cage.GateStatus{Mode: cage.GateOpen, Until: time.Now().Add(300 * time.Millisecond)}
	s.mu.Unlock()
	if w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"all","types":["history"]}`, true); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	job := awaitJobIdle(t, &s.jobs)
	if !strings.Contains(job.Error, "清除结果未知") {
		t.Fatalf("the job must end with the host's account: %+v", job)
	}
	poll := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"job","id":"`+jobID+`"}`, true)
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"failed"`) || !strings.Contains(poll.Body.String(), "清除结果未知") {
		t.Fatalf("a window following the clear: %d %s", poll.Code, poll.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(messages, func(message string) bool { return strings.Contains(message, "联网授权已到期") }) {
		t.Fatalf("the lapsed lease was not said: %q", messages)
	}
}

// Review 3, findings 5 and 10: after a reset the host follows the new
// controller as after a crash (ready toggles, a hidden picture is minimised
// again), reports the outcome the controller wrote, and gives up truthfully.
func TestBrowserResetReattachesAndReportsTheOutcome(t *testing.T) {
	restartLimit := browserRestartLimit
	defer func() { browserRestartLimit = restartLimit }()
	for _, c := range []struct {
		name, status, wantError string
		comesBack               bool
	}{
		{"done", "done", "", true},
		{"failed", "failed", "仍在使用原来的资料", true},
		{"never back", "", "用时过长", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			browserRestartLimit = 2 * time.Second
			var mu sync.Mutex
			var resetID, canceled string
			var healthCalls int
			var readyDuring []bool
			var visibility []bool
			var server *Server
			s, _ := browserDataServer(t, func(path string, body map[string]any) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case path == "/tabs":
					return 200, `{"tabs":[]}`
				case path == "/visibility":
					visibility = append(visibility, body["visible"] == true)
					return 200, `{"ok":true}`
				case path == "/health":
					healthCalls++
					server.mu.Lock()
					readyDuring = append(readyDuring, server.ready)
					server.mu.Unlock()
					if resetID == "" || !c.comesBack || healthCalls < 3 {
						return 502, `{"error":"down"}`
					}
					return 200, `{"ok":true}`
				case body["mode"] == "reset":
					resetID, _ = body["id"].(string)
					return 200, `{"ok":true,"restarting":true}`
				case body["mode"] == "reset-status":
					if body["id"] != resetID {
						return 200, `{"state":"unknown"}`
					}
					return 200, `{"state":"` + c.status + `"}`
				case body["mode"] == "reset-cancel":
					canceled, _ = body["id"].(string)
					return 200, `{"ok":true}`
				}
				return 200, `{"ok":true}`
			})
			server = s
			// The picture is hidden: the host told the old controller so.
			if err := s.browserViews.apply(s.changeBrowserVisibility); err != nil {
				t.Fatal(err)
			}
			if w := request(s, "POST", "/api/browser/data", s.origin, `{"mode":"reset"}`, true); w.Code != 200 {
				t.Fatalf("reset: %d %s", w.Code, w.Body.String())
			}
			job := awaitJobIdle(t, &s.jobs)
			time.Sleep(800 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if resetID == "" {
				t.Fatal("the reset carries no id")
			}
			if !slices.Contains(readyDuring, false) {
				t.Fatalf("ready never dropped during the restart: %v", readyDuring)
			}
			if c.wantError == "" {
				if job.Error != "" {
					t.Fatalf("reset: %+v", job)
				}
			} else if !strings.Contains(job.Error, c.wantError) {
				t.Fatalf("reset error: %+v", job)
			}
			s.mu.Lock()
			ready := s.ready
			s.mu.Unlock()
			if c.comesBack {
				if !ready {
					t.Fatal("not ready after the controller came back")
				}
				if len(visibility) < 2 || visibility[len(visibility)-1] {
					t.Fatalf("the hidden picture was not minimised again: %v", visibility)
				}
			} else if canceled != resetID {
				t.Fatalf("a reset the job gave up on must be cancelled: %q %q", canceled, resetID)
			}
		})
	}
}

func TestBrowserResetOnAnOldGuestSaysSoInChinese(t *testing.T) {
	s, _ := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		if path == "/tabs" {
			return 200, `{"tabs":[]}`
		}
		return 400, `{"error":"Unknown browser action"}`
	})
	request(s, "POST", "/api/browser/data", s.origin, `{"mode":"reset"}`, true)
	job := awaitJobIdle(t, &s.jobs)
	if !strings.Contains(job.Error, "更新环境组件") || strings.Contains(job.Error, "unsupported") {
		t.Fatalf("old guest: %q", job.Error)
	}
}

func mustBrowserDataSettings(t *testing.T, s *Server) browserDataSettings {
	t.Helper()
	settings, err := s.loadBrowserDataSettings()
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
