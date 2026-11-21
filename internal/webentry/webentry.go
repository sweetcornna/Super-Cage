// Package webentry decides which of the two web documents a browser gets at
// the workbench root: the desktop workbench (index.html) or the phone UI
// (mobile.html). The same rule runs in the workbench, for browsers that reach
// it directly, and in the tenant gateway, which hides the browser's cookies
// and user agent from the workspace and passes on only the result.
package webentry

import (
	"net/http"
	"strings"
)

// Header carries the gateway's decision to the workspace on a request for "/".
// It is set by the gateway, never forwarded from a browser, and only selects
// which static document the requester itself receives.
const Header = "X-Cage-Entry"

// Cookie is the per-device choice, written by the page (see
// ui/public/entry-guard.js and the phone UI's layout switch).
const Cookie = "cage-layout"

// Vary lists the request headers the root document depends on.
const Vary = "Cookie, User-Agent, Sec-CH-UA-Mobile, " + Header

const (
	Desktop = "desktop"
	Phone   = "phone"
)

// Choose returns Phone or Desktop for a browser request.
//
//   - An explicit choice in the cookie wins: "phone" or "desktop" (the switch in
//     either UI), or "phone-auto" (the desktop page found a touch-only device
//     narrower than a tablet, see entry-guard.js; a session cookie).
//   - Otherwise the browser's own claim to be a phone decides. The server cannot
//     see the pointer or the width, so tablets, and phones asking for the desktop
//     site, get the desktop document and its page-side guard.
func Choose(h http.Header) string {
	switch cookie(h) {
	case "phone", "phone-auto":
		return Phone
	case "desktop":
		return Desktop
	}
	if PhoneAgent(h) {
		return Phone
	}
	return Desktop
}

// FromHint reads the gateway's decision; ok is false when the request carries none.
func FromHint(h http.Header) (choice string, ok bool) {
	values := h.Values(Header)
	if len(values) == 1 && (values[0] == Phone || values[0] == Desktop) {
		return values[0], true
	}
	return "", false
}

// PhoneAgent reports whether the browser says it is a phone: the Chromium
// client hint when present, otherwise the user agent. iPads and Android
// tablets do not count; iPadOS Safari reports a Mac by default anyway.
func PhoneAgent(h http.Header) bool {
	if values := h.Values("Sec-CH-UA-Mobile"); len(values) == 1 {
		switch strings.TrimSpace(values[0]) {
		case "?1":
			return true
		case "?0":
			return false
		}
	}
	agent := h.Get("User-Agent")
	switch {
	case strings.Contains(agent, "iPad"):
		return false
	case strings.Contains(agent, "iPhone"), strings.Contains(agent, "iPod"):
		return true
	case strings.Contains(agent, "Android"):
		return strings.Contains(agent, "Mobile")
	}
	return strings.Contains(agent, "Mobi")
}

func cookie(h http.Header) string {
	request := http.Request{Header: http.Header{"Cookie": h.Values("Cookie")}}
	for _, c := range request.Cookies() {
		if c.Name == Cookie {
			switch c.Value {
			case "phone", "phone-auto", "desktop":
				return c.Value
			}
		}
	}
	return ""
}
