package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// herdrGuest stands in for agent-runtime.py herdr-*: it answers herdr-list
// with list and records every call.
type herdrGuest struct {
	mu         sync.Mutex
	calls      []string
	input      []map[string]any
	list       string
	status     string // herdr-status's answer when set
	closeReply string
}

func (g *herdrGuest) hook(_ context.Context, sub string, input any, output any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, sub)
	data, _ := json.Marshal(input)
	var in map[string]any
	_ = json.Unmarshal(data, &in)
	g.input = append(g.input, in)
	switch sub {
	case "herdr-list":
		return json.Unmarshal([]byte(g.list), output)
	case "herdr-status":
		if g.status != "" {
			return json.Unmarshal([]byte(g.status), output)
		}
		return json.Unmarshal([]byte(`{"installed":true,"version":"0.1.22","server_running":true,"terminals_count":2}`), output)
	case "herdr-close":
		if g.closeReply != "" {
			return json.Unmarshal([]byte(g.closeReply), output)
		}
		return json.Unmarshal([]byte(`{"closed":true,"running":true}`), output)
	}
	return nil
}

func TestHerdrCloseDoesNotReattachWhenTheGuestHasNotClosedYet(t *testing.T) {
	s, g := herdrServer(t, true)
	const id = "0123456789abcdef"
	s.rememberHerdrTerminal(id, herdrRecord{Name: herdrTerminalName(id), Title: "已手动关闭", Kind: "shell"})
	g.list = `{"running":true,"terminals":[{"address":"t2","name":"cage-0123456789abcdef"}]}`
	g.closeReply = `{"closed":false,"running":true}`
	if err := s.closeTerminalID(id); err != nil {
		t.Fatal(err)
	}
	record, ok := s.herdrRecord(id)
	if !ok || !record.Closed || len(s.withHerdrTerminals(context.Background(), nil)) != 0 {
		t.Fatal("a manually closed terminal could return from the guest list")
	}
}

func (g *herdrGuest) count(sub string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, call := range g.calls {
		if sub == "" || call == sub {
			n++
		}
	}
	return n
}

func (g *herdrGuest) last(sub string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.calls) - 1; i >= 0; i-- {
		if g.calls[i] == sub {
			return g.input[i]
		}
	}
	return nil
}

// herdrServer is a test server whose environment runs and whose guest is g.
func herdrServer(t *testing.T, enabled bool) (*Server, *herdrGuest) {
	t.Helper()
	s := testServer(t)
	s.ready = true
	t.Cleanup(s.terminals.close)
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if !s.vmRunning() {
		t.Fatal("the running-environment fixture is not seen")
	}
	g := &herdrGuest{list: `{"running":true,"terminals":[]}`}
	s.agentCLIs.runtimeHook = g.hook
	if enabled {
		s.herdr.mu.Lock()
		if err := s.saveHerdrConfigLocked(herdrConfig{Enabled: true, TakeoverTerminals: true, InstalledHint: true}); err != nil {
			t.Fatal(err)
		}
		s.herdr.mu.Unlock()
	}
	return s, g
}

func listedTerminals(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	w := request(s, "GET", "/api/terminals", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/terminals: %d %s", w.Code, w.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, item := range list {
		byID[item["id"].(string)] = item
	}
	return byID
}

