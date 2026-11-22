package webfront

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestValidPath(t *testing.T) {
	for raw, want := range map[string]bool{
		"/": true, "/api/ws": true, "/assets/app-1.js": true, "/a%20b": true,
		"/api/../x": false, "/api/%2e%2e/x": false, "/a%2fb": false, "/a\\b": false, "/a//b": false, "relative": false, "/a%00": false,
	} {
		u := &url.URL{Path: raw}
		if parsed, err := url.ParseRequestURI(raw); err == nil {
			u = parsed
		}
		if got := ValidPath(u); got != want {
			t.Errorf("%q: %v", raw, got)
		}
	}
}

func TestCookieMatchesAnyOfSeveral(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "s=impostor; s=real; t=x")
	if !CookieMatches(r, "s", "real") || CookieMatches(r, "s", "other") || CookieMatches(r, "t", "real") {
		t.Fatal("cookie match")
	}
	if CookieMatches(httptest.NewRequest("GET", "/", nil), "s", "") {
		t.Fatal("empty value matched")
	}
}

func TestSelectHeaders(t *testing.T) {
	in := http.Header{"Accept": {"a"}, "Cookie": {"c"}, "Authorization": {"x"}}
	out := SelectHeaders(in, "Accept", "Range")
	if len(out) != 1 || out.Get("Accept") != "a" {
		t.Fatal(out)
	}
}

func TestTicketsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	tickets := NewTickets[string](time.Minute, 8, func() time.Time { return now })
	a := tickets.Issue("a")
	if len(a) != 64 {
		t.Fatal(a)
	}
	now = now.Add(time.Minute)
	if _, ok := tickets.Consume(a); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	p := ParsePolicy("default-src 'self';  script-src 'self' 'nonce-a' ; ;frame-ancestors 'none'")
	if p.String() != "default-src 'self'; script-src 'self' 'nonce-a'; frame-ancestors 'none'" {
		t.Fatal(p.String())
	}
	p = p.Set("frame-ancestors", "http://localhost:1").Set("report-uri", "/r").Without("script-src")
	if p.String() != "default-src 'self'; frame-ancestors http://localhost:1; report-uri /r" {
		t.Fatal(p.String())
	}
}
