package tenant

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// brotliBytes stands in for a precompressed brotli body: the gateway must pass
// it on byte for byte, so it only needs to be recognizable.
var brotliBytes = []byte{0x1b, 0x2a, 0x00, 0xf8, 0xff, 0x03, 0x80, 0x21, 0x5a, 0x7e}

const (
	scriptBody  = "console.log('workbench');\n"
	immutable   = "public, max-age=31536000, immutable"
	browserSide = "private, max-age=31536000, immutable"
)

func gzipped(body string) []byte {
	var packed bytes.Buffer
	compressor := gzip.NewWriter(&packed)
	_, _ = compressor.Write([]byte(body))
	_ = compressor.Close()
	return packed.Bytes()
}

// staticGuest answers like the workbench's static handler: hashed assets with
// a year-long immutable lifetime, other files revalidated with an ETag, one
// strong ETag per representation, brotli and gzip chosen by Accept-Encoding,
// Range answered from the identity bytes. Paths of an older workbench answer
// no-store and nothing else.
func staticGuest(id string) http.Handler {
	base := fixtureGuest(id)
	files := map[string]string{ // path -> cache policy
		"/assets/app-abc12345.js":  immutable,
		"/":                        "no-cache",
		"/assets/odd-abc12345.js":  "max-age=60, s-maxage=1, foo",
		"/assets/huge-abc12345.js": "public, max-age=99999999999",
		"/old/app.js":              "no-store",
		"/old/silent.js":           "",
		"/api/static.js":           immutable,
		"/missing/abort.js":        immutable,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy, known := files[r.URL.Path]
		if !known || r.Header.Get("Cookie") != guestCookie+"="+fixtureSession {
			base.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/missing/abort.js" {
			panic(http.ErrAbortHandler)
		}
		body, etag, encoding := []byte(scriptBody), `"identity"`, ""
		accept := r.Header.Get("Accept-Encoding")
		if !strings.HasPrefix(r.URL.Path, "/old/") && r.Header.Get("Range") == "" {
			switch {
			case strings.Contains(accept, "br"):
				body, etag, encoding = brotliBytes, `"br"`, "br"
			case strings.Contains(accept, "gzip"):
				body, etag, encoding = gzipped(scriptBody), `"gzip"`, "gzip"
			}
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("X-Guest-Accept-Encoding", accept)
		if policy != "" {
			w.Header().Set("Cache-Control", policy)
		}
		if !strings.HasPrefix(r.URL.Path, "/old/") {
			w.Header().Set("ETag", etag)
			w.Header().Add("Vary", "Accept-Encoding")
		}
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	})
}

type staticFixture struct {
	*portalFixture
	cookie *http.Cookie
	id     string
}

func newStaticFixture(t *testing.T) *staticFixture {
	t.Helper()
	fixture := newFixture(t, false)
	fixture.runtime.guestHandler = staticGuest
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	return &staticFixture{portalFixture: fixture, cookie: fixture.connect(alice, record.ID), id: record.ID}
}

func (f *staticFixture) get(method, path string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Host = strings.TrimPrefix(vmOrigin(f.id), "https://")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return f.serve(request)
}

func cacheControl(response *httptest.ResponseRecorder) string {
	values := response.Header().Values("Cache-Control")
	if len(values) != 1 {
		return "<" + strings.Join(values, " | ") + ">"
	}
	return values[0]
}