// Herdr off, or nobody listing terminals: the guest is never asked.
func TestHerdrMakesNoGuestCallsWhenOffOrUnlisted(t *testing.T) {
	s, g := herdrServer(t, false)
	listedTerminals(t, s)
	if w := request(s, "GET", "/api/herdr", "", "", true); w.Code != http.StatusOK {
		t.Fatalf("GET /api/herdr: %d", w.Code)
	}
	_ = s.terminalsRevision()
	if n := g.count(""); n != 0 {
		t.Fatalf("Herdr off made %d guest calls: %v", n, g.calls)
	}

	s, g = herdrServer(t, true)
	// The status poll reads terminalsRevision: never hc.
	for range 5 {
		_ = s.terminalsRevision()
	}
	if n := g.count(""); n != 0 {
		t.Fatalf("status polls made %d guest calls: %v", n, g.calls)
	}
	// Lists within herdrListTTL share one herdr-list.
	for range 4 {
		listedTerminals(t, s)
	}
	if n := g.count("herdr-list"); n != 1 {
		t.Fatalf("4 lists made %d herdr-list calls, want 1", n)
	}
	s.herdr.listMu.Lock()
	s.herdr.listAt = time.Now().Add(-herdrListTTL - time.Millisecond)
	s.herdr.listMu.Unlock()
	listedTerminals(t, s)
	if n := g.count("herdr-list"); n != 2 {
		t.Fatalf("a list after the TTL made %d herdr-list calls in all, want 2", n)
	}
}

// Ownership comes from the workbench's records, never the guest's labels;
// another machine's terminals and malformed names never pass; guest strings
// lose their control and format characters.
func TestHerdrListOwnershipAndSanitizing(t *testing.T) {
	s, g := herdrServer(t, true)
	own := "0123456789abcdef"
	s.rememberHerdrTerminal(own, herdrRecord{Name: herdrTerminalName(own), Title: "终端 3", Kind: "claude", Workspace: "/project"})
	g.list = `{"running":true,"terminals":[
		{"address":"t1","name":"cage-0123456789abcdef","labels":{"cage":"1"}},
		{"address":"t2","name":"cage-aaaaaaaaaaaaaaaa","title":"","program":"claude","labels":{"cage":"1","cage.title":"forged"}},
		{"address":"box/t3","name":"remote"},
		{"address":"t4","name":"--help","title":"evil\u202etitle\u001b[31m \u0007bell"},
		{"address":"t5; rm -rf","name":"x"}
	]}`
	list := listedTerminals(t, s)
	mine := list[own]
	if mine == nil || mine["herdr_detached"] != true || mine["title"] != "终端 3" || mine["kind"] != "claude" || mine["herdr_imported"] != nil {
		t.Fatalf("Cage's own hc terminal: %+v", mine)
	}
	forged := list[herdrImportedID("t2")]
	if forged == nil || forged["herdr_imported"] != true || forged["kind"] != "shell" || forged["title"] != "cage-aaaaaaaaaaaaaaaa" {
		t.Fatalf("a cage=1 label made a terminal Cage's: %+v", forged)
	}
	evil := list[herdrImportedID("t4")]
	if evil == nil || evil["title"] != "eviltitle[31m bell" {
		t.Fatalf("guest title not cleaned: %+v", evil)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d terminals, want 3 (no machine/t3, no malformed address): %+v", len(list), list)
	}
	// The record learned t1's address; the name at another address is not it.
	g.list = `{"running":true,"terminals":[{"address":"t9","name":"cage-0123456789abcdef"}]}`
	s.invalidateHerdrList()
	list = listedTerminals(t, s)
	if _, ok := list[own]; ok || list[herdrImportedID("t9")]["herdr_imported"] != true {
		t.Fatalf("a reused name kept the record: %+v", list)
	}
	if _, ok := s.herdrRecord(own); ok {
		t.Fatal("the record of a terminal whose name moved was kept")
	}
	// A core that is down keeps the records: it may come back.
	s.rememberHerdrTerminal(own, herdrRecord{Name: herdrTerminalName(own), Title: "终端 3", Kind: "shell"})
	g.list = `{"running":false,"terminals":[]}`
	s.invalidateHerdrList()
	if list = listedTerminals(t, s); len(list) != 0 {
		t.Fatalf("listed with the core down: %+v", list)
	}
	if _, ok := s.herdrRecord(own); !ok {
		t.Fatal("a stopped core dropped Cage's record")
	}
	// A running core without the name drops it.
	g.list = `{"running":true,"terminals":[]}`
	s.invalidateHerdrList()
	listedTerminals(t, s)
	if _, ok := s.herdrRecord(own); ok {
		t.Fatal("a running core without the terminal kept its record")
	}
}

