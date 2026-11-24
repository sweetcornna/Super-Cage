package workbench

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// The tools' own settings pages, opened from Super Cage ("官方设置"). Each
// entry is a fixed page name sent by the client; the target is always built
// here from the tool's configured address, never taken from the request.
//
//   - T3 Code: its web app's /settings route (upstream
//     apps/web/src/routes/_chat.settings.tsx). Pairing (/pair) always lands
//     on "/", so a fresh tab is moved on once pairing has finished.
//   - Magpie: its window reads ?view=settings&tab= on load (upstream
//     internal/gui/assets/start.js and app.js SET_TABS), and the one-time
//     ?k= sign-in keeps the other parameters when it redirects (web.go).
const officialSettingsPage = "settings"

// t3CodeSettingsWait bounds how long a fresh T3 Code tab may take to pair
// before it is left on T3 Code's home page; tests shorten it.
var t3CodeSettingsWait = 15 * time.Second

// validOfficialPage accepts no page (the tool's home) or its settings page.
func validOfficialPage(page string) bool {
	return page == "" || page == officialSettingsPage
}

// t3CodeSettingsURL is T3 Code's settings page under base, which may carry
// a path prefix (an external instance behind a reverse proxy).
func t3CodeSettingsURL(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.ResolveReference(&url.URL{Path: "settings"}).String()
}

// magpieSettingsURL is raw (Magpie's home or its one-time sign-in link)
// opening on Settings › General instead.
func magpieSettingsURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	q := u.Query()
	q.Set("view", "settings")
	q.Set("tab", "general")
	u.RawQuery = q.Encode()
	return u.String()
}

// navigateBrowserTab moves one Cage browser tab to target.
func (s *Server) navigateBrowserTab(ctx context.Context, id, target string) error {
	_, err := s.browserAction(ctx, "navigate", &browserInput{ID: id, URL: target})
	return err
}

// t3CodePaired reports whether a tab showing rawURL has left T3 Code's
// pairing page on origin.
func t3CodePaired(rawURL, origin string) bool {
	if t3CodeOriginOf(rawURL) != origin {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := strings.TrimSuffix(u.Path, "/")
	return !strings.HasSuffix(path, "/pair")
}

// openT3CodeSettingsAfterPairing waits for a fresh T3 Code tab to finish
// pairing, then moves it to Settings. It reports false when the tab closed,
// pairing took longer than t3CodeSettingsWait, or the move failed: the tab
// then stays where T3 Code put it.
func (s *Server) openT3CodeSettingsAfterPairing(ctx context.Context, tabID, base string) bool {
	target, origin := t3CodeSettingsURL(base), t3CodeOriginOf(base)
	if target == "" || origin == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, t3CodeSettingsWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		current, open := s.browserTabURL(ctx, tabID)
		if !open {
			return false
		}
		if t3CodePaired(current, origin) {
			return s.navigateBrowserTab(ctx, tabID, target) == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// browserTabURL is the address one Cage browser tab shows; open is false
// once the tab is gone. A failed read counts as open, so a slow browser does
// not end the wait early.
func (s *Server) browserTabURL(ctx context.Context, id string) (current string, open bool) {
	data, err := s.browserAction(ctx, "tabs", nil)
	if err != nil {
		return "", true
	}
	var state struct {
		Tabs []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"tabs"`
	}
	if json.Unmarshal(data, &state) != nil {
		return "", true
	}
	for _, tab := range state.Tabs {
		if tab.ID == id {
			return tab.URL, true
		}
	}
	return "", false
}
