package workbench

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

type staticFixture struct {
	files    fstest.MapFS
	handler  http.Handler
	main     []byte
	mainGzip []byte
	mainBr   []byte
	lazy     []byte
	solo     []byte
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil || writer.Close() != nil {
		t.Fatal("gzip fixture failed")
	}
	return buffer.Bytes()
}

func gunzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	return out
}

func newStaticFixture(t *testing.T) *staticFixture {
	t.Helper()
	f := &staticFixture{
		main: bytes.Repeat([]byte("export const answer = 42;\n"), 300),
		lazy: bytes.Repeat([]byte("export const lazy = () => 1;\n"), 200),
		solo: bytes.Repeat([]byte(".solo { color: black }\n"), 200),
		// Go has no brotli encoder; the handler never decodes, so any bytes
		// smaller than the file stand in for a build-time companion.
		mainBr: []byte("pretend-brotli-stream"),
	}
	f.mainGzip = gzipBytes(t, f.main)
	f.files = fstest.MapFS{
		"index.html":                           {Data: bytes.Repeat([]byte("<div id=root></div>\n"), 80)},
		"mobile.html":                          {Data: []byte("<div></div>")},
		"cage.svg":                             {Data: []byte("<svg/>")},
		"cage.png":                             {Data: []byte("\x89PNG fake")},
		"build-integrity.json":                 {Data: []byte("{}")},
		"assets/main-AbCd1234.js":              {Data: f.main},
		"assets/main-AbCd1234.js.br":           {Data: f.mainBr},
		"assets/main-AbCd1234.js.gz":           {Data: f.mainGzip},
		"assets/lazy-Zz9_Y8x7.js":              {Data: f.lazy},
		"assets/lazy-Zz9_Y8x7.js.gz":           {Data: gzipBytes(t, f.lazy)},
		"assets/solo-QwErTy12.css":             {Data: f.solo},
		"assets/tiny-AbCd1234.js":              {Data: []byte("export default 1")},
		"assets/logo-AbCd1234.png":             {Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 600)},
		"assets/unhashed.js":                   {Data: f.main},
		"assets/vendor/deep-Qw3rTy9_.woff2":    {Data: bytes.Repeat([]byte{1, 2, 3}, 600)},
		"assets/not-a-hash-manifest-lowercase": {Data: []byte("x")},
	}
	f.handler = newStaticHandler(f.files)
	return f
}

