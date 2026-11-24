//go:build darwin

package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/localterm"
)

type recordingLaunches struct {
	mu      sync.Mutex
	steps   [][]string
	prompts []bool // whether each step waits on macOS's Automation question
	fail    bool   // every step fails, as when the application refuses
}

func (r *recordingLaunches) Run(_ context.Context, step localterm.Step) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step.Argv)
	r.prompts = append(r.prompts, step.Prompt)
	if r.fail {
		return errors.New("refused")
	}
	return nil
}

// refusedLaunches is what every test starts with: no step can run, so a test
// that forgets its own runner opens no window on the machine it runs on.
type refusedLaunches struct{}

func (refusedLaunches) Run(context.Context, localterm.Step) error {
	return errors.New("this test did not set a runner")
}

// noApplications finds nothing, as a Mac with none of Herdr, Ghostty or Terminal.
func noApplications() []localterm.Installed { return nil }

func refusedHerdrInstall(context.Context) error {
	return errors.New("this test did not set a Herdr installer")
}

func init() {
	localTerminalRunner, localTerminalDetect, localTerminalInstallHerdr = refusedLaunches{}, noApplications, refusedHerdrInstall
}

// installedAt is the application of the table with this id, as if installed
// under /Applications (a path that is never launched: the runner is a fake).
func installedAt(id string) localterm.Installed {
	for _, app := range localterm.Apps() {
		if app.ID == id {
			return localterm.Installed{App: app, Path: "/Applications/" + id + ".app"}
		}
	}
	panic("unknown application " + id)
}

// useLocalTerminal lets a test open windows through runner on a Mac that has
// exactly these applications, and puts the safe defaults back afterwards.
func useLocalTerminal(t *testing.T, runner localterm.Runner, ids ...string) {
	t.Helper()
	oldRunner, oldDetect := localTerminalRunner, localTerminalDetect
	localTerminalRunner = runner
	localTerminalDetect = func() []localterm.Installed {
		var found []localterm.Installed
		for _, id := range ids {
			found = append(found, installedAt(id))
		}
		return found
	}
	t.Cleanup(func() { localTerminalRunner, localTerminalDetect = oldRunner, oldDetect })
}