// Closing a Herdr terminal ends it in hc (kill + rm by the runtime): by its
// name for Cage's own, by its address for another client's.
func TestHerdrCloseEndsTheHcTerminal(t *testing.T) {
	s, g := herdrServer(t, true)
	own := "0123456789abcdef"
	s.rememberHerdrTerminal(own, herdrRecord{Name: herdrTerminalName(own), Title: "终端 1", Kind: "shell"})
	g.list = `{"running":true,"terminals":[{"address":"t1","name":"cage-0123456789abcdef"},{"address":"t2","name":"app-shell"}]}`
	listedTerminals(t, s)
	if w := request(s, "DELETE", "/api/terminals/"+own, s.origin, "", true); w.Code != http.StatusOK {
		t.Fatalf("closing Cage's hc terminal: %d %s", w.Code, w.Body.String())
	}
	if in := g.last("herdr-close"); in == nil || in["terminal"] != "cage-0123456789abcdef" {
		t.Fatalf("herdr-close for Cage's terminal: %+v", in)
	}
	if _, ok := s.herdrRecord(own); ok {
		t.Fatal("closing kept the record")
	}
	imported := herdrImportedID("t2")
	if w := request(s, "DELETE", "/api/terminals/"+imported, s.origin, "", true); w.Code != http.StatusOK {
		t.Fatalf("closing another client's hc terminal: %d %s", w.Code, w.Body.String())
	}
	if in := g.last("herdr-close"); in == nil || in["terminal"] != "t2" {
		t.Fatalf("herdr-close for another client's terminal: %+v", in)
	}
	// A plain terminal id that is no Herdr terminal never reaches the guest.
	before := g.count("herdr-close")
	_ = s.closeTerminalID("ffffffffffffffff")
	if g.count("herdr-close") != before {
		t.Fatal("closing an unknown terminal asked hc to end something")
	}
}

// herdrFakePTY is a PTY whose program waits until it is closed.
type herdrFakePTY struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	done   chan struct{}
	once   sync.Once
}

func newHerdrFakePTY() *herdrFakePTY {
	reader, writer := io.Pipe()
	return &herdrFakePTY{reader: reader, writer: writer, done: make(chan struct{})}
}
func (f *herdrFakePTY) Read(p []byte) (int, error)  { return f.reader.Read(p) }
func (f *herdrFakePTY) Write(p []byte) (int, error) { return len(p), nil }
func (f *herdrFakePTY) Resize(uint16, uint16) error { return nil }
func (f *herdrFakePTY) Kill() error                 { return nil }
func (f *herdrFakePTY) Wait() error                 { <-f.done; return nil }
func (f *herdrFakePTY) Close() error {
	f.once.Do(func() { close(f.done) })
	_ = f.writer.Close()
	return f.reader.Close()
}

// Viewers of other clients' hc terminals have their own cap: they never
// block new Cage terminals, and Cage terminals never block them.
func TestHerdrImportedViewersHaveTheirOwnCap(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	start := func(imported bool) error {
		_, err := s.terminals.startSessionAdmitted(imported, exec.Command("true"), "/p", "shell", claudeSelection{}, codexSelection{}, nil,
			func() (terminalPTY, error) { return newHerdrFakePTY(), nil }, nil)
		return err
	}
	for i := range herdrImportedLimit {
		if err := start(true); err != nil {
			t.Fatalf("imported viewer %d: %v", i+1, err)
		}
	}
	if err := start(true); !errors.Is(err, errHerdrTerminalLimit) {
		t.Fatalf("imported viewer past the cap: %v", err)
	}
	for i := range 12 {
		if err := start(false); err != nil {
			t.Fatalf("Cage terminal %d beside 12 imported viewers: %v", i+1, err)
		}
	}
	if err := start(false); !errors.Is(err, errTerminalLimit) {
		t.Fatalf("Cage terminal past the limit: %v", err)
	}
}

