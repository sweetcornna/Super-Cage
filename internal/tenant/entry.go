package tenant

import (
	"net/http"
	"net/http/httputil"

	"github.com/sylvinus/agent-vm/internal/webentry"
)

// entryHint tells the workspace which document to answer "/" with: the phone
// UI or the desktop workbench (internal/webentry). The workspace never sees the
// browser's cookies or user agent, so the gateway applies the rule and sends
// one of two fixed words in webentry.Header, on that request only. A browser's
// own value of the header is dropped with every header outside requestHeaders.
// The hint cannot reach another tenant: it rides on the requester's own
// request to its own workspace and only picks which static file it gets back.
// A workspace built before the phone web entry ignores it and stays desktop.
func entryHint(proxy *httputil.ReverseProxy, r *http.Request) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Path != "/" || len(r.Header.Values("Upgrade")) != 0 {
		return
	}
	choice := webentry.Choose(r.Header)
	rewrite := proxy.Rewrite
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		rewrite(request)
		request.Out.Header.Set(webentry.Header, choice)
	}
}
