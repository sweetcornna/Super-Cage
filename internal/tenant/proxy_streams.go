package tenant

import (
	"net/http"
	"strings"
)

// streamRequest reports whether a request will hold its place for as long as
// the browser keeps it: a WebSocket upgrade or a server-sent event stream.
// Both can be told apart before the guest answers.
func streamRequest(r *http.Request) bool {
	if len(r.Header.Values("Upgrade")) != 0 {
		return true
	}
	for _, accept := range r.Header.Values("Accept") {
		for _, kind := range strings.Split(accept, ",") {
			if mediaType, _, _ := strings.Cut(kind, ";"); strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// acquire takes a place in the budget that fits the request. The release
// function frees it; ok is false when that budget is full.
func (w *Workspace) acquire(r *http.Request) (release func(), ok bool) {
	slots := w.slots
	if streamRequest(r) {
		slots = w.streamSlots
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	default:
		return nil, false
	}
}
