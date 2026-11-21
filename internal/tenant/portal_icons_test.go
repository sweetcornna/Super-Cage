package tenant

import (
	"encoding/json"
	"strings"
	"testing"
)

// The portal can be added to a phone's home screen: manifest, icons and a CSP that lets them load.
func TestPortalManifestAndIcons(t *testing.T) {
	fixture := newFixture(t, false)
	page := fixture.request("GET", portalOrigin, "/", "", nil)
	csp := page.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "manifest-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("portal CSP: %s", csp)
	}
	manifest := fixture.request("GET", portalOrigin, "/manifest.webmanifest", "", nil)
	wantStatus(t, manifest, 200, "")
	var parsed struct {
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Icons    []struct {
			Src     string `json:"src"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if manifest.Header().Get("Content-Type") != "application/manifest+json" || json.Unmarshal(manifest.Body.Bytes(), &parsed) != nil || parsed.StartURL != "/" || parsed.Display != "standalone" || len(parsed.Icons) != 3 {
		t.Fatalf("manifest: %s", manifest.Body.String())
	}
	for _, icon := range append([]string{"/favicon.ico", "/icon.svg", "/apple-touch-icon.png"}, parsed.Icons[0].Src, parsed.Icons[1].Src, parsed.Icons[2].Src) {
		response := fixture.request("GET", portalOrigin, icon, "", nil)
		wantStatus(t, response, 200, "")
		if response.Body.Len() < 100 {
			t.Fatalf("%s is empty", icon)
		}
	}
	wantStatus(t, fixture.request("GET", portalOrigin, "/icon-1.png", "", nil), 404, "not_found")
	wantStatus(t, fixture.request("GET", portalOrigin, "/portal-icons/icon-192.png", "", nil), 404, "not_found")
}
