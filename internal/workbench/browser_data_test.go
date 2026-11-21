package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestBrowserDataForwardsOnlyWhatTheGuestNeeds(t *testing.T) {
	s, calls := browserDataServer(t, func(path string, body map[string]any) (int, string) {
		if body["mode"] == "clear" {
			return 200, `{"job":{"id":"0b7c4c5e-9d55-4c4b-9f43-6a8f1e0b2c11","state":"running"}}`
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
	got := calls()
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
	settings := s.loadBrowserDataSettings()
	if settings.Defaults.Range != "day" || strings.Join(settings.Defaults.Types, ",") != "history,site" {
		t.Fatalf("defaults not remembered: %+v", settings.Defaults)
	}
	w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"clear","range":"all","types":["site"],"keepEnabled":false,"session":"private"}`, true)
	if w.Code != 200 || calls()[1].Body["keepEnabled"] != false || calls()[1].Body["since"].(float64) != 0 || calls()[1].Body["session"] != "private" {
		t.Fatalf("unticked keep or private session lost: %d %+v", w.Code, calls()[1].Body)
	}
	if s.loadBrowserDataSettings().Defaults.Range != "day" {
		t.Fatal("a private clear must not change the remembered defaults")
	}
	w = request(s, "POST", "/api/browser/data", s.origin, `{"mode":"sites","query":"git","page":2,"limit":5}`, true)
	if w.Code != 200 || calls()[2].Body["sort"] != "size" || calls()[2].Body["query"] != "git" {
		t.Fatalf("sites: %+v", calls()[2].Body)
	}
	if _, leaked := calls()[2].Body["keep"]; leaked {
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
	if len(s.loadBrowserDataSettings().Keep) != 0 {
		t.Fatal("keep-remove")
	}
	info, err := os.Stat(s.browserDataSettingsPath())
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("settings file must be private: %v %v", info, err)
	}
	if err := os.WriteFile(s.browserDataSettingsPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.loadBrowserDataSettings(); got.Auto.Enabled || got.Defaults.Range != "day" {
		t.Fatalf("a damaged file must read as the defaults: %+v", got)
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
	settings := s.loadBrowserDataSettings()
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
