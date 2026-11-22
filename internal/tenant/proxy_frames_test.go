package tenant

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

const (
	frameSurface = "0b3c6f0e-8d5a-4c1e-9f2b-7a6d5e4c3b2a"
	badSurface   = "1b3c6f0e-8d5a-4c1e-9f2b-7a6d5e4c3b2a"
	frameMeta    = "eyJpZCI6ImEiLCJmcmFtZV90YWciOiJiIn0"
	frameTag     = "0123456789abcdef0123456789abcdef"
)

// frameBody is a binary frame: every byte value, so nothing may be re-encoded.
var frameBody = func() []byte {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i)
	}
	return data
}()

// frameGuest answers a negotiated phone frame read the way the workbench does.
func frameGuest(id string) http.Handler {
	base := fixtureGuest(id)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mobile/browser/" + frameSurface + "/frame":
			if r.URL.RawQuery != "after="+frameTag+"&wait=12000&fmt=jpeg" || r.Header.Get("Cookie") != guestCookie+"="+fixtureSession {
				http.Error(w, "fixture frame query changed: "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set(mobilelink.FrameMetaHeader, frameMeta)
			w.Header().Set(mobilelink.FrameStreamHeader, "1")
			_, _ = w.Write(frameBody)
		case "/api/mobile/browser/" + badSurface + "/frame":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set(mobilelink.FrameMetaHeader, "not base64url!")
			w.Header().Set(mobilelink.FrameStreamHeader, "2")
			_, _ = w.Write(frameBody)
		default:
			base.ServeHTTP(w, r)
		}
	})
}

func TestTenantForwardsPhoneFrameNegotiation(t *testing.T) {
	if !mobilelink.ValidFrameMeta(frameMeta) {
		t.Fatal("the fixture's frame metadata is not valid")
	}
	fixture := newFixture(t, false)
	fixture.runtime.guestHandler = frameGuest
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	f := &staticFixture{portalFixture: fixture, cookie: fixture.connect(alice, record.ID), id: record.ID}
	response := f.get("GET", "/api/mobile/browser/"+frameSurface+"/frame?after="+frameTag+"&wait=12000&fmt=jpeg", f.cookie, nil)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("frame read: HTTP %d %q %s", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
	if got := response.Header().Get(mobilelink.FrameMetaHeader); got != frameMeta {
		t.Fatalf("%s through the gateway: %q, want %q", mobilelink.FrameMetaHeader, got, frameMeta)
	}
	if got := response.Header().Get(mobilelink.FrameStreamHeader); got != "1" {
		t.Fatalf("%s through the gateway: %q, want 1", mobilelink.FrameStreamHeader, got)
	}
	if !bytes.Equal(response.Body.Bytes(), frameBody) {
		t.Fatalf("the frame body changed: %d bytes", response.Body.Len())
	}
	// Values the phone could not use are dropped, not passed on.
	response = f.get("GET", "/api/mobile/browser/"+badSurface+"/frame?after="+frameTag+"&wait=12000&fmt=jpeg", f.cookie, nil)
	if response.Header().Get(mobilelink.FrameMetaHeader) != "" || response.Header().Get(mobilelink.FrameStreamHeader) != "" {
		t.Fatalf("invalid frame headers passed: %v", response.Header())
	}
}