func (f *staticFixture) get(method, target string, header map[string]string) (*http.Response, []byte) {
	r := httptest.NewRequest(method, target, nil)
	for name, value := range header {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	response := w.Result()
	body, _ := io.ReadAll(response.Body)
	return response, body
}

func TestStaticCachePolicyAndContentType(t *testing.T) {
	f := newStaticFixture(t)
	const immutable = "public, max-age=31536000, immutable"
	for _, c := range []struct{ path, cache, kind string }{
		{"/", "no-cache", "text/html; charset=utf-8"},
		{"/mobile.html", "no-cache", "text/html; charset=utf-8"},
		{"/cage.svg", "no-cache", "image/svg+xml"},
		{"/cage.png", "no-cache", "image/png"},
		{"/build-integrity.json", "no-cache", "application/json"},
		{"/assets/main-AbCd1234.js", immutable, "text/javascript; charset=utf-8"},
		{"/assets/lazy-Zz9_Y8x7.js", immutable, "text/javascript; charset=utf-8"},
		{"/assets/solo-QwErTy12.css", immutable, "text/css; charset=utf-8"},
		{"/assets/logo-AbCd1234.png", immutable, "image/png"},
		{"/assets/vendor/deep-Qw3rTy9_.woff2", immutable, "font/woff2"},
		// A file in assets/ without a hash in its name may change under the
		// same URL and must be revalidated.
		{"/assets/unhashed.js", "no-cache", "text/javascript; charset=utf-8"},
		{"/assets/not-a-hash-manifest-lowercase", "no-cache", "application/octet-stream"},
	} {
		response, body := f.get("GET", c.path, nil)
		if response.StatusCode != 200 || response.Header.Get("Cache-Control") != c.cache || response.Header.Get("Content-Type") != c.kind || response.Header.Get("ETag") == "" || len(body) == 0 {
			t.Errorf("%s: %d cache=%q type=%q etag=%q", c.path, response.StatusCode, response.Header.Get("Cache-Control"), response.Header.Get("Content-Type"), response.Header.Get("ETag"))
		}
	}
}

func TestStaticVaryOnlyWhereACodedCopyCanExist(t *testing.T) {
	f := newStaticFixture(t)
	for path, want := range map[string]bool{
		"/": true, "/assets/main-AbCd1234.js": true, "/assets/solo-QwErTy12.css": true,
		"/mobile.html": false, "/cage.svg": false, "/cage.png": false, "/assets/tiny-AbCd1234.js": false, "/assets/logo-AbCd1234.png": false, "/assets/vendor/deep-Qw3rTy9_.woff2": false,
	} {
		for _, encoding := range []string{"", "gzip", "br, gzip"} {
			response, _ := f.get("GET", path, map[string]string{"Accept-Encoding": encoding})
			if got := strings.Contains(response.Header.Get("Vary"), "Accept-Encoding"); got != want {
				t.Errorf("%s with %q: Vary=%q", path, encoding, response.Header.Get("Vary"))
			}
			if !want && response.Header.Get("Content-Encoding") != "" {
				t.Errorf("%s was coded: %q", path, response.Header.Get("Content-Encoding"))
			}
		}
	}
}

func TestStaticEncodingNegotiation(t *testing.T) {
	f := newStaticFixture(t)
	for _, c := range []struct {
		acceptEncoding string
		main, lazy     string // expected Content-Encoding for the file with both companions / only gzip
		solo           string // no companions: gzip is produced in memory, brotli is not
	}{
		{"", "", "", ""},
		{"identity", "", "", ""},
		{"gzip", "gzip", "gzip", "gzip"},
		{"gzip, deflate", "gzip", "gzip", "gzip"},
		{"br", "br", "", ""},
		{"gzip, deflate, br, zstd", "br", "gzip", "gzip"},
		{"br;q=0.5, gzip;q=0.9", "gzip", "gzip", "gzip"},
		{"br;q=0.9, gzip;q=0.5", "br", "gzip", "gzip"},
		{"br;q=0, gzip", "gzip", "gzip", "gzip"},
		{"gzip;q=0", "", "", ""},
		{"gzip;q=0.0, br", "br", "", ""},
		{"*", "br", "gzip", "gzip"},
		{"*;q=0", "", "", ""},
		{"gzip, *;q=0", "gzip", "gzip", "gzip"},
		{"BR, GZIP", "br", "gzip", "gzip"},
		{"x-gzip", "gzip", "gzip", "gzip"},
		{"br;q=bogus, gzip", "gzip", "gzip", "gzip"},
	} {
		for _, target := range []struct{ path, want string }{{"/assets/main-AbCd1234.js", c.main}, {"/assets/lazy-Zz9_Y8x7.js", c.lazy}, {"/assets/solo-QwErTy12.css", c.solo}} {
			response, body := f.get("GET", target.path, map[string]string{"Accept-Encoding": c.acceptEncoding})
			if response.StatusCode != 200 || response.Header.Get("Content-Encoding") != target.want {
				t.Errorf("%s with Accept-Encoding %q: %d encoding %q, want %q", target.path, c.acceptEncoding, response.StatusCode, response.Header.Get("Content-Encoding"), target.want)
				continue
			}
			if length, _ := strconv.Atoi(response.Header.Get("Content-Length")); length != len(body) {
				t.Errorf("%s with %q: Content-Length %q for %d bytes", target.path, c.acceptEncoding, response.Header.Get("Content-Length"), len(body))
			}
			if !strings.Contains(response.Header.Get("Vary"), "Accept-Encoding") {
				t.Errorf("%s with %q: Vary %q", target.path, c.acceptEncoding, response.Header.Get("Vary"))
			}
		}
	}
}

func TestStaticCodedBodiesMatchTheFile(t *testing.T) {
	f := newStaticFixture(t)
	plain, plainBody := f.get("GET", "/assets/main-AbCd1234.js", nil)
	gz, gzBody := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": "gzip"})
	br, brBody := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": "br, gzip"})
	if !bytes.Equal(plainBody, f.main) || !bytes.Equal(gunzipBytes(t, gzBody), f.main) || !bytes.Equal(brBody, f.mainBr) {
		t.Fatal("a representation does not carry the expected bytes")
	}
	if !bytes.Equal(gzBody, f.mainGzip) {
		t.Error("the build-time gzip companion was not served as is")
	}
	// A cached copy must never be revalidated against another coding's tag.
	tags := map[string]bool{plain.Header.Get("ETag"): true, gz.Header.Get("ETag"): true, br.Header.Get("ETag"): true}
	if len(tags) != 3 {
		t.Errorf("representations share ETags: %q %q %q", plain.Header.Get("ETag"), gz.Header.Get("ETag"), br.Header.Get("ETag"))
	}
	for _, response := range []*http.Response{plain, gz, br} {
		if response.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("cache policy %q", response.Header.Get("Cache-Control"))
		}
	}
}

