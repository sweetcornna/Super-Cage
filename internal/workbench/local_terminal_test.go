//go:build darwin

package workbench

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/localterm"
)

type recordingLaunches struct {
	mu    sync.Mutex
	steps [][]string
	fail  bool // every step fails, as when the application refuses
}

func (r *recordingLaunches) Run(_ context.Context, step localterm.Step) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step.Argv)
	if r.fail {
		return errors.New("refused")
	}
	return nil
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
	every, perMinute := localTerminalOpenEvery, localTerminalOpenPerMinute
	localTerminalOpenEvery, localTerminalOpenPerMinute = 0, 1000 // many opens in a row
	t.Cleanup(func() { localTerminalOpenEvery, localTerminalOpenPerMinute = every, perMinute })
	desktop, workload, full := desktopBeforeWorkload(t)
	launches := &recordingLaunches{}
	localTerminalRunner = launches
	home := t.TempDir()
	localTerminalHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { localTerminalHome = os.UserHomeDir })
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
	step := launches.steps[0]
	if len(step) != 4 || step[0] != "/Applications/kitty.app/Contents/MacOS/kitty" || step[1] != "--single-instance" || step[2] != "/bin/sh" {
		t.Fatal(step)
	}
	script, err := os.ReadFile(step[3])
	if err != nil {
		t.Fatal(err)
	}
	argv := scriptCommand(t, step)
	executable, _ := os.Executable()
	if argv[0] != executable || argv[1] != "attach" {
		t.Fatal(argv)
	}
	for _, secret := range []string{desktop.token, desktop.session, workload.token, workload.session} {
		if strings.Contains(strings.Join(step, " ")+string(script), secret) {
			t.Fatal("the command line holds a secret")
		}
	}
	// The command the window would run works: it reaches the terminal through
	// the desktop's current connection.
	cfg, err := attach.ParseArgs(argv[2:])
	if err != nil || cfg.ConfigPath != full || cfg.Target.Connection != generation || cfg.Terminal != session.id {
		t.Fatal(cfg, err)
	}
	a := startAttach(t, cfg)
	a.waitFor("ready", 1)
	a.typeText("through-the-desktop")
	a.waitFor("got:\"through-the-desktop\"", 1)
	a.cancel()
	if result := a.exit(5 * time.Second); result.Code != 0 {
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
		last := strings.Join(scriptCommand(t, launches.steps[len(launches.steps)-1]), " ")
		if !strings.HasSuffix(last, "--connection "+generation+" "+want) {
			t.Fatal(last)
		}
	}
	// With cage-attach installed beside cage, the window runs it directly:
	// the same arguments, without "attach".
	dir := t.TempDir()
	for _, name := range []string{"cage", "cage-attach"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	desktop.exe = filepath.Join(dir, "cage")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	slim := scriptCommand(t, launches.steps[len(launches.steps)-1])
	if slim[0] != filepath.Join(resolved, "cage-attach") || slim[1] != "--config" || strings.Join(slim[1:], " ") != strings.Join(argv[2:], " ") {
		t.Fatal(slim)
	}
}

// scriptCommand reads the command a launch step's one-shot script runs: the
// line after the script removes itself, in /bin/sh quoting.
func scriptCommand(t *testing.T, step []string) []string {
	t.Helper()
	data, err := os.ReadFile(step[len(step)-1])
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "rm -f -- \"$0\"\n")
	line, _, _ := strings.Cut(rest, "\n")
	if !ok || line == "" {
		t.Fatalf("no command in %q", data)
	}
	var words []string
	var word strings.Builder
	quoted, inWord := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'' && !quoted:
			quoted, inWord = true, true
		case c == '\'' && quoted:
			quoted = false
		case c == '\\' && !quoted && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ' && !quoted:
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// Opening the same terminal twice in quick succession gives each window its
// own script: neither can remove or replace the other's.
func TestLocalTerminalOpensTheSameTerminalTwice(t *testing.T) {
	desktop, workload, _ := desktopBeforeWorkload(t)
	launches := &recordingLaunches{}
	localTerminalRunner = launches
	home := t.TempDir()
	localTerminalHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { localTerminalHome = os.UserHomeDir })
	localTerminalDetect = func() []localterm.Installed {
		return []localterm.Installed{{App: *localterm.Lookup("kitty"), Path: "/Applications/kitty.app"}}
	}
	t.Cleanup(func() { localTerminalRunner = localterm.ExecRunner{}; localTerminalDetect = localterm.Detect })
	generation := desktop.remote.desktop.generation
	session := startChild(t, workload, "report")
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"kitty\"}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	open := "{\"terminal_id\":\"" + session.id + "\"}"
	for range 2 {
		if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		time.Sleep(1100 * time.Millisecond) // past the rate limit, if any
	}
	if len(launches.steps) != 2 {
		t.Fatal(launches.steps)
	}
	first, second := launches.steps[0][len(launches.steps[0])-1], launches.steps[1][len(launches.steps[1])-1]
	if first == second {
		t.Fatal("both windows share one script", first)
	}
	for _, script := range []string{first, second} {
		if _, err := os.Stat(script); err != nil {
			t.Fatal(err)
		}
	}
}

// blockingLaunches holds every step until released, as osascript does while
// macOS shows its Automation prompt.
type blockingLaunches struct {
	entered, release chan struct{}
	mu               sync.Mutex
	steps            int
	cancelled        bool
}

func (b *blockingLaunches) Run(ctx context.Context, step localterm.Step) error {
	b.mu.Lock()
	b.steps++
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.cancelled = ctx.Err() != nil
	b.mu.Unlock()
	return nil
}