func TestLocalTerminalSettingsStayOnThisComputer(t *testing.T) {
	desktop, _, full := desktopBeforeWorkload(t)
	settingsFile := filepath.Join(filepath.Dir(full), localterm.SettingsFile)
	// Two switches, no application: there is nothing to choose.
	w := clientCall(desktop, "GET", "/api/client/local-terminal", "", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"settings\":{\"image_upload\":true,\"clipboard_write\":true},\"supported\":true}" {
		t.Fatal(w.Code, w.Body.String())
	}
	body := "{\"image_upload\":false,\"clipboard_write\":true}"
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", body, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"settings\":{\"image_upload\":false,\"clipboard_write\":true},\"supported\":true}" {
		t.Fatal(w.Code, w.Body.String())
	}
	info, err := os.Stat(settingsFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	if settings, _ := localterm.LoadSettings(filepath.Dir(full)); settings.ImageUpload || !settings.ClipboardWrite {
		t.Fatal(settings)
	}
	// A request that still names an application is not one of this build's.
	for _, bad := range []string{"{\"app\":\"ghostty\",\"image_upload\":true,\"clipboard_write\":true}", "{\"app\":\"\"}", "{\"unknown\":1}"} {
		if w := clientCall(desktop, "PUT", "/api/client/local-terminal", bad, ""); w.Code != 400 {
			t.Fatal(bad, w.Code, w.Body.String())
		}
	}
	// A page from elsewhere cannot change it.
	r := httptest.NewRequest("PUT", desktop.origin+"/api/client/local-terminal", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://attacker.example")
	r.AddCookie(&http.Cookie{Name: desktop.cookieName(), Value: desktop.session})
	w = httptest.NewRecorder()
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
}

// A file written by an earlier build holds the application the owner chose
// then. It is ignored without an error (the page and an open work as they do
// without it), and the next save leaves it out of the file.
func TestLocalTerminalIgnoresAnEarlierAppChoice(t *testing.T) {
	desktop, workload, full := desktopBeforeWorkload(t)
	launches := &recordingLaunches{}
	useLocalTerminal(t, launches, "terminal")
	settingsFile := filepath.Join(filepath.Dir(full), localterm.SettingsFile)
	for _, stored := range []string{"iterm2", "kitty", "no-such-app"} {
		if err := os.WriteFile(settingsFile, []byte("{\"app\":\""+stored+"\",\"image_upload\":false,\"clipboard_write\":true}"), 0o600); err != nil {
			t.Fatal(err)
		}
		w := clientCall(desktop, "GET", "/api/client/local-terminal", "", "")
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"settings\":{\"image_upload\":false,\"clipboard_write\":true},\"supported\":true}" {
			t.Fatal(stored, w.Code, w.Body.String())
		}
		if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"image_upload\":false,\"clipboard_write\":false}", ""); w.Code != 200 {
			t.Fatal(stored, w.Code, w.Body.String())
		}
		var saved map[string]any
		data, err := os.ReadFile(settingsFile)
		if err != nil || json.Unmarshal(data, &saved) != nil || len(saved) != 2 || saved["image_upload"] != false || saved["clipboard_write"] != false {
			t.Fatalf("%s: the file after saving: %s %v", stored, data, err)
		}
	}
	// An earlier choice of another application does not decide the open.
	if err := os.WriteFile(settingsFile, []byte("{\"app\":\"kitty\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	every := localTerminalOpenEvery
	localTerminalOpenEvery = 0
	t.Cleanup(func() { localTerminalOpenEvery = every })
	open := "{\"terminal_id\":\"" + startChild(t, workload, "report").id + "\"}"
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, desktop.remote.desktop.generation); w.Code != 200 || len(launches.steps) != 1 || launches.steps[0][0] != "/usr/bin/open" {
		t.Fatal(w.Code, w.Body.String(), launches.steps)
	}
}

// The application that opens is asked for apart from the settings, and is
// Herdr when it is installed, else Ghostty, else the system Terminal, else none.
func TestLocalTerminalAppRoute(t *testing.T) {
	desktop, _, _ := desktopBeforeWorkload(t)
	for _, row := range []struct {
		installed []string
		want      string
	}{
		{[]string{"terminal", "ghostty", "herdr"}, "{\"app\":{\"id\":\"herdr\",\"name\":\"Herdr\",\"automation\":false}}"},
		{[]string{"ghostty", "herdr"}, "{\"app\":{\"id\":\"herdr\",\"name\":\"Herdr\",\"automation\":false}}"},
		{[]string{"herdr"}, "{\"app\":{\"id\":\"herdr\",\"name\":\"Herdr\",\"automation\":false}}"},
		{[]string{"terminal", "ghostty"}, "{\"app\":{\"id\":\"ghostty\",\"name\":\"Ghostty\",\"automation\":true}}"},
		{[]string{"ghostty"}, "{\"app\":{\"id\":\"ghostty\",\"name\":\"Ghostty\",\"automation\":true}}"},
		{[]string{"terminal"}, "{\"app\":{\"id\":\"terminal\",\"name\":\"系统终端\",\"automation\":false}}"},
		{nil, "{\"app\":null}"},
	} {
		useLocalTerminal(t, refusedLaunches{}, row.installed...)
		w := clientCall(desktop, "GET", "/api/client/local-terminal/app", "", "")
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != row.want {
			t.Errorf("%v: %d %s, want %s", row.installed, w.Code, w.Body.String(), row.want)
		}
	}
	// The list of applications of earlier builds is gone.
	if w := clientCall(desktop, "GET", "/api/client/local-terminal/apps", "", ""); w.Code != 404 {
		t.Error("the list of applications is still served:", w.Code)
	}
	// The settings never detect anything.
	detections := 0
	oldDetect := localTerminalDetect
	localTerminalDetect = func() []localterm.Installed { detections++; return nil }
	t.Cleanup(func() { localTerminalDetect = oldDetect })
	clientCall(desktop, "GET", "/api/client/local-terminal", "", "")
	if detections != 0 {
		t.Fatal("the settings route looked for applications")
	}
}

func TestLocalTerminalOpensAWorkingAttach(t *testing.T) {
	every, perMinute := localTerminalOpenEvery, localTerminalOpenPerMinute
	localTerminalOpenEvery, localTerminalOpenPerMinute = 0, 1000 // many opens in a row
	t.Cleanup(func() { localTerminalOpenEvery, localTerminalOpenPerMinute = every, perMinute })
	desktop, workload, full := desktopBeforeWorkload(t)
	launches := &recordingLaunches{}
	useLocalTerminal(t, launches)
	generation := desktop.remote.desktop.generation
	session := startChild(t, workload, "report")
	open := "{\"terminal_id\":\"" + session.id + "\"}"
	// None of Herdr, Ghostty or the system Terminal is here: nothing opens.
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 409 || !strings.Contains(w.Body.String(), "找不到 Herdr、Ghostty 或系统终端") {
		t.Fatal(w.Code, w.Body.String())
	}
	useLocalTerminal(t, launches, "terminal")
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
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"app\":\"系统终端\"}" {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(launches.steps) != 1 {
		t.Fatal(launches.steps)
	}
	step := launches.steps[0]
	if len(step) != 4 || step[0] != "/usr/bin/open" || step[1] != "-a" || step[2] != "/Applications/terminal.app" || launches.prompts[0] {
		t.Fatal(step, launches.prompts)
	}
	script, err := os.ReadFile(launchedScript(t, step))
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

// With Herdr installed it opens the window with no Automation prompt, replacing
// Ghostty; without Herdr, Ghostty opens the window after macOS's Automation
// question; with both gone, the very same request opens the system Terminal.
// Nothing is stored in between: there is no choice to keep.
func TestLocalTerminalOpensInHerdrElseGhosttyElseTerminal(t *testing.T) {
	every := localTerminalOpenEvery
	localTerminalOpenEvery = 0
	t.Cleanup(func() { localTerminalOpenEvery = every })
	desktop, workload, _ := desktopBeforeWorkload(t)
	generation := desktop.remote.desktop.generation
	open := "{\"terminal_id\":\"" + startChild(t, workload, "report").id + "\"}"
	launches := &recordingLaunches{}
	useLocalTerminal(t, launches, "terminal", "ghostty", "herdr")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"app\":\"Herdr\"}" {
		t.Fatal(w.Code, w.Body.String())
	}
	step := launches.steps[0]
	if len(step) != 2 || step[0] != "/bin/sh" || !strings.HasSuffix(step[1], "-herdr.sh") || launches.prompts[0] {
		t.Fatal(step, launches.prompts)
	}
	if argv := scriptCommand(t, step); argv[1] != "attach" {
		t.Fatal(argv)
	}
	useLocalTerminal(t, launches, "terminal", "ghostty")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"app\":\"Ghostty\"}" {
		t.Fatal(w.Code, w.Body.String())
	}
	step = launches.steps[1]
	if len(step) != 3 || step[0] != "/usr/bin/osascript" || !strings.Contains(step[2], "tell application id \"com.mitchellh.ghostty\"") || !launches.prompts[1] {
		t.Fatal(step, launches.prompts)
	}
	if argv := scriptCommand(t, step); argv[1] != "attach" {
		t.Fatal(argv)
	}
	useLocalTerminal(t, launches, "terminal")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{\"app\":\"系统终端\"}" {
		t.Fatal(w.Code, w.Body.String())
	}
	if step := launches.steps[2]; step[0] != "/usr/bin/open" || launches.prompts[2] {
		t.Fatal(step, launches.prompts)
	}
	// A failing Herdr or Ghostty is reported as it is, with its name: nothing
	// falls back to another application behind the owner's back.
	useLocalTerminal(t, &recordingLaunches{fail: true}, "terminal", "ghostty", "herdr")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "无法在 Herdr 中打开：refused") {
		t.Fatal(w.Code, w.Body.String())
	}
	useLocalTerminal(t, &recordingLaunches{fail: true}, "terminal", "ghostty")
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/open", open, generation); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "无法在 Ghostty 中打开：refused") {
		t.Fatal(w.Code, w.Body.String())
	}
}

// launchedScript is the one-shot script a launch step runs: the last word of
// Terminal's open, the command in Ghostty's AppleScript, or the .command
// script referenced inside Herdr's helper script.
func launchedScript(t *testing.T, step []string) string {
	t.Helper()
	re := regexp.MustCompile("/[^\\s\"']*super-cage-[0-9a-f-]+\\.command")
	if path := re.FindString(strings.Join(step, "\n")); path != "" {
		return path
	}
	if len(step) == 2 && step[0] == "/bin/sh" && strings.HasSuffix(step[1], "-herdr.sh") {
		data, err := os.ReadFile(step[1])
		if err != nil {
			t.Fatal(err)
		}
		if path := re.FindString(string(data)); path != "" {
			return path
		}
	}
	t.Fatalf("no script in %q", step)
	return ""
}

// scriptCommand reads the command a launch step's one-shot script runs: the
// line after the script removes itself, in /bin/sh quoting.
func scriptCommand(t *testing.T, step []string) []string {
	t.Helper()
	data, err := os.ReadFile(launchedScript(t, step))
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
	useLocalTerminal(t, launches, "terminal")
	generation := desktop.remote.desktop.generation
	session := startChild(t, workload, "report")
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
	first, second := launchedScript(t, launches.steps[0]), launchedScript(t, launches.steps[1])
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
	// Ghostty is the application macOS asks the owner about.
	useLocalTerminal(t, launches, "ghostty", "terminal")
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

func terminalOpens(t *testing.T) (desktop, workload *Server, launches *recordingLaunches) {
	t.Helper()
	desktop, workload, _ = desktopBeforeWorkload(t)
	launches = &recordingLaunches{}
	useLocalTerminal(t, launches, "terminal")
	return desktop, workload, launches
}

// The settings and the open request are refused while the environment the
// desktop shows is locked. The lock is the workload's: the desktop asks it.
func TestLocalTerminalRefusedWhileLocked(t *testing.T) {
	desktop, workload, launches := terminalOpens(t)
	generation := desktop.remote.desktop.generation
	open := "{\"terminal_id\":\"" + startChild(t, workload, "report").id + "\"}"
	workload.account.lock()
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"clipboard_write\":true}", ""); w.Code != http.StatusLocked {
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
	desktop, workload, launches := terminalOpens(t)
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

// Settings are not changed while the environment shown cannot say whether
// it is locked, as a window is not opened then. With none shown there is
// no lock to keep.
func TestLocalTerminalSettingsWaitForTheLockAnswer(t *testing.T) {
	desktop, _, _ := terminalOpens(t)
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"image_upload\":true,\"clipboard_write\":false}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	gateway := desktop.remote.desktop
	gateway.active.cancel() // the environment no longer answers
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"clipboard_write\":true}", ""); w.Code != http.StatusConflict {
		t.Fatal("settings changed without the lock answer:", w.Code, w.Body.String())
	}
	if settings, _ := localterm.LoadSettings(desktop.localTerminalDir()); settings.ClipboardWrite {
		t.Fatal("written anyway")
	}
	gateway.mu.Lock()
	gateway.active = nil
	gateway.mu.Unlock()
	if w := clientCall(desktop, "PUT", "/api/client/local-terminal", "{\"clipboard_write\":true}", ""); w.Code != 200 {
		t.Fatal("refused with no environment shown:", w.Code, w.Body.String())
	}
	if settings, _ := localterm.LoadSettings(desktop.localTerminalDir()); !settings.ClipboardWrite {
		t.Fatal("not written with no environment shown")
	}
}

// Optional Herdr desktop installation is locked-guarded, single-flighted, and
// immediately re-detects apps so Herdr takes over from Ghostty.
func TestLocalTerminalInstallHerdr(t *testing.T) {
	desktop, workload, _ := desktopBeforeWorkload(t)

	var mu sync.Mutex
	installedIDs := []string{"ghostty", "terminal"}
	installCalls := 0
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var installErr error

	oldDetect, oldInstall := localTerminalDetect, localTerminalInstallHerdr
	localTerminalDetect = func() []localterm.Installed {
		mu.Lock()
		defer mu.Unlock()
		found := make([]localterm.Installed, 0, len(installedIDs))
		for _, id := range installedIDs {
			found = append(found, installedAt(id))
		}
		return found
	}
	localTerminalInstallHerdr = func(context.Context) error {
		mu.Lock()
		installCalls++
		err := installErr
		mu.Unlock()
		entered <- struct{}{}
		<-release
		if err != nil {
			return err
		}
		mu.Lock()
		installedIDs = []string{"herdr", "ghostty", "terminal"}
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		localTerminalDetect, localTerminalInstallHerdr = oldDetect, oldInstall
	})

	// Refused while locked.
	workload.account.lock()
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", ""); w.Code != http.StatusLocked {
		t.Fatal("installed while locked:", w.Code, w.Body.String())
	}
	if installCalls != 0 {
		t.Fatal("installer ran while locked")
	}
	workload.account.setKey(make([]byte, 32))

	// Refused while another install is already in progress.
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", "")
	}()
	<-entered
	second := clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", "")
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "正在安装 Herdr") {
		close(release)
		t.Fatal("concurrent install was not refused:", second.Code, second.Body.String())
	}
	close(release)
	w := <-first
	want := `{"app":{"id":"herdr","name":"Herdr","automation":false},"installed":true}`
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("install response = %d %s, want %s", w.Code, w.Body.String(), want)
	}

	// Failure reports 502 and clears the single-flight guard.
	release = make(chan struct{})
	close(release)
	mu.Lock()
	installErr = errors.New("network unreachable")
	mu.Unlock()
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", ""); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "network unreachable") {
		t.Fatal("failed install:", w.Code, w.Body.String())
	}
	mu.Lock()
	installErr = nil
	mu.Unlock()
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", ""); w.Code != http.StatusOK {
		t.Fatal("retry after failed install:", w.Code, w.Body.String())
	}

	// Refused when the environment cannot answer the lock check.
	desktop.remote.desktop.active.cancel()
	if w := clientCall(desktop, "POST", "/api/client/local-terminal/install-herdr", "{}", ""); w.Code != http.StatusConflict {
		t.Fatal("installed without lock answer:", w.Code, w.Body.String())
	}

	// Non-desktop workload does not expose the route.
	workloadOnly := testServer(t)
	workloadOnly.remote = newRemoteGateway(workloadOnly.profile)
	t.Cleanup(workloadOnly.remote.close)
	if w := clientCall(workloadOnly, "POST", "/api/client/local-terminal/install-herdr", "{}", ""); w.Code != http.StatusNotFound {
		t.Fatal("workload answered install-herdr:", w.Code, w.Body.String())
	}
}
