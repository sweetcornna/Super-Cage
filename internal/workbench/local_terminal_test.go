//go:build darwin

package workbench

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

type recordingLaunches struct {
	mu    sync.Mutex
	steps [][]string
}

func (r *recordingLaunches) Run(_ context.Context, step localterm.Step) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step.Argv)
	return nil
}

// desktopBeforeWorkload is the desktop's own listener (a desktop gateway)
// with a workload controller selected as its local environment, wired as in
// production: cage attach --connection reaches the workload through it.
func desktopBeforeWorkload(t *testing.T) (desktop, workload *Server, full string) {
	t.Helper()
	workload = attachController(t, nil)
	jar, _ := cookiejar.New(nil)
	upstream, _ := url.Parse(workload.origin)
	jar.SetCookies(upstream, []*http.Cookie{
		{Name: workload.cookieName(), Value: workload.session, Path: "/", HttpOnly: true},
		{Name: workload.cookieName() + "-desktop", Value: trustedViewProof(workload.token), Path: "/", HttpOnly: true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session, err := remotehost.LocalSession(ctx, workload.origin, remotehost.PublicManifest{Platform: "darwin", InstanceID: randomToken()}, jar)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	desktop = attachController(t, nil)
	desktop.desktopEntry = true
	desktop.remote = newDesktopGateway(desktop.profile, &desktopLocal{profile: workload.profile})
	t.Cleanup(desktop.remote.close)
	gateway := desktop.remote.desktop
	gateway.active = &desktopTarget{id: gateway.generation, mode: "local", ctx: ctx, cancel: cancel, session: session}
	// The desktop's listener lives under the desktop-entry configuration.
	full = filepath.Join(t.TempDir(), "config.json")
	path := desktopConfigPath(full)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(filepath.Join(desktop.profile, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "control.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := openControllerLock(filepath.Dir(path), os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockController(lock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockController(lock); lock.Close() })
	desktop.configPath = path
	desktop.appUpdateConfigPath = full
	return desktop, workload, full
}

func clientCall(s *Server, method, path, body, connection string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	if connection != "" {
		r.Header.Set(connectionHeader, connection)
	}
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func TestLocalTerminalSettingsStayOnThisComputer(t *testing.T) {
	desktop, _, full := desktopBeforeWorkload(t)
	if w := clientCall(desktop, "GET", "/api/client/local-terminal", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "\"image_upload\":true") || !strings.Contains(w.Body.String(), "\"clipboard_write\":true") || !strings.Contains(w.Body.String(), "\"app\":\"\"") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"kitty\",\"image_upload\":false,\"clipboard_write\":true}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(full), localterm.SettingsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	if settings, _ := localterm.LoadSettings(filepath.Dir(full)); settings.App != "kitty" || settings.ImageUpload || !settings.ClipboardWrite {
		t.Fatal(settings)
	}
	for _, body := range []string{"{\"app\":\"no-such\"}", "{\"app\":\"herdr\"}"} {
		if w := clientCall(desktop, "PUT", "/api/client/local-terminal", body, ""); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	// A page from elsewhere cannot change it.
	r := httptest.NewRequest("PUT", desktop.origin+"/api/client/local-terminal", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://attacker.example")
	r.AddCookie(&http.Cookie{Name: desktop.cookieName(), Value: desktop.session})
	w := httptest.NewRecorder()
	desktop.handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin settings change", w.Code)
	}
	// A workload (not the desktop) has no such routes.
	workloadOnly := testServer(t)
	workloadOnly.remote = newRemoteGateway(workloadOnly.profile)
	t.Cleanup(workloadOnly.remote.close)
	if w := request(workloadOnly, "GET", "/api/client/local-terminal", "", "", true); w.Code != 404 {
		t.Fatal("a workload answered the local terminal routes", w.Code)
	}
	// Detection only runs when the list is asked for; Terminal is always here.
	w = clientCall(desktop, "GET", "/api/client/local-terminal/apps", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "{\"id\":\"terminal\",\"name\":\"终端\",\"automation\":false,\"hosts\":true,\"installed\":true}") || !strings.Contains(w.Body.String(), "\"id\":\"ghostty\"") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLocalTerminalOpensAWorkingAttach(t *testing.T) {
	desktop, workload, full := desktopBeforeWorkload(t)
	launches := &recordingLaunches{}
	localTerminalRunner = launches
	localTerminalDetect = func() []localterm.Installed {
		return []localterm.Installed{{App: *localterm.Lookup("kitty"), Path: "/Applications/kitty.app"}}
	}
	t.Cleanup(func() { localTerminalRunner = localterm.ExecRunner{}; localTerminalDetect = localterm.Detect })
	generation := desktop.remote.desktop.generation
	session := startChild(t, workload, "report")
	open := "{\"terminal_id\":\"" + session.id + "\"}"
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 409 || !strings.Contains(w.Body.String(), "还没有选择本机终端应用") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"kitty\",\"image_upload\":true,\"clipboard_write\":true}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, stale := range []string{"", "an-earlier-connection"} {
		if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, stale); w.Code != 409 {
			t.Fatal("opened on a stale connection", stale, w.Code)
		}
	}
	for _, bad := range []string{"{}", "{\"terminal_id\":\"../../x\"}", "{\"project\":\"relative\"}", "{\"session_id\":\"x;rm\"}", "{\"project\":\"/p\",\"kind\":\"vim\"}"} {
		if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", bad, generation); w.Code != 400 {
			t.Fatal(bad, w.Code, w.Body.String())
		}
	}
	if len(launches.steps) != 0 {
		t.Fatal("a refused request launched", launches.steps)
	}
	w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "\"app\":\"kitty\"") {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(launches.steps) != 1 {
		t.Fatal(launches.steps)
	}
	argv := launches.steps[0]
	line := strings.Join(argv, " ")
	executable, _ := os.Executable()
	if argv[0] != "/Applications/kitty.app/Contents/MacOS/kitty" || argv[1] != "--single-instance" || argv[2] != executable || argv[3] != "attach" {
		t.Fatal(argv)
	}
	for _, secret := range []string{desktop.token, desktop.session, workload.token, workload.session} {
		if strings.Contains(line, secret) {
			t.Fatal("the command line holds a secret")
		}
	}
	// The command the window would run works: it reaches the terminal through
	// the desktop's current connection.
	cfg, err := parseAttachArgs(argv[4:])
	if err != nil || cfg.configPath != full || cfg.target.Connection != generation || cfg.terminal != session.id {
		t.Fatal(cfg, err)
	}
	a := startAttach(t, cfg)
	a.waitFor("ready", 1)
	a.typeText("through-the-desktop")
	a.waitFor("got:\"through-the-desktop\"", 1)
	a.cancel()
	if result := a.exit(5 * time.Second); result.code != 0 {
		t.Fatalf("%+v", result)
	}
	// A new terminal in a project, and a resumed conversation.
	for body, want := range map[string]string{
		"{\"project\":\"/work/app\",\"kind\":\"claude\",\"claude_account_id\":\"work\"}": "--new claude --project /work/app --claude-account work",
		"{\"session_id\":\"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0\"}":                      "--resume 0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
	} {
		if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", body, generation); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
		last := strings.Join(launches.steps[len(launches.steps)-1], " ")
		if !strings.HasSuffix(last, "--connection "+generation+" "+want) {
			t.Fatal(last)
		}
	}
}
