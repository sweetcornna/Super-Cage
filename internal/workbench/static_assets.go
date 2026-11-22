package workbench

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/webentry"
)

// The workbench UI is a static build embedded in the executable and reached
// over links that can be slow (the tenant portal, a remote desktop client).
// This handler replaces http.FileServer for it:
//
//   - files under assets/ carry a content hash in their name (the build
//     enforces it, see ui/scripts/verify-build.mjs), so they are served with a
//     year-long immutable lifetime and never downloaded twice;
//   - every other file (the HTML entry pages, icons, the integrity manifest)
//     is revalidated on each use with a content-hash ETag and costs one
//     304 when unchanged;
//   - compressible files are served from precompressed companions written at
//     build time (name.br, name.gz) chosen by Accept-Encoding; a missing gzip
//     companion is produced once in memory, a missing brotli one falls back to
//     gzip or identity;
//   - Range requests are answered from the identity bytes, HEAD and
//     conditional requests behave as for any file, and Content-Type comes from
//     a fixed table because mime.TypeByExtension depends on the host OS (a
//     Windows registry can map .js to text/plain, which nosniff then refuses).
//
// Only successful responses get these cache headers. Errors and anything under
// /api/ keep the no-store set by the surrounding handler.

// Files smaller than this are not worth a second representation. Keep in sync
// with MIN_COMPRESS_BYTES in ui/scripts/precompress.mjs.
const staticMinCompressBytes = 1024

const staticImmutableCache = "public, max-age=31536000, immutable"

// hashedStaticAsset matches build output whose name ends in an 8-character
// content hash, such as assets/main-DGbUz_fZ.js. A name must change whenever
// the bytes do, or "immutable" would pin stale code in the browser.
var hashedStaticAsset = regexp.MustCompile(`^assets/(?:[^/]+/)*[^/]+-[A-Za-z0-9_-]{8}(?:\.[A-Za-z0-9]+)+$`)

var staticContentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "text/xml; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".wasm":        "application/wasm",
}

// Extensions worth compressing; images and woff/woff2 are already compressed.
// Keep in sync with COMPRESSIBLE in ui/scripts/precompress.mjs.
var staticCompressible = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".json": true, ".map": true,
	".webmanifest": true, ".txt": true, ".xml": true, ".svg": true, ".wasm": true, ".ttf": true, ".otf": true,
}

func staticContentType(name string) string {
	extension := strings.ToLower(path.Ext(name))
	if kind, ok := staticContentTypes[extension]; ok {
		return kind
	}
	if kind := mime.TypeByExtension(extension); kind != "" {
		return kind
	}
	return "application/octet-stream"
}

// staticRepresentation is one concrete byte sequence of a file with its own
// strong validator, so a brotli copy and the identity copy never share an ETag.
type staticRepresentation struct {
	data []byte
	etag string
}

