package workbench

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestScienceDaemonContract checks, against a real Claude Science daemon,
// what the front and the fake daemon assume about it (Q7): the link
// "claude-science url" prints, the nonce exchange that yields operon_auth,
// bearer authentication, the cookies it sets, and the document's CSP. It is
// read-only (GET requests besides the exchange) and runs only on a machine
// with a real guest:
//
//	CAGE_TEST_SCIENCE_DAEMON=127.0.0.1:47800   reaches the daemon (a forward)
//	CAGE_TEST_SCIENCE_LINK='ssh guest claude-science url'
//	CAGE_TEST_SCIENCE_PORT=47800               the daemon's port in the guest
//	CAGE_TEST_SCIENCE_SIGNED_OUT=1             the account is signed out
//
// Run it twice, signed in and signed out (Q11).
//
// Skipped otherwise; everything else about the daemon in these tests rests
// on science_fake_daemon_test.go.
func TestScienceDaemonContract(t *testing.T) {
	address, command := os.Getenv("CAGE_TEST_SCIENCE_DAEMON"), os.Getenv("CAGE_TEST_SCIENCE_LINK")
	if address == "" || command == "" {
		t.Skip("no real Claude Science daemon (CAGE_TEST_SCIENCE_DAEMON, CAGE_TEST_SCIENCE_LINK)")
	}
	port := 47800
	if value, err := strconv.Atoi(os.Getenv("CAGE_TEST_SCIENCE_PORT")); err == nil {
		port = value
	}
	link := func(ctx context.Context) (string, error) {
		output, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
		for _, field := range strings.Fields(string(output)) {
			if strings.HasPrefix(field, "http://") {
				return field, nil
			}
		}
		return "", err
	}
	upstream := newScienceUpstream(tcpDialer{address: address}, port, link)
	t.Cleanup(upstream.close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The link names the daemon's port and a nonce; the exchange sets
	// operon_auth (scienceUpstream.exchange).
	bearer, err := upstream.token(ctx)
	if err != nil || bearer == "" {
		t.Fatalf("exchange: %v", err)
	}
	get := func(path string, withBearer bool) *http.Response {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.origin()+path, nil)
		request.Host = upstream.hostPort()
		if withBearer {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := upstream.transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	// Bearer authentication is what the front uses (design §2.1).
	if r := get("/api/auth/status", true); r.StatusCode != http.StatusOK {
		t.Fatalf("status with the bearer: %d", r.StatusCode)
	}
	if r := get("/api/projects", false); r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
		t.Errorf("an API call without the bearer: %d (the fake answers 401)", r.StatusCode)
	}
	// Round-2 N5: on a 401 the front asks scienceBearerCheckPath with the
	// same bearer. It must answer 200 to a good bearer, signed in or not, and
	// 401 to any other; then a 401 the app gives a signed-out account shows
	// "需要登录" and mints nothing, and only a refused bearer is replaced.
	if r := get(scienceBearerCheckPath, true); r.StatusCode != http.StatusOK {
		t.Errorf("%s answers %d to a good bearer: the front could not tell a signed-out account from a refused credential", scienceBearerCheckPath, r.StatusCode)
	}
	forged, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.origin()+scienceBearerCheckPath, nil)
	forged.Host = upstream.hostPort()
	forged.Header.Set("Authorization", "Bearer not-a-credential")
	if r, err := upstream.transport.RoundTrip(forged); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s answers %d to a refused bearer: the front would never replace one", scienceBearerCheckPath, r.StatusCode)
		}
	}
	signedOut := os.Getenv("CAGE_TEST_SCIENCE_SIGNED_OUT") == "1"
	for _, path := range []string{"/api/projects"} {
		r := get(path, true)
		t.Logf("%s with the bearer (signed out: %v): %d", path, signedOut, r.StatusCode)
		if r.StatusCode == http.StatusUnauthorized && !signedOut {
			t.Errorf("%s answers 401 to a good bearer of a signed-in account", path)
		}
	}
	// The document: HTML, a nonce-based CSP the front merges, the cookies
	// the front holds for the page.
	document := get("/", true)
	body, _ := io.ReadAll(io.LimitReader(document.Body, 1<<20))
	policy := strings.Join(document.Header.Values("Content-Security-Policy"), "; ")
	var cookies []string
	for _, cookie := range document.Cookies() {
		cookies = append(cookies, cookie.Name)
	}
	t.Logf("document: %d %s, %d bytes, CSP %q, cookies %v", document.StatusCode, document.Header.Get("Content-Type"), len(body), policy, cookies)
	if document.StatusCode != http.StatusOK || !strings.HasPrefix(document.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("document: %d %s", document.StatusCode, document.Header.Get("Content-Type"))
	}
	if !strings.Contains(policy, "'nonce-") {
		t.Errorf("the document's CSP has no nonce: %q", policy)
	}
	status := get("/api/auth/status", true)
	for _, cookie := range status.Cookies() {
		t.Logf("status sets cookie %s (path %q)", cookie.Name, cookie.Path)
	}
}