// A re-attached viewer session starts its offsets past any an earlier
// session's viewer holds: a stale resume restarts from the kept history.
func TestHerdrViewerOffsetsRestartStaleResumes(t *testing.T) {
	session := newTerminalSession(nil)
	session.written = herdrOffsetBase()
	session.history = []byte("prompt$ ")
	session.written += uint64(len(session.history))
	for _, stale := range []uint64{0, 4096, 1 << 30} {
		if out := session.outputAfter(stale, 1<<20); !out.Reset || string(out.Data) != "prompt$ " {
			t.Fatalf("stale offset %d: reset=%v data=%q", stale, out.Reset, out.Data)
		}
	}
	if base := herdrOffsetBase(); base >= 1<<53 {
		t.Fatalf("offset base %d is past what a JavaScript number holds", base)
	}
}

// An ordinary terminal id that is no Herdr terminal still 404s on its stream,
// and the stream asks the guest nothing when Herdr is off.
func TestHerdrStreamOfUnknownTerminal(t *testing.T) {
	s, g := herdrServer(t, false)
	if w := request(s, "GET", "/api/terminals/0123456789abcdef/stream", "", "", true); w.Code != http.StatusNotFound {
		t.Fatalf("stream of an unknown terminal: %d", w.Code)
	}
	if w := request(s, "GET", "/api/terminals/..%2fx/stream", "", "", true); w.Code != http.StatusNotFound {
		t.Fatalf("stream of a malformed id: %d", w.Code)
	}
	if g.count("") != 0 {
		t.Fatalf("Herdr off: the stream asked the guest %v", g.calls)
	}
	s, g = herdrServer(t, true)
	g.list = `{"running":true,"terminals":[]}`
	if w := request(s, "GET", "/api/terminals/0123456789abcdef/stream", "", "", true); w.Code != http.StatusNotFound {
		t.Fatalf("stream of a terminal hc does not have: %d", w.Code)
	}
}

// Upgrading from the Herdr TUI (0.1.21-0.1.23): its herdr.json says
// installed, but herdr-core is not there until it is installed. The first
// status (the settings card, or any window's first load) clears the hint:
// new terminals stay plain and lists never ask hc.
func TestHerdrUpgradeFromTheTUIClearsTheInstalledHint(t *testing.T) {
	s, g := herdrServer(t, true)
	g.status = `{"installed":false,"legacy_installed":true}`
	if !s.shouldTakeoverWithHerdr() {
		t.Fatal("the old herdr.json should still say installed before any status")
	}
	w := request(s, "GET", "/api/herdr", "", "", true)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["installed"] != false || body["legacy_installed"] != true {
		t.Fatalf("status after the upgrade: %d %s", w.Code, w.Body.String())
	}
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("new terminals would go to a Herdr that is not installed")
	}
	if cfg, _ := s.herdrConfig(); cfg.InstalledHint || !cfg.Enabled || !cfg.TakeoverTerminals {
		t.Fatalf("config after the upgrade: %+v", cfg)
	}
	listedTerminals(t, s)
	if n := g.count("herdr-list"); n != 0 {
		t.Fatalf("lists asked hc %d times without herdr-core", n)
	}
}

