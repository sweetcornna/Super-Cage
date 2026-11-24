package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOfficialSettingsAddresses(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:13773/":          "http://127.0.0.1:13773/settings",
		"https://t3.example.test":          "https://t3.example.test/settings",
		"https://t3.example.test/base":     "https://t3.example.test/base/settings",
		"https://t3.example.test/b/?x=1#f": "https://t3.example.test/b/settings",
		"javascript:alert(1)":              "",
		"/relative":                        "",
	} {
		if got := t3CodeSettingsURL(base); got != want {
			t.Errorf("t3CodeSettingsURL(%q) = %q, want %q", base, got, want)
		}
	}
	for raw, want := range map[string]string{
		"http://127.0.0.1:3430/":             "http://127.0.0.1:3430/?tab=general&view=settings",
		"http://127.0.0.1:3430/?k=secret":    "http://127.0.0.1:3430/?k=secret&tab=general&view=settings",
		"https://m.example.test/?view=usage": "https://m.example.test/?tab=general&view=settings",
		"file:///etc/passwd":                 "",
	} {
		if got := magpieSettingsURL(raw); got != want {
			t.Errorf("magpieSettingsURL(%q) = %q, want %q", raw, got, want)
		}
	}
	origin := "https://t3.example.test"
	for url, want := range map[string]bool{
		"https://t3.example.test/pair#token=x": false,
		"https://t3.example.test/base/pair":    false,
		"https://t3.example.test/":             true,
		"https://t3.example.test/new":          true,
		"https://other.example.test/":          false,
		"about:blank":                          false,
	} {
		if got := t3CodePaired(url, origin); got != want {
			t.Errorf("t3CodePaired(%q) = %v, want %v", url, got, want)
		}
	}
	if !validOfficialPage("") || !validOfficialPage("settings") || validOfficialPage("../admin") || validOfficialPage("https://evil.test") {
		t.Fatal("validOfficialPage accepted an unknown page")
	}
}

func TestHerdrSettingsKeysSanitized(t *testing.T) {
	keep := (&herdrSettingsKeys{Chunks: []string{"\x02", "s"}, Shortcut: "ctrl+b s"}).sanitized()
	if len(keep.Chunks) != 2 || keep.Chunks[0] != "\x02" || keep.Shortcut != "ctrl+b s" {
		t.Fatalf("default key dropped: %+v", keep)
	}
	for _, chunks := range [][]string{
		{"rm -rf ~\r"},          // longer than any key press
		{"\x02", "s", "x", "y"}, // more presses than any binding
		{"\x02", "\u00e9"},      // not something a key sends
		{"\x02", ""},
	} {
		got := (&herdrSettingsKeys{Onboarding: true, Chunks: chunks, Shortcut: "x\x1by"}).sanitized()
		if got.Chunks != nil || !got.Onboarding || got.Shortcut != "xy" {
			t.Errorf("sanitized(%q) = %+v", chunks, got)
		}
	}
	if (*herdrSettingsKeys)(nil).sanitized() != nil {
		t.Fatal("nil keys became non-nil")
	}
	var status herdrGuestStatus
	if err := json.Unmarshal([]byte(`{"installed":true,"settings_keys":{"onboarding":false,"chunks":["\u0002","s"],"shortcut":"ctrl+b s"}}`), &status); err != nil || status.SettingsKeys == nil || len(status.SettingsKeys.Chunks) != 2 {
		t.Fatalf("guest settings_keys not decoded: %+v %v", status, err)
	}
}

// fakeSettingsBrowser is the Cage browser for the official-settings tests:
// tabs answers from urls, and every new, activate and navigate is recorded.
type fakeSettingsBrowser struct {
	mu        sync.Mutex
	tabs      map[string]string
	pairPolls int // /tabs reads that still show a fresh tab on /pair
	created   []string
	activated []string
	navigated []string
}

func (b *fakeSettingsBrowser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var body struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/tabs":
		type tab struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		var list []tab
		for id, url := range b.tabs {
			if id == "tab-new" && b.pairPolls > 0 {
				b.pairPolls--
				url = strings.TrimSuffix(url, "/") + "/pair#token=one-time"
			}
			list = append(list, tab{ID: id, URL: url})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tabs": list})
	case "/new":
		b.created = append(b.created, body.URL)
		b.tabs["tab-new"] = strings.SplitN(body.URL, "?", 2)[0]
		_, _ = w.Write([]byte(`{"id":"tab-new"}`))
	case "/activate":
		b.activated = append(b.activated, body.ID)
		_, _ = w.Write([]byte(`{"ok":true}`))
	case "/navigate":
		b.navigated = append(b.navigated, body.ID+" "+body.URL)
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.NotFound(w, r)
	}
}

func (b *fakeSettingsBrowser) snapshot() (created, activated, navigated []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.created...), append([]string(nil), b.activated...), append([]string(nil), b.navigated...)
}

