package tenant

import (
	"net/http"
	"net/http/httputil"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The workbench says how long a static file may be kept: content-hashed
// assets for a year, the HTML pages until revalidated. The gateway forwards
// that for successful GET and HEAD responses outside /api/; every other
// response, and any workbench that says nothing usable (an older one answers
// no-store), keeps the gateway's "Cache-Control: no-store".
//
// maxAssetAge caps the lifetime a workbench may grant, one year.
const maxAssetAge = 365 * 24 * 60 * 60

var (
	assetETag = regexp.MustCompile(`^(?:W/)?"[\x21\x23-\x7e]{0,126}"$`)
	assetVary = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\s*,\s*[A-Za-z0-9-]+)*$`)
)

// staticRequest reports whether the response to r may carry the workbench's
// cache policy: a read of a file, not an API call and not a stream.
func staticRequest(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && len(r.Header.Values("Upgrade")) == 0 &&
		r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/")
}

// cacheStatic changes how proxy treats a request for a file. The workbench
// serves precompressed copies chosen by the browser's own Accept-Encoding, so
// that header is what it sees, and a client that sent none gets the identity
// bytes (the transport would otherwise ask for gzip itself and decode it, and
// the ETag would belong to a copy the browser never saw). Its cache policy and
// validators reach the browser. reply is the header of the response the
// gateway sends; it already holds no-store.
func cacheStatic(proxy *httputil.ReverseProxy, r *http.Request, reply http.Header) {
	if !staticRequest(r) {
		return
	}
	rewrite, modify := proxy.Rewrite, proxy.ModifyResponse
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		rewrite(request)
		if request.Out.Header.Get("Accept-Encoding") == "" {
			request.Out.Header.Set("Accept-Encoding", "identity")
		}
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		guest := response.Header
		if err := modify(response); err != nil {
			return err
		}
		forwardStaticCaching(response, guest, reply)
		return nil
	}
}

func forwardStaticCaching(response *http.Response, guest, reply http.Header) {
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusNotModified {
		return
	}
	control, ok := assetCacheControl(guest.Values("Cache-Control"))
	if !ok {
		return
	}
	// ReverseProxy adds the headers of the response to the ones already set,
	// so the gateway's no-store has to go: both would be sent, and no-store
	// would still win.
	reply.Del("Cache-Control")
	headers := response.Header
	headers.Set("Cache-Control", control)
	if etag := guest.Get("ETag"); assetETag.MatchString(etag) {
		headers.Set("ETag", etag)
	}
	if modified, err := http.ParseTime(guest.Get("Last-Modified")); err == nil {
		headers.Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	vary := strings.Join(guest.Values("Vary"), ", ")
	if !assetVary.MatchString(vary) {
		vary = ""
	}
	// A coded body must say that it depends on the browser's Accept-Encoding.
	if guest.Get("Content-Encoding") != "" && !slices.ContainsFunc(strings.Split(vary, ","), func(name string) bool { return strings.EqualFold(strings.TrimSpace(name), "Accept-Encoding") }) {
		vary = strings.TrimPrefix(vary+", Accept-Encoding", ", ")
	}
	if vary != "" {
		headers.Set("Vary", vary)
	}
}

// assetCacheControl turns the workbench's Cache-Control into one the gateway
// forwards. Only a small set of directives is understood; anything else, and
// any no-store, means the response stays uncacheable. The browser may keep the
// response but a shared cache may not, so public becomes private: the gateway
// authenticates with a cookie, which a shared cache would not know about.
func assetCacheControl(values []string) (string, bool) {
	var directives []string
	for _, directive := range strings.Split(strings.Join(values, ","), ",") {
		directive = strings.ToLower(strings.TrimSpace(directive))
		switch {
		case directive == "public":
			directive = "private"
		case directive == "private", directive == "immutable", directive == "no-cache", directive == "must-revalidate":
		case strings.HasPrefix(directive, "max-age="):
			digits := strings.TrimPrefix(directive, "max-age=")
			if digits == "" || strings.ContainsFunc(digits, func(char rune) bool { return char < '0' || char > '9' }) {
				return "", false
			}
			seconds, err := strconv.ParseUint(digits, 10, 63)
			if err != nil {
				// Too large for 63 bits: a very long lifetime, capped below.
				seconds = maxAssetAge
			}
			directive = "max-age=" + strconv.FormatUint(min(seconds, maxAssetAge), 10)
		default:
			return "", false
		}
		if !slices.Contains(directives, directive) {
			directives = append(directives, directive)
		}
	}
	return strings.Join(directives, ", "), len(directives) != 0
}
