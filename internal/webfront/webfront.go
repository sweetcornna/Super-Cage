// Package webfront holds the pieces of a locked-down reverse-proxy front
// that serves a guest web app on a separate loopback origin: path and
// header allow-lists, WebSocket upgrade checks, single-use tickets and
// Content-Security-Policy parsing and merging.
//
// The path and header helpers are copies of internal/tenant/proxy.go
// (validPath, selectHeaders, the upgrade checks). They are copied, not
// moved: the tenant gateway keeps its own and is not touched by phase 2 of
// Claude Science. Keep the two in step when either changes.
package webfront

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ValidPath accepts only clean absolute paths without backslashes, encoded
// slashes or control characters (copy of tenant validPath).
func ValidPath(target *url.URL) bool {
	value := target.Path
	escaped := strings.ToLower(target.EscapedPath())
	return strings.HasPrefix(value, "/") && (path.Clean(value) == value || (value != "//" && path.Clean(value)+"/" == value)) &&
		!strings.Contains(value, `\`) && !strings.Contains(escaped, "%2f") && strings.IndexFunc(value, unicode.IsControl) < 0
}

// SelectHeaders copies only the named headers (copy of tenant selectHeaders).
func SelectHeaders(src http.Header, names ...string) http.Header {
	result := make(http.Header)
	for _, name := range names {
		for _, value := range src.Values(name) {
			result.Add(name, value)
		}
	}
	return result
}

// IsUpgrade reports whether the request asks for any protocol upgrade.
func IsUpgrade(r *http.Request) bool { return len(r.Header.Values("Upgrade")) != 0 }

// ValidWebSocketUpgrade is the tenant gateway's rule: GET with exactly one
// Upgrade header, and that header is websocket.
func ValidWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && len(r.Header.Values("Upgrade")) == 1 && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// ValidSwitchingResponse checks a 101 answer to a WebSocket request.
func ValidSwitchingResponse(request *http.Request, response *http.Response) bool {
	return strings.EqualFold(request.Header.Get("Upgrade"), "websocket") && strings.EqualFold(response.Header.Get("Upgrade"), "websocket")
}

// Secret returns n random bytes as hex.
func Secret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Equal compares two secrets in constant time; empty never matches.
func Equal(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// CookieMatches accepts any of several cookies with the same name. A page on
// another port of the same host can set a cookie with that name (cookies are
// not port-scoped) and shadow the real one; a single r.Cookie lookup would
// then see only the impostor.
func CookieMatches(r *http.Request, name, want string) bool {
	matched := false
	for _, c := range r.Cookies() {
		if c.Name == name && Equal(c.Value, want) {
			matched = true
		}
	}
	return matched
}

// CookieValues returns every value sent under name.
func CookieValues(r *http.Request, name string) []string {
	var values []string
	for _, c := range r.Cookies() {
		if c.Name == name {
			values = append(values, c.Value)
		}
	}
	return values
}

// Tickets are single-use entry secrets with a short lifetime, each carrying
// a value (what the ticket was issued for).
type Tickets[T any] struct {
	mu      sync.Mutex
	life    time.Duration
	limit   int
	now     func() time.Time
	entries map[string]ticket[T]
}

type ticket[T any] struct {
	value   T
	expires time.Time
}

// NewTickets keeps at most limit live tickets, each valid for life.
func NewTickets[T any](life time.Duration, limit int, now func() time.Time) *Tickets[T] {
	if now == nil {
		now = time.Now
	}
	return &Tickets[T]{life: life, limit: limit, now: now, entries: make(map[string]ticket[T])}
}

// Issue returns a new 64-hex ticket for value.
func (t *Tickets[T]) Issue(value T) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for key, entry := range t.entries {
		if !now.Before(entry.expires) {
			delete(t.entries, key)
		}
	}
	for len(t.entries) >= t.limit {
		var oldest string
		var at time.Time
		for key, entry := range t.entries {
			if oldest == "" || entry.expires.Before(at) {
				oldest, at = key, entry.expires
			}
		}
		delete(t.entries, oldest)
	}
	token := Secret(32)
	t.entries[token] = ticket[T]{value: value, expires: now.Add(t.life)}
	return token
}

// Consume redeems a ticket once. Expired, unknown or reused tickets fail.
func (t *Tickets[T]) Consume(token string) (T, bool) {
	var zero T
	if len(token) != 64 {
		return zero, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[token]
	if !ok {
		return zero, false
	}
	delete(t.entries, token)
	if !t.now().Before(entry.expires) {
		return zero, false
	}
	return entry.value, true
}

// Clear forgets every ticket.
func (t *Tickets[T]) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.entries)
}
