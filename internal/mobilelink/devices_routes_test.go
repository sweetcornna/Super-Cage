package mobilelink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The host end of a device link answers 404 for every route outside the phone
// surface and never passes it to the workbench: account recovery, settings,
// the device list and its codes, the SSH entry, the joining side.
func TestHostLinkRefusesRoutesOutsideThePhoneSurface(t *testing.T) {
	passed := 0
	handler := operations(func(ctx context.Context, method, path string, body []byte) (*Reply, error) {
		passed++
		return &Reply{Status: http.StatusOK, ContentType: "application/json", Body: []byte("{}")}, nil
	})
	for _, route := range [][2]string{
		{"POST", "/api/account/recovery-generate"}, {"POST", "/api/account/recovery-reset"}, {"POST", "/api/account/unlock"},
		{"GET", "/api/devices"}, {"POST", "/api/devices/invite"}, {"DELETE", "/api/devices/invite"}, {"DELETE", "/api/devices/x"},
		{"POST", "/api/mobile-pair"}, {"DELETE", "/api/mobile-pair"}, {"GET", "/api/settings"}, {"POST", "/api/settings"},
		{"POST", "/api/client/devices/join"}, {"GET", "/api/entry"}, {"POST", "/api/entry/on"}, {"POST", "/api/app-update/policy"},
	} {
		r := httptest.NewRequest(route[0], "http://cage-mobile.invalid"+route[1], strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: HTTP %d", route[0], route[1], w.Code)
		}
	}
	if passed != 0 {
		t.Fatalf("%d refused requests reached the workbench", passed)
	}
	// The same handler does pass a phone route.
	r := httptest.NewRequest("GET", "http://cage-mobile.invalid/api/mobile/cli/projects", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || passed != 1 {
		t.Fatalf("phone route: HTTP %d, passed %d", w.Code, passed)
	}
}