func TestTenantPassesStaticCachingAndCompressionThrough(t *testing.T) {
	f := newStaticFixture(t)

	// A brotli asset arrives byte for byte, with exactly one Cache-Control (the
	// gateway's own no-store is gone), its ETag and Vary.
	response := f.get("GET", "/assets/app-abc12345.js", f.cookie, map[string]string{"Accept-Encoding": "br, gzip"})
	if response.Code != 200 || cacheControl(response) != browserSide {
		t.Fatalf("asset: HTTP %d, Cache-Control %s", response.Code, cacheControl(response))
	}
	if response.Header().Get("Content-Encoding") != "br" || !bytes.Equal(response.Body.Bytes(), brotliBytes) {
		t.Fatalf("brotli body changed: %q %x", response.Header().Get("Content-Encoding"), response.Body.Bytes())
	}
	if response.Header().Get("ETag") != `"br"` || response.Header().Values("Vary")[0] != "Accept-Encoding" || len(response.Header().Values("Vary")) != 1 {
		t.Fatalf("validators: %v", response.Header())
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("X-Guest-Accept-Encoding") != "" {
		t.Fatalf("gateway headers changed or a guest header leaked: %v", response.Header())
	}
	if response.Header().Get("Content-Length") != "10" {
		t.Fatalf("Content-Length %q", response.Header().Get("Content-Length"))
	}

	// Gzip is passed on as gzip, with its own ETag.
	response = f.get("GET", "/assets/app-abc12345.js", f.cookie, map[string]string{"Accept-Encoding": "gzip"})
	reader, err := gzip.NewReader(response.Body)
	if err != nil || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("ETag") != `"gzip"` {
		t.Fatalf("gzip asset: %q %q %v", response.Header().Get("Content-Encoding"), response.Header().Get("ETag"), err)
	}
	if plain, _ := io.ReadAll(reader); string(plain) != scriptBody {
		t.Fatalf("gzip asset decodes to %q", plain)
	}

	// A client without Accept-Encoding gets the identity bytes and the identity
	// ETag, not the gateway's own gzip negotiation.
	response = f.get("GET", "/assets/app-abc12345.js", f.cookie, nil)
	if response.Header().Get("Content-Encoding") != "" || response.Body.String() != scriptBody || response.Header().Get("ETag") != `"identity"` {
		t.Fatalf("plain asset: %q %q %q", response.Header().Get("Content-Encoding"), response.Body.String(), response.Header().Get("ETag"))
	}

	// Revalidation: the validator reaches the workbench; its 304 keeps the policy.
	response = f.get("GET", "/assets/app-abc12345.js", f.cookie, map[string]string{"Accept-Encoding": "br", "If-None-Match": `"br"`})
	if response.Code != 304 || cacheControl(response) != browserSide || response.Header().Get("ETag") != `"br"` || response.Body.Len() != 0 {
		t.Fatalf("revalidation: HTTP %d %v", response.Code, response.Header())
	}

	// Range is answered from the identity bytes.
	response = f.get("GET", "/assets/app-abc12345.js", f.cookie, map[string]string{"Accept-Encoding": "br", "Range": "bytes=0-6"})
	if response.Code != 206 || response.Body.String() != scriptBody[:7] || response.Header().Get("Content-Range") != "bytes 0-6/"+strconv.Itoa(len(scriptBody)) || response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("range: HTTP %d %q %v", response.Code, response.Body.String(), response.Header())
	}
	if cacheControl(response) != browserSide || response.Header().Get("Accept-Ranges") != "bytes" || response.Header().Get("ETag") != `"identity"` {
		t.Fatalf("range headers: %v", response.Header())
	}

	// HTML is revalidated: no-cache with its ETag, and a 304 for the browser.
	response = f.get("GET", "/", f.cookie, map[string]string{"Accept-Encoding": "gzip"})
	if response.Code != 200 || cacheControl(response) != "no-cache" || response.Header().Get("ETag") != `"gzip"` {
		t.Fatalf("html: HTTP %d, Cache-Control %s, ETag %q", response.Code, cacheControl(response), response.Header().Get("ETag"))
	}
	if response = f.get("GET", "/", f.cookie, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": `"gzip"`}); response.Code != 304 || cacheControl(response) != "no-cache" {
		t.Fatalf("html revalidation: HTTP %d %s", response.Code, cacheControl(response))
	}

	if response = f.get("HEAD", "/assets/app-abc12345.js", f.cookie, nil); cacheControl(response) != browserSide || response.Body.Len() != 0 {
		t.Fatalf("HEAD: %v", response.Header())
	}
	if response = f.get("GET", "/assets/huge-abc12345.js", f.cookie, nil); cacheControl(response) != "private, max-age=31536000" {
		t.Fatalf("a very long lifetime must be capped: %s", cacheControl(response))
	}
}

func TestTenantKeepsEverythingElseUncacheable(t *testing.T) {
	f := newStaticFixture(t)
	// An older workbench (identity, no-store, no validators), an unknown policy,
	// no policy, an API path that claims to be public, and other methods.
	for _, path := range []string{"/old/app.js", "/old/silent.js", "/assets/odd-abc12345.js", "/api/static.js"} {
		response := f.get("GET", path, f.cookie, map[string]string{"Accept-Encoding": "br"})
		if response.Code != 200 || cacheControl(response) != "no-store" || response.Header().Get("ETag") != "" || response.Header().Get("Vary") != "" {
			t.Fatalf("%s: HTTP %d, Cache-Control %s, ETag %q, Vary %q", path, response.Code, cacheControl(response), response.Header().Get("ETag"), response.Header().Get("Vary"))
		}
	}
	if response := f.get("GET", "/old/app.js", f.cookie, nil); response.Body.String() != scriptBody || response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("an older workbench's asset changed: %q", response.Body.String())
	}
	if response := f.get("POST", "/assets/app-abc12345.js", f.cookie, nil); cacheControl(response) != "no-store" {
		t.Fatalf("POST: %s", cacheControl(response))
	}
	// The gateway's own failures and answers keep no-store too.
	wantStatus(t, f.get("GET", "/missing/abort.js", f.cookie, nil), 503, "runtime_unavailable")
	wantStatus(t, f.get("GET", "/assets/app-abc12345.js", nil, nil), 401, "unauthenticated")
	if response := f.get("GET", "/missing/nothing.js", f.cookie, nil); response.Code != 200 && cacheControl(response) != "no-store" {
		t.Fatalf("a guest answer without a policy: HTTP %d %s", response.Code, cacheControl(response))
	}
}

func TestAssetCacheControlAcceptsOnlyWhatItUnderstands(t *testing.T) {
	for _, test := range []struct {
		values []string
		want   string
		ok     bool
	}{
		{[]string{"public, max-age=31536000, immutable"}, "private, max-age=31536000, immutable", true},
		{[]string{"Max-Age=60", "Must-Revalidate"}, "max-age=60, must-revalidate", true},
		{[]string{"private, private"}, "private", true},
		{[]string{"max-age=99999999999999999999999"}, "max-age=31536000", true},
		{[]string{"no-cache"}, "no-cache", true},
		{[]string{"no-store"}, "", false},
		{[]string{"max-age=60, no-store"}, "", false},
		{[]string{"max-age="}, "", false},
		{[]string{"max-age=-1"}, "", false},
		{[]string{"max-age=1.5"}, "", false},
		{[]string{"s-maxage=60"}, "", false},
		{[]string{"max-age=60, community=x"}, "", false},
		{[]string{""}, "", false},
		{nil, "", false},
	} {
		got, ok := assetCacheControl(test.values)
		if got != test.want || ok != test.ok {
			t.Errorf("assetCacheControl(%q) = %q, %v; want %q, %v", test.values, got, ok, test.want, test.ok)
		}
	}
}