// A stream request for a Herdr terminal without a running session (another
// client's, or Cage's own after its viewer ended) starts an attach-mode viewer
// of that hc terminal under the same id; a second request gets the same one.
func TestHerdrStreamStartsAViewerOfTheHcTerminal(t *testing.T) {
	s, g := herdrServer(t, true)
	var started []string
	previous := herdrViewerPTY
	herdrViewerPTY = func(cmd *exec.Cmd, _, _ uint16) (terminalPTY, error) {
		started = append(started, strings.Join(cmd.Args, " "))
		return newHerdrFakePTY(), nil
	}
	t.Cleanup(func() { herdrViewerPTY = previous })
	own := "0123456789abcdef"
	s.rememberHerdrTerminal(own, herdrRecord{Name: herdrTerminalName(own), Title: "终端 3", Kind: "claude"})
	g.list = `{"running":true,"terminals":[{"address":"t1","name":"cage-0123456789abcdef"},{"address":"t2","name":"notes","title":"vim notes"}]}`
	before := herdrOffsetBase()
	check := func(id, target, title string, imported bool) *terminalSession {
		t.Helper()
		session, err := s.openHerdrViewer(context.Background(), id)
		if err != nil || session == nil {
			t.Fatalf("opening %s: %v", id, err)
		}
		session.mu.Lock()
		got := []any{session.id, session.herdrName, session.herdrImported, session.herdrBacked, session.title}
		written := session.written
		session.mu.Unlock()
		if want := []any{id, target, imported, true, title}; !reflect.DeepEqual(got, want) {
			t.Fatalf("viewer of %s: %v, want %v", id, got, want)
		}
		if written < before {
			t.Fatalf("viewer of %s starts its offsets at %d, under %d", id, written, before)
		}
		last := started[len(started)-1]
		if !strings.Contains(last, "CAGE_HERDR_MODE=attach") || !strings.Contains(last, "CAGE_HERDR_TARGET="+target) {
			t.Fatalf("viewer command of %s: %s", id, last)
		}
		return session
	}
	other := check(herdrImportedID("t2"), "t2", "vim notes", true)
	mine := check(own, herdrTerminalName(own), "终端 3", false)
	if again, err := s.openHerdrViewer(context.Background(), own); err != nil || again != mine || len(started) != 2 {
		t.Fatalf("a second stream started another viewer: %v %d", err, len(started))
	}
	if list := listedTerminals(t, s); list[herdrImportedID("t2")]["herdr_detached"] == true || list[own]["herdr_detached"] == true {
		t.Fatalf("viewed terminals still listed as detached: %+v", list)
	}
	_ = other
}