func TestT3CodeOpensItsOwnSettings(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if w := request(s, "POST", "/api/t3code/config", s.origin, `{"enabled":true,"mode":"external","external_url":"https://t3.example.test/base","open_mode":"direct"}`, true); w.Code != http.StatusOK {
		t.Fatalf("config: %d %s", w.Code, w.Body.String())
	}
	browser := &fakeSettingsBrowser{tabs: map[string]string{"tab-other": "http://127.0.0.1:19999/"}, pairPolls: 2}
	guest := httptest.NewServer(browser)
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()

	if w := request(s, "POST", "/api/t3code/open", s.origin, `{"page":"../admin"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown page accepted: %d %s", w.Code, w.Body.String())
	}

	// A fresh tab: opened in the workbench even though direct is preferred,
	// left alone while it pairs, then moved to Settings.
	w := request(s, "POST", "/api/t3code/open", s.origin, `{"page":"settings"}`, true)
	var reply map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	created, _, navigated := browser.snapshot()
	if w.Code != http.StatusOK || reply["mode"] != "workbench" || reply["settings_opened"] != true ||
		len(created) != 1 || len(navigated) != 1 || navigated[0] != "tab-new https://t3.example.test/base/settings" {
		t.Fatalf("fresh settings open: %d %s created=%q navigated=%q", w.Code, w.Body.String(), created, navigated)
	}
	if browser.pairPolls != 0 {
		t.Fatalf("navigated before pairing finished (%d /pair reads left)", browser.pairPolls)
	}

	// The open tab is reused: activated and moved, no second pairing.
	w = request(s, "POST", "/api/t3code/open", s.origin, `{"page":"settings"}`, true)
	created, activated, navigated := browser.snapshot()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"settings_opened":true`) || len(created) != 1 ||
		len(activated) != 1 || activated[0] != "tab-new" || len(navigated) != 2 {
		t.Fatalf("reused settings open: %d %s created=%q activated=%q navigated=%q", w.Code, w.Body.String(), created, activated, navigated)
	}

	// A plain open still goes to T3 Code's home and never navigates.
	if w = request(s, "POST", "/api/t3code/open", s.origin, `{"force_mode":"workbench"}`, true); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "settings_opened") {
		t.Fatalf("plain open: %d %s", w.Code, w.Body.String())
	}

	// Pairing that never finishes leaves the tab on T3 Code and says so.
	old := t3CodeSettingsWait
	t3CodeSettingsWait = 600 * time.Millisecond
	defer func() { t3CodeSettingsWait = old }()
	browser.mu.Lock()
	delete(browser.tabs, "tab-new")
	browser.pairPolls = 1 << 20
	browser.mu.Unlock()
	w = request(s, "POST", "/api/t3code/open", s.origin, `{"page":"settings"}`, true)
	_, _, navigated = browser.snapshot()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"settings_opened":false`) || len(navigated) != 2 {
		t.Fatalf("unfinished pairing: %d %s navigated=%q", w.Code, w.Body.String(), navigated)
	}
}

func TestMagpieOpensItsOwnSettings(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if w := request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"https://magpie.example.test","open_mode":"workbench"}`, true); w.Code != http.StatusOK {
		t.Fatalf("config: %d %s", w.Code, w.Body.String())
	}
	browser := &fakeSettingsBrowser{tabs: map[string]string{}}
	guest := httptest.NewServer(browser)
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()

	if w := request(s, "POST", "/api/magpie/open", s.origin, `{"page":"https://evil.test/"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown page accepted: %d %s", w.Code, w.Body.String())
	}
	// A fresh tab opens straight on Settings › General.
	w := request(s, "POST", "/api/magpie/open", s.origin, `{"page":"settings"}`, true)
	created, _, navigated := browser.snapshot()
	if w.Code != http.StatusOK || len(created) != 1 || created[0] != "https://magpie.example.test/?tab=general&view=settings" || len(navigated) != 0 {
		t.Fatalf("fresh settings open: %d %s created=%q navigated=%q", w.Code, w.Body.String(), created, navigated)
	}
	// The open tab is moved there without a new sign-in.
	w = request(s, "POST", "/api/magpie/open", s.origin, `{"page":"settings"}`, true)
	created, activated, navigated := browser.snapshot()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"settings_opened":true`) || len(created) != 1 ||
		len(activated) != 1 || len(navigated) != 1 || !strings.HasSuffix(navigated[0], "?tab=general&view=settings") {
		t.Fatalf("reused settings open: %d %s created=%q activated=%q navigated=%q", w.Code, w.Body.String(), created, activated, navigated)
	}

	// The native window (direct mode) lands on Settings too: its one-time
	// grant redirects to the page asked for.
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ext.Close()
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"`+ext.URL+`","open_mode":"direct"}`, true); w.Code != http.StatusOK {
		t.Fatalf("direct config: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "POST", "/api/magpie/open", s.origin, `{"force_mode":"direct","page":"settings"}`, true)
	var direct struct {
		Mode      string `json:"mode"`
		DirectURL string `json:"direct_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &direct); err != nil || w.Code != http.StatusOK || direct.Mode != "direct" || direct.DirectURL == "" {
		t.Fatalf("direct settings open: %d %s", w.Code, w.Body.String())
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(direct.DirectURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/?tab=general&view=settings" {
		t.Fatalf("direct grant redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
