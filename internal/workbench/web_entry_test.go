package workbench

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/webentry"
)

const (
	entryIPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	entryMac    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"
)

func entryDocuments(t *testing.T) (desktop, phone []byte) {
	t.Helper()
	desktop, err := fs.ReadFile(assets, "web/index.html")
	if err != nil {
		t.Skip("build the UI first (npm --prefix ui run build)")
	}
	phone, err = fs.ReadFile(assets, "web/mobile.html")
	if err != nil {
		t.Skip("build the UI first (npm --prefix ui run build)")
	}
	return desktop, phone
}

func rootRequest(s *Server, agent, cookie, hint string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", s.origin+"/", nil)
	r.Header.Set("User-Agent", agent)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if hint != "" {
		r.Header.Set(webentry.Header, hint)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

// A phone that opens the workbench gets the phone UI at the same URL, without
// downloading the desktop workbench, unless it chose the desktop layout.
func TestRootDocumentFollowsTheDevice(t *testing.T) {
	desktop, phone := entryDocuments(t)
	s := testServer(t)
	for _, test := range []struct {
		name, agent, cookie, hint string
		want                      []byte
	}{
		{"phone", entryIPhone, "", "", phone},
		{"desktop", entryMac, "", "", desktop},
		{"phone chose the desktop", entryIPhone, "cage-layout=desktop", "", desktop},
		{"desktop chose the phone", entryMac, "cage-layout=phone", "", phone},
		{"tablet page guard", entryMac, "cage-layout=phone-auto", "", phone},
		{"gateway hint, no browser headers", "", "", "phone", phone},
		{"gateway hint wins", entryIPhone, "", "desktop", desktop},
		{"invalid hint falls back to the device", entryIPhone, "", "tablet", phone},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := rootRequest(s, test.agent, test.cookie, test.hint)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), test.want) {
				t.Fatalf("status %d, wrong document", w.Code)
			}
			if vary := strings.Join(w.Header().Values("Vary"), ", "); !strings.Contains(vary, "Cookie") || !strings.Contains(vary, "User-Agent") || !strings.Contains(vary, webentry.Header) {
				t.Fatalf("root document must vary on what chose it: %q", vary)
			}
			if w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("root document must be revalidated: %q", w.Header().Get("Cache-Control"))
			}
		})
	}
	// The two documents never share a validator, so a cached one is never reused for the other.
	phoneTag, desktopTag := rootRequest(s, entryIPhone, "", "").Header().Get("ETag"), rootRequest(s, entryMac, "", "").Header().Get("ETag")
	if phoneTag == "" || phoneTag == desktopTag {
		t.Fatalf("ETags %q and %q", phoneTag, desktopTag)
	}
	r := httptest.NewRequest("GET", s.origin+"/", nil)
	r.Header.Set("User-Agent", entryIPhone)
	r.Header.Set("If-None-Match", desktopTag)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), phone) {
		t.Fatal("a phone revalidating the desktop document must get the phone UI")
	}
	// Other paths are untouched: the phone's own entry and the desktop entry by name.
	for path, want := range map[string][]byte{"/mobile.html": phone} {
		r := httptest.NewRequest("GET", s.origin+path, nil)
		r.Header.Set("User-Agent", entryMac)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) || strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "User-Agent") {
			t.Fatalf("%s changed", path)
		}
	}
}

// Servers that cannot answer the phone routes for a browser keep the desktop.
func TestRootDocumentStaysDesktopWithoutPhoneRoutes(t *testing.T) {
	desktop, _ := entryDocuments(t)
	for _, mode := range []string{"desktop entry", "client only"} {
		s := testServer(t)
		s.desktopEntry = mode == "desktop entry"
		s.clientOnly = mode == "client only"
		if w := rootRequest(s, entryIPhone, "cage-layout=phone", "phone"); !bytes.Equal(w.Body.Bytes(), desktop) {
			t.Fatalf("%s served the phone UI", mode)
		}
	}
}

// The premise of the phone document: a browser session (the workbench cookie,
// nothing else) can use the phone routes on a plain server, and the desktop
// app's gateway refuses them, which is why it never serves the phone UI.
func TestPhoneRoutesAnswerABrowserSession(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/mobile/cli/projects", "/api/mobile/cli/sessions"} {
		if w := request(s, "GET", path, "", "", true); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w := request(s, "GET", path, "", "", false); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without the session: %d", path, w.Code)
		}
	}
	s.desktopEntry = true
	if w := request(s, "GET", "/api/mobile/cli/projects", "", "", true); w.Code != http.StatusConflict {
		t.Fatalf("desktop entry answered the phone route: %d", w.Code)
	}
}
