package tenant

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gateway decides the root document for the workspace and sends only the
// result: never the browser's user agent, cookies or a browser-sent hint.
func TestTenantEntryHintReachesOnlyTheRootDocument(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "phone")
	cookie := fixture.connect(alice, record.ID)
	host := strings.TrimPrefix(vmOrigin(record.ID), "https://")
	const iPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	const mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"
	root := func(method, path, agent, layout, forged string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = host
		r.Header.Set("User-Agent", agent)
		r.AddCookie(cookie)
		if layout != "" {
			r.AddCookie(&http.Cookie{Name: "cage-layout", Value: layout})
		}
		if forged != "" {
			r.Header.Set("X-Cage-Entry", forged)
		}
		return fixture.serve(r)
	}
	guestCookie := guestCookie + "=" + fixtureSession
	for _, test := range []struct {
		name, agent, layout, forged, want string
	}{
		{"phone", iPhone, "", "", "phone"},
		{"desktop", mac, "", "", "desktop"},
		{"phone chose the desktop", iPhone, "desktop", "", "desktop"},
		{"desktop chose the phone", mac, "phone", "", "phone"},
		{"tablet page guard", mac, "phone-auto", "", "phone"},
		{"browser cannot forge the hint", mac, "", "phone", "desktop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := root("GET", "/?echo-entry", test.agent, test.layout, test.forged)
			wantStatus(t, response, 200, "")
			if got, want := response.Body.String(), test.want+"||"+guestCookie; got != want {
				t.Fatalf("workspace saw %q, want %q", got, want)
			}
		})
	}
	// Any other request carries no hint, whatever the browser sends.
	for _, path := range []string{"/index.html?echo-entry", "/mobile.html?echo-entry", "/api/anything?echo-entry"} {
		response := root("GET", path, iPhone, "phone", "phone")
		if got := response.Body.String(); got != "||"+guestCookie {
			t.Fatalf("%s: workspace saw %q", path, got)
		}
	}
	if response := root("HEAD", "/?echo-entry", iPhone, "", ""); response.Code != 200 {
		t.Fatalf("HEAD /: %d", response.Code)
	}
}