func TestStaticSmallerCompanionRule(t *testing.T) {
	f := newStaticFixture(t)
	// A companion that is not smaller than the file would only cost bytes.
	f.files["assets/lazy-Zz9_Y8x7.js.br"] = &fstest.MapFile{Data: bytes.Repeat([]byte("x"), len(f.lazy)+5)}
	f.files["assets/empty-AbCd1234.js"] = &fstest.MapFile{Data: bytes.Repeat([]byte("var a=1;\n"), 200)}
	f.files["assets/empty-AbCd1234.js.br"] = &fstest.MapFile{}
	for _, path := range []string{"/assets/lazy-Zz9_Y8x7.js", "/assets/empty-AbCd1234.js"} {
		response, _ := f.get("GET", path, map[string]string{"Accept-Encoding": "br"})
		if response.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: unusable brotli companion was served", path)
		}
	}
}

func TestStaticConditionalRequests(t *testing.T) {
	f := newStaticFixture(t)
	for _, c := range []struct{ path, acceptEncoding string }{
		{"/", ""}, {"/", "gzip"}, {"/assets/main-AbCd1234.js", ""}, {"/assets/main-AbCd1234.js", "br"}, {"/assets/solo-QwErTy12.css", "gzip"}, {"/cage.svg", ""},
	} {
		first, _ := f.get("GET", c.path, map[string]string{"Accept-Encoding": c.acceptEncoding})
		etag := first.Header.Get("ETag")
		again, body := f.get("GET", c.path, map[string]string{"Accept-Encoding": c.acceptEncoding, "If-None-Match": etag})
		if again.StatusCode != 304 || len(body) != 0 {
			t.Errorf("%s (%q) with its own validator: %d, %d body bytes", c.path, c.acceptEncoding, again.StatusCode, len(body))
			continue
		}
		// RFC 9111: a 304 refreshes the stored headers, so they must be present.
		for _, name := range []string{"ETag", "Cache-Control", "Vary"} {
			if again.Header.Get(name) != first.Header.Get(name) {
				t.Errorf("%s (%q): 304 %s=%q, 200 had %q", c.path, c.acceptEncoding, name, again.Header.Get(name), first.Header.Get(name))
			}
		}
		if stale, _ := f.get("GET", c.path, map[string]string{"Accept-Encoding": c.acceptEncoding, "If-None-Match": `"stale"`}); stale.StatusCode != 200 {
			t.Errorf("%s: unknown validator answered %d", c.path, stale.StatusCode)
		}
	}
	// A validator from another coding of the same file must not match.
	plain, _ := f.get("GET", "/assets/main-AbCd1234.js", nil)
	if other, _ := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": plain.Header.Get("ETag")}); other.StatusCode != 200 {
		t.Errorf("identity validator matched the brotli representation: %d", other.StatusCode)
	}
}

