package tenant

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The portal's three files are its first screen on a phone, on every visit (gateway pages stay no-store):
// gzip them for browsers that accept it (about 170 kB down to about 45 kB). Compressed copies are kept per
// content, so a changed -ui-dir file is compressed again.
var portalGzip sync.Map // [32]byte (sha256 of the file) -> []byte

func servePortalFile(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Add("Vary", "Accept-Encoding")
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		sum := sha256.Sum256(body)
		packed, ok := portalGzip.Load(sum)
		if !ok {
			var buffer bytes.Buffer
			writer, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
			_, _ = writer.Write(body)
			_ = writer.Close()
			packed, _ = portalGzip.LoadOrStore(sum, buffer.Bytes())
		}
		body = packed.([]byte)
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// acceptsGzip reads an Accept-Encoding list: gzip (or *) without q=0.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		q := strings.ReplaceAll(strings.ToLower(params), " ", "")
		if q == "q=0" || q == "q=0.0" || q == "q=0.00" || q == "q=0.000" {
			return false
		}
		return true
	}
	return false
}