func blockingOpen(t *testing.T) (*Server, string, *blockingLaunches, string) {
	t.Helper()
	desktop, workload, _ := desktopBeforeWorkload(t)
	launches := &blockingLaunches{entered: make(chan struct{}, 4), release: make(chan struct{})}
	localTerminalRunner = launches
	home := t.TempDir()
	localTerminalHome = func() (string, error) { return home, nil }
	localTerminalDetect = func() []localterm.Installed {
		return []localterm.Installed{{App: *localterm.Lookup("iterm2"), Path: "/Applications/iTerm.app"}}
	}
	t.Cleanup(func() {
		localTerminalRunner, localTerminalHome, localTerminalDetect = localterm.ExecRunner{}, os.UserHomeDir, localterm.Detect
	})
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"iterm2\"}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	session := startChild(t, workload, "report")
	return desktop, desktop.remote.desktop.generation, launches, "{\"terminal_id\":\"" + session.id + "\"}"
}

// While one window waits for the owner to answer macOS's Automation prompt,
// a second open is refused: after "Allow", two windows would appear.
func TestLocalTerminalOpensOneAtATime(t *testing.T) {
	desktop, generation, launches, open := blockingOpen(t)
	first := make(chan int, 1)
	go func() { first <- clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation).Code }()
	<-launches.entered
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation) }()
	select {
	case w := <-second:
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "正在打开") {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		close(launches.release)
		t.Fatal("a second open waited behind the first instead of being refused")
	}
	close(launches.release)
	if code := <-first; code != 200 {
		t.Fatal(code)
	}
}

// The window opens even if the page that asked stops waiting: a launch is
// not stopped in the middle of macOS's permission prompt.
func TestLocalTerminalOpenOutlivesTheRequest(t *testing.T) {
	desktop, generation, launches, open := blockingOpen(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", desktop.origin+"/api/client/local-terminal/open", strings.NewReader(open)).WithContext(ctx)
	r.Header.Set("Origin", desktop.origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(connectionHeader, generation)
	r.AddCookie(&http.Cookie{Name: desktop.cookieName(), Value: desktop.session})
	done := make(chan struct{})
	go func() { desktop.handler().ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-launches.entered
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(launches.release)
	<-done
	launches.mu.Lock()
	defer launches.mu.Unlock()
	if launches.cancelled {
		t.Fatal("the launch was stopped when the request ended")
	}
}

func kittyOpens(t *testing.T) (desktop, workload *Server, launches *recordingLaunches) {
	t.Helper()
	desktop, workload, _ = desktopBeforeWorkload(t)
	launches = &recordingLaunches{}
	localTerminalRunner = launches
	home := t.TempDir()
	localTerminalHome = func() (string, error) { return home, nil }
	localTerminalDetect = func() []localterm.Installed {
		return []localterm.Installed{{App: *localterm.Lookup("kitty"), Path: "/Applications/kitty.app"}}
	}
	t.Cleanup(func() {
		localTerminalRunner, localTerminalHome, localTerminalDetect = localterm.ExecRunner{}, os.UserHomeDir, localterm.Detect
	})
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"kitty\"}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return desktop, workload, launches
}

// The settings and the open request are refused while the environment the
// desktop shows is locked. The lock is the workload's: the desktop asks it.
func TestLocalTerminalRefusedWhileLocked(t *testing.T) {
	desktop, workload, launches := kittyOpens(t)
	generation := desktop.remote.desktop.generation
	open := "{\"terminal_id\":\"" + startChild(t, workload, "report").id + "\"}"
	workload.account.lock()
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"app\":\"kitty\",\"clipboard_write\":true}", ""); w.Code != http.StatusLocked {
		t.Fatal("settings changed while locked:", w.Code, w.Body.String())
	}
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != http.StatusLocked {
		t.Fatal("opened while locked:", w.Code, w.Body.String())
	}
	if len(launches.steps) != 0 {
		t.Fatal("launched while locked", launches.steps)
	}
	workload.account.setKey(make([]byte, 32))
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 {
		t.Fatal("not opened after unlocking:", w.Code, w.Body.String())
	}
}

// Opening is bounded: once a second, ten times a minute, whether or not
// the launch worked.
func TestLocalTerminalOpenRateLimit(t *testing.T) {
	desktop, workload, launches := kittyOpens(t)
	generation := desktop.remote.desktop.generation
	open := "{\"terminal_id\":\"" + startChild(t, workload, "report").id + "\"}"
	call := func() int {
		return clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation).Code
	}
	if code := call(); code != 200 {
		t.Fatal(code)
	}
	if code := call(); code != http.StatusTooManyRequests {
		t.Fatal("a second open within a second:", code)
	}
	time.Sleep(1100 * time.Millisecond)
	launches.fail = true
	if code := call(); code != http.StatusBadGateway {
		t.Fatal("a failing launch:", code)
	}
	launches.fail = false
	opened := len(launches.steps)
	every := localTerminalOpenEvery
	localTerminalOpenEvery = 20 * time.Millisecond // the minute's bound, faster
	t.Cleanup(func() { localTerminalOpenEvery = every })
	for i := 0; i < 12; i++ {
		time.Sleep(30 * time.Millisecond)
		if code := call(); code == http.StatusTooManyRequests {
			if total := 2 + i; total != 10 {
				t.Fatalf("refused after %d opens in a minute, want 10", total)
			}
			if len(launches.steps) != opened+i {
				t.Fatal("a refused open launched")
			}
			return
		}
	}
	t.Fatal("never refused within a minute")
}
