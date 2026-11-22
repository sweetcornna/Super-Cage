package webentry

import (
	"net/http"
	"testing"
)

const (
	iPhone        = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	iPad          = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	iPadDesktop   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"
	pixel         = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"
	androidTablet = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	firefoxPhone  = "Mozilla/5.0 (Android 14; Mobile; rv:132.0) Gecko/132.0 Firefox/132.0"
	chromeMac     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	windows       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0"
)

func header(agent, mobileHint, cookie string) http.Header {
	h := http.Header{}
	if agent != "" {
		h.Set("User-Agent", agent)
	}
	if mobileHint != "" {
		h.Set("Sec-CH-UA-Mobile", mobileHint)
	}
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

func TestChoose(t *testing.T) {
	for _, test := range []struct {
		name, agent, hint, cookie, want string
	}{
		{"iPhone Safari", iPhone, "", "", Phone},
		{"Android phone Chrome", pixel, "?1", "", Phone},
		{"Android phone without hints", pixel, "", "", Phone},
		{"Firefox on a phone", firefoxPhone, "", "", Phone},
		{"iPad Safari (mobile agent)", iPad, "", "", Desktop},
		{"iPadOS Safari reports a Mac", iPadDesktop, "", "", Desktop},
		{"Android tablet", androidTablet, "?0", "", Desktop},
		{"phone asking for the desktop site", androidTablet, "?0", "", Desktop},
		{"hint wins over agent", pixel, "?0", "", Desktop},
		{"desktop Chrome", chromeMac, "?0", "", Desktop},
		{"Windows Edge", windows, "", "", Desktop},
		{"no agent", "", "", "", Desktop},
		{"explicit desktop on a phone", iPhone, "", "cage-layout=desktop", Desktop},
		{"explicit phone on a desktop", chromeMac, "?0", "other=1; cage-layout=phone", Phone},
		{"page guard on a tablet", iPadDesktop, "", "cage-layout=phone-auto", Phone},
		{"unknown value is ignored", iPhone, "", "cage-layout=tablet", Phone},
		{"unknown value is ignored on desktop", chromeMac, "", "cage-layout=%70hone", Desktop},
		{"cookie split over headers", chromeMac, "", "a=b", Desktop},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Choose(header(test.agent, test.hint, test.cookie)); got != test.want {
				t.Fatalf("Choose = %q, want %q", got, test.want)
			}
		})
	}
	h := header(chromeMac, "", "a=b")
	h.Add("Cookie", "cage-layout=phone")
	if Choose(h) != Phone {
		t.Fatal("a second Cookie header line is still read")
	}
}

func TestFromHint(t *testing.T) {
	for value, want := range map[string]bool{"phone": true, "desktop": true, "": false, "Phone": false, "phone, desktop": false} {
		h := http.Header{}
		if value != "" {
			h.Set(Header, value)
		}
		if _, ok := FromHint(h); ok != want {
			t.Fatalf("FromHint(%q) ok = %v", value, ok)
		}
	}
	h := http.Header{}
	h.Add(Header, "phone")
	h.Add(Header, "desktop")
	if _, ok := FromHint(h); ok {
		t.Fatal("two hints must not be trusted")
	}
}