// A herdr.json from Super Cage 0.1.21-0.1.23 (with the TUI's theme) loads.
func TestHerdrOldConfigWithThemeLoads(t *testing.T) {
	s := testServer(t)
	if err := os.WriteFile(filepath.Join(s.profile, "herdr.json"), []byte(`{"enabled":true,"takeover_terminals":false,"theme":"dracula","installed_hint":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.herdrConfig()
	if err != nil || !cfg.Enabled || cfg.TakeoverTerminals || !cfg.InstalledHint {
		t.Fatalf("old herdr.json: %+v %v", cfg, err)
	}
	if data, _ := json.Marshal(cfg); strings.Contains(string(data), "theme") {
		t.Fatalf("theme kept: %s", data)
	}
}

// A paste into a Herdr terminal through the real path: the viewer's stream
// input, the session's PTY, agent-runtime.py herdr-attach (hc run, then
// hc attach) and the program in hc, here one that reads raw bytes. Both a
// short bracketed paste and one past 4 KiB must arrive byte for byte.
//
// It needs a real herdr-core and its own session, never the user's:
// CAGE_HERDR_LIVE_HOME is a HOME whose .local/bin/herdr-core runs one, and
// HC_SESSION a cage-dev-* session (XDG_STATE_HOME, when set, is passed on).
func TestHerdrLivePasteReachesTheProgram(t *testing.T) {
	home, hcSession := os.Getenv("CAGE_HERDR_LIVE_HOME"), os.Getenv("HC_SESSION")
	if home == "" || !strings.HasPrefix(hcSession, "cage-dev-") {
		t.Skip("needs CAGE_HERDR_LIVE_HOME and HC_SESSION=cage-dev-* (a real herdr-core)")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	runtime, err := filepath.Abs(filepath.Join("..", "cage", "agent-runtime.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "paste.bin")
	name := herdrTerminalName(randomToken()[:16])
	env := []string{"HOME=" + home, "HC_SESSION=" + hcSession, "PATH=" + filepath.Join(home, ".local", "bin") + ":/usr/bin:/bin",
		"TERM=xterm-256color", "LANG=en_US.UTF-8"}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		env = append(env, "XDG_STATE_HOME="+state)
	}
	runtimeCmd := func(sub string, input string) {
		cmd := exec.Command(python, "-I", runtime, sub)
		cmd.Env, cmd.Stdin = env, strings.NewReader(input)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("%s: %v %s", sub, err, out)
		}
	}
	t.Cleanup(func() { runtimeCmd("herdr-close", `{"terminal":"`+name+`"}`) })
	// The program: raw input, bracketed paste on, everything up to the end of
	// the second paste into dump.
	program := "import os,sys,tty\n" +
		"tty.setraw(0)\n" +
		"os.write(1, b'\\x1b[?2004hPASTE-READY\\r\\n')\n" +
		"data = b''\n" +
		"while data.count(b'\\x1b[201~') < 2:\n" +
		"    data += os.read(0, 65536)\n" +
		"open(sys.argv[1], 'wb').write(data)\n" +
		"os.write(1, b'PASTE-DONE\\r\\n')\n"
	script := filepath.Join(dir, "program.py")
	if err := os.WriteFile(script, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", runtime, "herdr-attach")
	cmd.Env = append(env, "CAGE_HERDR_MODE=create", "CAGE_HERDR_NAME="+name, "CAGE_HERDR_WORKSPACE="+dir,
		"CAGE_HERDR_PROJECT_LABEL=paste", "CAGE_HERDR_TAB_LABEL=paste", "CAGE_HERDR_KIND=shell", "CAGE_HERDR_SHELL=/bin/sh",
		"CAGE_HERDR_INNER_CMD=exec "+python+" -I "+script+" "+dump)
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	session, err := s.terminals.startSession(cmd, dir, "shell", claudeSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var text string
	for !strings.Contains(text, "PASTE-READY") {
		_, chunk, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("no prompt from the program in hc: %v %q", err, text)
		}
		text += string(chunk)
	}
	short := "\x1b[200~héllo 中文 `x` $HOME\ttab\rline 2\x1b[201~"
	var long strings.Builder
	for i := range 9000 {
		long.WriteByte(byte(32 + i*7%95))
	}
	big := "\x1b[200~" + long.String() + "\x1b[201~"
	for _, paste := range []string{short, big} {
		if err := ws.WriteJSON(map[string]string{"type": "input", "data": paste}); err != nil {
			t.Fatal(err)
		}
	}
	for !strings.Contains(text, "PASTE-DONE") {
		_, chunk, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("the program did not get both pastes: %v", err)
		}
		text += string(chunk)
	}
	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte(short + big); !bytes.Equal(got, want) {
		t.Fatalf("the program got %d bytes, want %d", len(got), len(want))
	}
	t.Logf("%d + %d pasted bytes reached the program in hc unchanged", len(short), len(big))
}

// hc starts a program in the core's environment, and the core may have been
// started by another hc client or a user service with a different one. A
// terminal Cage opens in hc must still see Cage's environment (what the
// plain path's program gets), with hc's own HC_* variables, and no secret
// of it may show in what hc keeps (argv, names, labels). Same requirements
// as TestHerdrLivePasteReachesTheProgram; it uses its own session,
// HC_SESSION + "-env", and starts that core first with a minimal
// environment.
func TestHerdrLiveProgramGetsCagesEnvironment(t *testing.T) {
	home, base := os.Getenv("CAGE_HERDR_LIVE_HOME"), os.Getenv("HC_SESSION")
	if home == "" || !strings.HasPrefix(base, "cage-dev-") {
		t.Skip("needs CAGE_HERDR_LIVE_HOME and HC_SESSION=cage-dev-* (a real herdr-core)")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	runtime, err := filepath.Abs(filepath.Join("..", "cage", "agent-runtime.py"))
	if err != nil {
		t.Fatal(err)
	}
	hcSession := base + "-env"
	hc := filepath.Join(home, ".local", "bin", "herdr-core")
	state := os.Getenv("XDG_STATE_HOME")
	hcEnv := func(extra ...string) []string {
		env := []string{"HC_SESSION=" + hcSession, "PATH=/usr/bin:/bin"}
		if state != "" {
			env = append(env, "XDG_STATE_HOME="+state)
		}
		return append(env, extra...)
	}
	hcRun := func(args ...string) string {
		cmd := exec.Command(hc, args...)
		cmd.Env = hcEnv()
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	hcRun("server", "stop")
	t.Cleanup(func() { hcRun("server", "stop") })
	// The core first, as another client would start it: a minimal environment.
	start := exec.Command(hc, "server", "start", "--json")
	start.Env = hcEnv("LANG=C", "CORE_ONLY=core-side", "CAGE_ENV_PROBE=core-side")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting the core: %v %s", err, out)
	}

	dir := t.TempDir()
	dump := filepath.Join(dir, "env.json")
	name := herdrTerminalName(randomToken()[:16])
	secret := "s3cr3t-" + randomToken()[:12]
	cage := map[string]string{
		"HOME": home, "PATH": filepath.Join(home, ".local", "bin") + ":/usr/local/bin:/usr/bin:/bin",
		"LANG": "zh_CN.UTF-8", "TZ": "Asia/Shanghai", "BROWSER": "/opt/cage/bin/cage-browser", "DISPLAY": ":7",
		"CLAUDE_CONFIG_DIR": filepath.Join(dir, "claude-account-2"), "CAGE_ENV_PROBE": "cage-side", "CAGE_TEST_SECRET": secret,
	}
	env := []string{"HC_SESSION=" + hcSession, "TERM=xterm-256color"}
	if state != "" {
		env = append(env, "XDG_STATE_HOME="+state)
	}
	for key, value := range cage {
		env = append(env, key+"="+value)
	}
	program := filepath.Join(dir, "program.py")
	if err := os.WriteFile(program, []byte("import json,os,sys\n"+
		"open(sys.argv[1], 'w').write(json.dumps(dict(os.environ)))\n"+
		"os.write(1, b'ENV-DONE\\r\\n')\n"+
		"os.read(0, 1)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", runtime, "herdr-attach")
	cmd.Env = append(env, "CAGE_HERDR_MODE=create", "CAGE_HERDR_NAME="+name, "CAGE_HERDR_WORKSPACE="+dir,
		"CAGE_HERDR_PROJECT_LABEL=env", "CAGE_HERDR_TAB_LABEL=Claude 1", "CAGE_HERDR_KIND=claude", "CAGE_HERDR_SHELL=/bin/sh",
		"CAGE_HERDR_INNER_CMD=exec "+python+" -I "+program+" "+dump)
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	session, err := s.terminals.startSession(cmd, dir, "claude", claudeSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var text string
	for !strings.Contains(text, "ENV-DONE") {
		_, chunk, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("the program in hc never ran: %v %q", err, text)
		}
		text += string(chunk)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	var seen map[string]string
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatal(err)
	}
	for key, want := range cage {
		if seen[key] != want {
			t.Errorf("%s: the program saw %q, Cage has %q", key, seen[key], want)
		}
	}
	if _, ok := seen["CORE_ONLY"]; ok {
		t.Error("the core's environment reached the program")
	}
	if !strings.HasPrefix(seen["HC_TERMINAL_ID"], "t") || seen["HC_SESSION"] != hcSession {
		t.Errorf("hc's own variables: HC_TERMINAL_ID=%q HC_SESSION=%q", seen["HC_TERMINAL_ID"], seen["HC_SESSION"])
	}
	for key := range seen {
		if strings.HasPrefix(key, "CAGE_HERDR_") {
			t.Errorf("%s reached the program", key)
		}
	}
	listed := hcRun("ls", "--json") + hcRun("get", "--json", "--", name)
	if !strings.Contains(listed, name) {
		t.Fatalf("hc does not list %s: %s", name, listed)
	}
	if strings.Contains(listed, secret) || strings.Contains(listed, "CAGE_TEST_SECRET") {
		t.Fatalf("hc shows Cage's environment: %s", listed)
	}
	t.Logf("the program in a core started with LANG=C saw Cage's %d variables and hc's HC_TERMINAL_ID=%s", len(cage), seen["HC_TERMINAL_ID"])
}