func newStaticRepresentation(data []byte) *staticRepresentation {
	sum := sha256.Sum256(data)
	return &staticRepresentation{data: data, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

type staticAsset struct {
	name         string
	contentType  string
	cacheControl string
	// varies is set when a coded representation may exist for this file, so
	// every response for it, coded or not, must say it depends on Accept-Encoding.
	varies   bool
	identity *staticRepresentation

	brotliOnce, gzipOnce sync.Once
	brotli, gzip         *staticRepresentation
}

type staticHandler struct {
	files  fs.FS
	mu     sync.Mutex
	assets map[string]*staticAsset
	// phone, when set, picks the document at "/": true serves the phone UI
	// (mobile.html) there instead of the desktop workbench (index.html).
	phone func(*http.Request) bool
	// offered reports whether this server can serve the phone UI at "/" at all;
	// its desktop document then carries phoneEntryMeta.
	offered        func() bool
	offeredDesktop *staticAsset
}

// phoneEntryMeta marks the desktop document of a server that can also serve the
// phone UI at "/" (read by ui/src/entry-layout.mjs).
const phoneEntryMeta = `<meta name="cage-phone-entry" content="available">`

func newStaticHandler(files fs.FS) http.Handler {
	return &staticHandler{files: files, assets: map[string]*staticAsset{}}
}

// newEntryStaticHandler serves files like newStaticHandler and answers "/" with
// the document phone chooses (see internal/webentry). Both documents live at the
// same URL, so deep links, reloads and Back are the same in either layout.
func newEntryStaticHandler(files fs.FS, phone func(*http.Request) bool, offered func() bool) http.Handler {
	return &staticHandler{files: files, assets: map[string]*staticAsset{}, phone: phone, offered: offered}
}

// offeredDesktopAsset is index.html with phoneEntryMeta after <head>: its own
// bytes, validator and (in-memory) compression, never index.html's companions.
func (h *staticHandler) offeredDesktopAsset(plain *staticAsset) *staticAsset {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.offeredDesktop != nil {
		return h.offeredDesktop
	}
	data := bytes.Replace(plain.identity.data, []byte("<head>"), []byte("<head>"+phoneEntryMeta), 1)
	if bytes.Equal(data, plain.identity.data) {
		return plain
	}
	h.offeredDesktop = &staticAsset{
		name: "index.html (phone entry)", contentType: plain.contentType, cacheControl: plain.cacheControl,
		identity: newStaticRepresentation(data),
		varies:   staticCompressible[".html"] && len(data) >= staticMinCompressBytes,
	}
	return h.offeredDesktop
}

// companion reports whether name is a build-time compressed copy of another
// file; those are internal and are never served under their own name.
func (h *staticHandler) companion(name string) bool {
	for _, suffix := range []string{".br", ".gz"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			if info, err := fs.Stat(h.files, base); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

// lookup loads a file once and keeps it. The embedded tree is immutable and
// small, so keeping the bytes costs less than hashing them per request.
func (h *staticHandler) lookup(name string) *staticAsset {
	h.mu.Lock()
	defer h.mu.Unlock()
	if asset := h.assets[name]; asset != nil {
		return asset
	}
	if !fs.ValidPath(name) {
		return nil
	}
	if info, err := fs.Stat(h.files, name); err != nil || !info.Mode().IsRegular() || h.companion(name) {
		return nil
	}
	data, err := fs.ReadFile(h.files, name)
	if err != nil {
		return nil
	}
	asset := &staticAsset{name: name, contentType: staticContentType(name), cacheControl: "no-cache", identity: newStaticRepresentation(data)}
	if hashedStaticAsset.MatchString(name) {
		asset.cacheControl = staticImmutableCache
	}
	asset.varies = staticCompressible[strings.ToLower(path.Ext(name))] && len(data) >= staticMinCompressBytes
	h.assets[name] = asset
	return asset
}

// companionData reads a build-time companion; one that is empty or not smaller
// than the file is ignored, as the build would not have written it.
func (h *staticHandler) companionData(asset *staticAsset, suffix string) *staticRepresentation {
	data, err := fs.ReadFile(h.files, asset.name+suffix)
	if err != nil || len(data) == 0 || len(data) >= len(asset.identity.data) {
		return nil
	}
	return newStaticRepresentation(data)
}

func (h *staticHandler) brotliOf(asset *staticAsset) *staticRepresentation {
	asset.brotliOnce.Do(func() { asset.brotli = h.companionData(asset, ".br") })
	return asset.brotli
}

// gzipOf prefers the build-time companion and otherwise compresses once, so a
// build without precompressed files still serves compressed bytes.
func (h *staticHandler) gzipOf(asset *staticAsset) *staticRepresentation {
	asset.gzipOnce.Do(func() {
		if asset.gzip = h.companionData(asset, ".gz"); asset.gzip != nil {
			return
		}
		var buffer bytes.Buffer
		writer, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
		if err != nil {
			return
		}
		if _, err := writer.Write(asset.identity.data); err != nil || writer.Close() != nil {
			return
		}
		if buffer.Len() < len(asset.identity.data) {
			asset.gzip = newStaticRepresentation(buffer.Bytes())
		}
	})
	return asset.gzip
}

// acceptedStaticCodings returns the quality values the client gave brotli and
// gzip (RFC 9110, 12.5.3): an explicit entry wins over "*", an absent coding
// is not acceptable unless "*" allows it. Zero means "do not use".
func acceptedStaticCodings(header http.Header) (brotli, gzip float64) {
	brotli, gzip = -1, -1
	star := -1.0
	for _, value := range header.Values("Accept-Encoding") {
		for _, part := range strings.Split(value, ",") {
			token, parameters, _ := strings.Cut(strings.TrimSpace(part), ";")
			quality := 1.0
			for _, parameter := range strings.Split(parameters, ";") {
				if key, number, ok := strings.Cut(strings.TrimSpace(parameter), "="); ok && strings.EqualFold(strings.TrimSpace(key), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
					if err != nil || parsed < 0 {
						parsed = 0
					}
					quality = min(parsed, 1)
				}
			}
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "br":
				brotli = quality
			case "gzip", "x-gzip":
				gzip = quality
			case "*":
				star = quality
			}
		}
	}
	if brotli < 0 {
		brotli = max(star, 0)
	}
	if gzip < 0 {
		gzip = max(star, 0)
	}
	return brotli, gzip
}

// represent picks the representation for this request: the best coding the
// client accepts among those that exist, else the identity bytes. A Range
// request always gets identity bytes: offsets then mean the same thing for
// every client, and a coded body is never cut in the middle.
func (h *staticHandler) represent(asset *staticAsset, r *http.Request) (*staticRepresentation, string) {
	if !asset.varies || r.Header.Get("Range") != "" {
		return asset.identity, ""
	}
	brotliQuality, gzipQuality := acceptedStaticCodings(r.Header)
	var best *staticRepresentation
	encoding, bestQuality := "", 0.0
	if brotliQuality > 0 {
		if rep := h.brotliOf(asset); rep != nil {
			best, encoding, bestQuality = rep, "br", brotliQuality
		}
	}
	if gzipQuality > bestQuality {
		if rep := h.gzipOf(asset); rep != nil {
			best, encoding = rep, "gzip"
		}
	}
	if best == nil {
		return asset.identity, ""
	}
	return best, encoding
}

// staticStatusWriter keeps the headers of a successful response off the other
// ones. http.ServeContent answers a failed If-Match (412) without a body and
// without touching headers set for the 200, and removes Cache-Control from the
// errors it writes itself (416): the coded-body headers must go and no-store
// must come back.
type staticStatusWriter struct{ http.ResponseWriter }

func (w staticStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w staticStatusWriter) WriteHeader(status int) {
	if status != http.StatusOK && status != http.StatusPartialContent {
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")
	}
	if status >= http.StatusBadRequest {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// Unknown API routes fall through to this handler; they must never be
	// answered from, or cached like, a file.
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(p, "/index.html") {
		// As http.FileServer does: the entry page lives at the directory URL.
		target := "./"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if strings.HasSuffix(p, "/") && p != "/" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	root := name == "" && h.phone != nil
	if name == "" {
		name = "index.html"
		if root && h.phone(r) {
			name = "mobile.html"
		}
	}
	asset := h.lookup(name)
	if asset == nil {
		http.NotFound(w, r)
		return
	}
	if root && name == "index.html" && h.offered != nil && h.offered() {
		asset = h.offeredDesktopAsset(asset)
	}
	representation, encoding := h.represent(asset, r)
	header := w.Header()
	header.Set("Content-Type", asset.contentType)
	header.Set("Cache-Control", asset.cacheControl)
	header.Set("ETag", representation.etag)
	if asset.varies {
		header.Add("Vary", "Accept-Encoding")
	}
	if root {
		header.Add("Vary", webentry.Vary)
	}
	if encoding != "" {
		// ServeContent leaves Content-Length unset when Content-Encoding is.
		header.Set("Content-Encoding", encoding)
		header.Set("Content-Length", strconv.Itoa(len(representation.data)))
	}
	http.ServeContent(staticStatusWriter{w}, r, asset.name, time.Time{}, bytes.NewReader(representation.data))
}
