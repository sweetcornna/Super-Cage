package workbench

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests drive the real handler stack (host check, security headers,
// routing) over whatever UI build is embedded, so they hold for any valid
// build: they only look at the entry page and one asset it references.

func embeddedEntryAsset(t *testing.T) string {
	t.Helper()
	web, err := fs.Sub(assets, "web")
	if err != nil {
		t.Fatal(err)
	}
	html, err := fs.ReadFile(web, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`src="(/assets/[^"]+\.js)"`).FindSubmatch(html)
	if match == nil {
		t.Skip("the embedded web directory holds no built UI (run npm --prefix ui run build)")
	}
	return string(match[1])
}

func staticRequest(s *Server, method, path string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, nil)
	for name, value := range header {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func TestWorkbenchServesEntryPageRevalidatedAndAssetsImmutable(t *testing.T) {
	s := testServer(t)
	page := staticRequest(s, "GET", "/", nil)
	if page.Code != 200 || page.Header().Get("Cache-Control") != "no-cache" || page.Header().Get("ETag") == "" {
		t.Fatalf("entry page: %d cache=%q etag=%q", page.Code, page.Header().Get("Cache-Control"), page.Header().Get("ETag"))
	}
	if again := staticRequest(s, "GET", "/", map[string]string{"If-None-Match": page.Header().Get("ETag")}); again.Code != 304 {
		t.Fatalf("matching validator: %d", again.Code)
	}
	script := embeddedEntryAsset(t)
	asset := staticRequest(s, "GET", script, nil)
	if asset.Code != 200 || asset.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset: %d cache=%q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	if kind := asset.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/javascript") {
		t.Fatalf("script type %q", kind)
	}
	for _, response := range []*httptest.ResponseRecorder{page, asset} {
		header := response.Header()
		if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Referrer-Policy") != "no-referrer" || header.Get("X-Frame-Options") != "DENY" || !strings.Contains(header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatalf("security headers lost: %v", header)
		}
	}
}

func TestWorkbenchServesTheEmbeddedBuildCompressed(t *testing.T) {
	s := testServer(t)
	script := embeddedEntryAsset(t)
	web, _ := fs.Sub(assets, "web")
	identity, err := fs.ReadFile(web, strings.TrimPrefix(script, "/"))
	if err != nil {
		t.Fatal(err)
	}
	gz := staticRequest(s, "GET", script, map[string]string{"Accept-Encoding": "gzip"})
	if gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("Vary") != "Accept-Encoding" || gz.Header().Get("Content-Length") != strconv.Itoa(gz.Body.Len()) {
		t.Fatalf("gzip: encoding=%q vary=%q length=%q", gz.Header().Get("Content-Encoding"), gz.Header().Get("Vary"), gz.Header().Get("Content-Length"))
	}
	if body := gunzipBytes(t, gz.Body.Bytes()); !bytes.Equal(body, identity) {
		t.Fatal("gzip body differs from the embedded file")
	}
	// Brotli is served when the build carries a companion, else the client
	// falls back to gzip: both are valid for a build made without the step.
	br := staticRequest(s, "GET", script, map[string]string{"Accept-Encoding": "br, gzip"})
	if companion, err := fs.ReadFile(web, strings.TrimPrefix(script, "/")+".br"); err == nil {
		if br.Header().Get("Content-Encoding") != "br" || !bytes.Equal(br.Body.Bytes(), companion) {
			t.Fatalf("brotli companion not served: encoding=%q", br.Header().Get("Content-Encoding"))
		}
	} else if br.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("no brotli companion, encoding=%q", br.Header().Get("Content-Encoding"))
	}
	if plain := staticRequest(s, "GET", script, nil); plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), identity) {
		t.Fatal("a client that sends no Accept-Encoding must get the file as it is")
	}
	if ranged := staticRequest(s, "GET", script, map[string]string{"Accept-Encoding": "br, gzip", "Range": "bytes=0-15"}); ranged.Code != 206 || !bytes.Equal(ranged.Body.Bytes(), identity[:16]) {
		t.Fatalf("range: %d", ranged.Code)
	}
	if head := staticRequest(s, "HEAD", script, map[string]string{"Accept-Encoding": "gzip"}); head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != gz.Header().Get("Content-Length") {
		t.Fatalf("HEAD: %d length=%q", head.Code, head.Header().Get("Content-Length"))
	}
}

func TestWorkbenchNeverCachesAPIOrErrors(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/status", "/api/session", "/api/no-such-route", "/assets/missing-AbCdEf12.js", "/assets/"} {
		w := staticRequest(s, "GET", path, nil)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d cache=%q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	if w := staticRequest(s, "GET", "/api/no-such-route", map[string]string{"Cookie": s.cookieName() + "=" + s.session}); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unknown API route: %d cache=%q", w.Code, w.Header().Get("Cache-Control"))
	}
}