func TestStaticHeadMatchesGet(t *testing.T) {
	f := newStaticFixture(t)
	for _, encoding := range []string{"", "gzip", "br"} {
		get, body := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": encoding})
		head, headBody := f.get("HEAD", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": encoding})
		if head.StatusCode != 200 || len(headBody) != 0 {
			t.Fatalf("HEAD %q: %d with %d body bytes", encoding, head.StatusCode, len(headBody))
		}
		for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Length", "ETag", "Cache-Control", "Vary"} {
			if head.Header.Get(name) != get.Header.Get(name) {
				t.Errorf("HEAD %q: %s=%q, GET has %q", encoding, name, head.Header.Get(name), get.Header.Get(name))
			}
		}
		if head.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
			t.Errorf("HEAD %q: Content-Length %q for a %d byte body", encoding, head.Header.Get("Content-Length"), len(body))
		}
	}
}

func TestStaticRangeServesIdentityBytes(t *testing.T) {
	f := newStaticFixture(t)
	response, body := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Range": "bytes=10-19", "Accept-Encoding": "br, gzip"})
	if response.StatusCode != 206 || response.Header.Get("Content-Encoding") != "" || !bytes.Equal(body, f.main[10:20]) {
		t.Fatalf("range with Accept-Encoding: %d encoding %q body %q", response.StatusCode, response.Header.Get("Content-Encoding"), body)
	}
	if response.Header.Get("Content-Range") != "bytes 10-19/"+strconv.Itoa(len(f.main)) || response.Header.Get("Content-Length") != "10" || !strings.Contains(response.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("range headers: %v", response.Header)
	}
	if response.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges %q", response.Header.Get("Accept-Ranges"))
	}
	// A suffix range and a range on a file served without coding work too.
	if tail, tailBody := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Range": "bytes=-5"}); tail.StatusCode != 206 || !bytes.Equal(tailBody, f.main[len(f.main)-5:]) {
		t.Errorf("suffix range: %d %q", tail.StatusCode, tailBody)
	}
	// If-Range with a validator of another representation yields the whole file.
	br, _ := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": "br"})
	if full, fullBody := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Range": "bytes=0-9", "If-Range": br.Header.Get("ETag")}); full.StatusCode != 200 || !bytes.Equal(fullBody, f.main) {
		t.Errorf("If-Range with a stale validator: %d", full.StatusCode)
	}
	bad, _ := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Range": "bytes=999999-"})
	if bad.StatusCode != 416 || bad.Header.Get("Cache-Control") != "no-store" || bad.Header.Get("Content-Encoding") != "" {
		t.Errorf("unsatisfiable range: %d cache=%q", bad.StatusCode, bad.Header.Get("Cache-Control"))
	}
}

func TestStaticFailedPreconditionIsAPlainError(t *testing.T) {
	f := newStaticFixture(t)
	response, body := f.get("GET", "/assets/main-AbCd1234.js", map[string]string{"Accept-Encoding": "br", "If-Match": `"nothing"`})
	if response.StatusCode != 412 || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store" || bytes.Contains(body, f.mainBr) {
		t.Errorf("If-Match mismatch: %d encoding=%q cache=%q body=%q", response.StatusCode, response.Header.Get("Content-Encoding"), response.Header.Get("Cache-Control"), body)
	}
	if length := response.Header.Get("Content-Length"); length != "" && length != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %q for a %d byte error", length, len(body))
	}
}

func TestStaticContentTypeIgnoresTheHostMIMERegistry(t *testing.T) {
	// On Windows the registry can map .js to text/plain; with nosniff the
	// browser then refuses to run the page's modules.
	for extension, original := range map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"} {
		if err := mime.AddExtensionType(extension, "text/plain"); err != nil {
			t.Fatal(err)
		}
		defer mime.AddExtensionType(extension, original)
	}
	if !strings.HasPrefix(mime.TypeByExtension(".js"), "text/plain") {
		t.Fatal("fixture did not override the host MIME table")
	}
	f := newStaticFixture(t)
	for path, want := range map[string]string{"/assets/main-AbCd1234.js": "text/javascript; charset=utf-8", "/assets/solo-QwErTy12.css": "text/css; charset=utf-8", "/cage.svg": "image/svg+xml"} {
		if response, _ := f.get("GET", path, nil); response.Header.Get("Content-Type") != want {
			t.Errorf("%s served as %q", path, response.Header.Get("Content-Type"))
		}
	}
}

