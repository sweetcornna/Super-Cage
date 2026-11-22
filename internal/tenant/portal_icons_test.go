package tenant

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
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

// The portal's own files travel gzipped to browsers that accept it, and stay uncached like every gateway page.
func TestPortalFilesAreCompressed(t *testing.T) {
	fixture := newFixture(t, false)
	for _, path := range []string{"/", "/index.html"} {
		plain := fixture.request("GET", portalOrigin, path, "", nil)
		wantStatus(t, plain, 200, "")
		if plain.Header().Get("Content-Encoding") != "" || plain.Body.Len() == 0 {
			t.Fatalf("%s without Accept-Encoding: %q", path, plain.Header().Get("Content-Encoding"))
		}
		request := httptest.NewRequest("GET", path, nil)
		request.Host = strings.TrimPrefix(portalOrigin, "https://")
		request.Host = strings.TrimPrefix(request.Host, "http://")
		request.Header.Set("Accept-Encoding", "br, gzip;q=0.8")
		packed := fixture.serve(request)
		wantStatus(t, packed, 200, "")
		if packed.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(packed.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s: encoding %q vary %q", path, packed.Header().Get("Content-Encoding"), packed.Header().Get("Vary"))
		}
		reader, err := gzip.NewReader(bytes.NewReader(packed.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		unpacked, _ := io.ReadAll(reader)
		if !bytes.Equal(unpacked, plain.Body.Bytes()) {
			t.Fatalf("%s: gzip body does not match (%d -> %d)", path, plain.Body.Len(), packed.Body.Len())
		}
	}
	// The real script shrinks to well under half.
	if script, err := os.ReadFile("../../ui/tenant/app.mjs"); err == nil {
		request := httptest.NewRequest("GET", "/app.mjs", nil)
		request.Header.Set("Accept-Encoding", "gzip")
		response := httptest.NewRecorder()
		servePortalFile(response, request, "text/javascript; charset=utf-8", script)
		if response.Header().Get("Content-Encoding") != "gzip" || response.Body.Len()*3 > len(script) {
			t.Fatalf("app.mjs: %d -> %d bytes", len(script), response.Body.Len())
		}
	}
	if acceptsGzip("gzip;q=0") || acceptsGzip("br") || !acceptsGzip("*") || !acceptsGzip("deflate, GZIP") {
		t.Fatal("Accept-Encoding parsing")
	}
}