func TestStaticRefusesWhatIsNotAFile(t *testing.T) {
	f := newStaticFixture(t)
	f.files["api/secret.js"] = &fstest.MapFile{Data: []byte("x")}
	for _, path := range []string{
		"/assets/", "/assets", "/assets/vendor/", "/missing.js", "/assets/missing-AbCd1234.js", "/../../etc/passwd",
		"/assets/main-AbCd1234.js.br", "/assets/main-AbCd1234.js.gz", // build-time companions are internal
		"/api/status", "/api/", "/api/secret.js", "/api",
	} {
		response, _ := f.get("GET", path, nil)
		if response.StatusCode != 404 || response.Header.Get("ETag") != "" || strings.Contains(response.Header.Get("Cache-Control"), "immutable") || response.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: %d etag=%q cache=%q", path, response.StatusCode, response.Header.Get("ETag"), response.Header.Get("Cache-Control"))
		}
	}
	if response, _ := f.get("POST", "/", nil); response.StatusCode != 405 || response.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	if response, _ := f.get("GET", "/index.html?x=1", nil); response.StatusCode != 301 || response.Header.Get("Location") != "./?x=1" {
		t.Errorf("index.html: %d %q", response.StatusCode, response.Header.Get("Location"))
	}
}

func TestStaticGzipFallbackIsBuiltOnceAndSafeUnderLoad(t *testing.T) {
	f := newStaticFixture(t)
	var wg sync.WaitGroup
	tags := make(chan string, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, body := f.get("GET", "/assets/solo-QwErTy12.css", map[string]string{"Accept-Encoding": "gzip"})
			if response.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(gunzipBytes(t, body), f.solo) {
				t.Errorf("fallback gzip: %d %q", response.StatusCode, response.Header.Get("Content-Encoding"))
			}
			tags <- response.Header.Get("ETag")
		}()
	}
	wg.Wait()
	close(tags)
	seen := map[string]bool{}
	for tag := range tags {
		seen[tag] = true
	}
	if len(seen) != 1 {
		t.Errorf("fallback gzip is not stable across requests: %v", seen)
	}
}

// An older tenant gateway drops the browser's Accept-Encoding and forwards
// only a whitelist; Go's transport then asks for gzip by itself and unpacks
// the answer. That path must keep working byte for byte.
func TestStaticServesAClientThatDecodesGzipItself(t *testing.T) {
	f := newStaticFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	response, err := http.Get(server.URL + "/assets/main-AbCd1234.js")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || !response.Uncompressed || !bytes.Equal(body, f.main) {
		t.Fatalf("transparent gzip: uncompressed=%v err=%v equal=%v", response.Uncompressed, err, bytes.Equal(body, f.main))
	}
	// With a Range header the transport does not ask for gzip at all.
	request, _ := http.NewRequest("GET", server.URL+"/assets/main-AbCd1234.js", nil)
	request.Header.Set("Range", "bytes=0-3")
	ranged, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer ranged.Body.Close()
	rangedBody, _ := io.ReadAll(ranged.Body)
	if ranged.StatusCode != 206 || !bytes.Equal(rangedBody, f.main[:4]) {
		t.Fatalf("range through a plain client: %d %q", ranged.StatusCode, rangedBody)
	}
}

func TestAcceptedStaticCodings(t *testing.T) {
	for _, c := range []struct {
		values   []string
		br, gzip float64
	}{
		{nil, 0, 0},
		{[]string{"gzip"}, 0, 1},
		{[]string{"br, gzip;q=0.5"}, 1, 0.5},
		{[]string{"*"}, 1, 1},
		{[]string{"*;q=0.3, gzip"}, 0.3, 1},
		{[]string{"gzip", "br;q=0.2"}, 0.2, 1},
		{[]string{"br;q=2"}, 1, 0},
		{[]string{"gzip;q=-1"}, 0, 0},
	} {
		got1, got2 := acceptedStaticCodings(http.Header{"Accept-Encoding": c.values})
		if got1 != c.br || got2 != c.gzip {
			t.Errorf("%q: br=%v gzip=%v, want %v %v", c.values, got1, got2, c.br, c.gzip)
		}
	}
}
