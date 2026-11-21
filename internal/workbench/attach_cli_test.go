//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
	"github.com/sylvinus/agent-vm/internal/remotehost"
	"github.com/sylvinus/agent-vm/internal/version"
)

// TestAttachChild is the program inside the Cage terminal: it reports what
// it reads, in raw mode, and acts on a few commands.
func TestAttachChild(t *testing.T) {
	mode := os.Getenv("CAGE_ATTACH_CHILD")
	if mode == "" {
		return
	}
	if _, err := term.MakeRaw(0); err != nil {
		os.Exit(3)
	}
	fmt.Print("ready\r\n")
	buffer := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buffer)
		if err != nil {
			os.Exit(0)
		}
		chunk := string(buffer[:n])
		if mode == "echo" {
			if chunk == "\x06" { // Ctrl+F: 4 MB of output at once
				line := strings.Repeat("x", 78) + "\r\n"
				for i := 0; i < (4<<20)/len(line); i++ {
					os.Stdout.WriteString(line)
				}
				os.Stdout.WriteString("flood-done\r\n")
				continue
			}
			os.Stdout.Write(buffer[:n])
			continue
		}
		fmt.Printf("got:%s\r\n", strconv.Quote(chunk))
		switch {
		case strings.Contains(chunk, "?size"):
			cols, rows, _ := term.GetSize(0)
			fmt.Printf("size=%dx%d\r\n", cols, rows)
		case strings.Contains(chunk, "?query"):
			fmt.Print("\x1b[c")
		case strings.Contains(chunk, "?effects"):
			// A query, a clipboard write, a notification and a bell.
			fmt.Print("\x1b[c\x1b]52;c;aGk=\x07\x1b]9;done\x07\x07effects-shown\r\n")
		case strings.Contains(chunk, "?clip-open"):
			// A clipboard write the program never finishes.
			fmt.Print("clip-open-shown\r\n\x1b]52;c;" + strings.Repeat("QUFB", 64))
		case strings.Contains(chunk, "?dropped"):
			// DA2, in-band resize reports and focus reports.
			fmt.Print("\x1b[>c\x1b[?2048h\x1b[?1004hdropped-shown\r\n")
		case strings.Contains(chunk, "?burst"):
			for i := 1; i <= 3000; i++ {
				fmt.Printf("line-%04d\r\n", i)
				if i%200 == 0 {
					time.Sleep(2 * time.Millisecond)
				}
			}
		case strings.Contains(chunk, "exit"):
			os.Exit(0)
		}
	}
}

func attachChildCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachChild$")
	cmd.Env = append(os.Environ(), "CAGE_ATTACH_CHILD="+mode)
	return cmd
}

// attachController is agentTestController with its handler wrapped, so a
// test can watch or replace routes.
func attachController(t *testing.T, wrap func(http.Handler) http.Handler) *Server {
	t.Helper()
	s := testServer(t)
	identity, err := currentController()
	if err != nil {
		t.Fatal(err)
	}
	s.identity = identity
	handler := s.handler()
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	s.origin = "http://localhost:" + u.Port()
	lock, err := openControllerLock(s.profile, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockController(lock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockController(lock); lock.Close() })
	if err := writePrivate(filepath.Join(s.profile, "control.json"), controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: identity}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.terminals.close)
	return s
}

// vmStatus answers /api/status, which a test server cannot know.
func vmStatus(running *bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/status" {
				jsonReply(w, map[string]any{"vm_running": *running})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// attachClient is one cage attach in a pseudo-terminal: the test plays the
// terminal application on the master side.
type attachClient struct {
	t             *testing.T
	master, slave *os.File
	resized       chan struct{}
	cancel        context.CancelFunc
	done          chan struct{} // closed when runAttach returned result
	result        attach.Exit
	raw           chan struct{}
	mu            sync.Mutex
	out           bytes.Buffer
	changed       chan struct{}
}

func startAttach(t *testing.T, cfg attach.Config) *attachClient {
	t.Helper()
	return startAttachWith(t, cfg, attach.Options{Check: agentSameBuild})
}

func startAttachWith(t *testing.T, cfg attach.Config, options attach.Options) *attachClient {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = pty.Setsize(slave, &pty.Winsize{Cols: 100, Rows: 30})
	ctx, cancel := context.WithCancel(context.Background())
	c := &attachClient{t: t, master: master, slave: slave, resized: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), raw: make(chan struct{}, 1), changed: make(chan struct{}, 1)}
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				c.mu.Lock()
				c.out.Write(buffer[:n])
				c.mu.Unlock()
				select {
				case c.changed <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	tty := attach.TTY{In: slave, Out: slave, ErrOut: io.Discard, Resized: c.resized,
		Size: func() (int, int, bool) {
			size, err := pty.GetsizeFull(slave)
			if err != nil {
				return 0, 0, false
			}
			return int(size.Cols), int(size.Rows), true
		},
		MakeRaw: func() (func(), error) {
			state, err := term.MakeRaw(int(slave.Fd()))
			c.raw <- struct{}{}
			return func() { _ = term.Restore(int(slave.Fd()), state) }, err
		},
	}
	go func() { c.result = attach.Run(ctx, cfg, tty, options); close(c.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
		}
		slave.Close()
		master.Close()
	})
	return c
}

func (c *attachClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

// waitFor waits until the application has shown want n times.
func (c *attachClient) waitFor(want string, n int) string {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if out := c.output(); strings.Count(out, want) >= n {
			return out
		}
		select {
		case <-c.changed:
		case <-deadline:
			c.t.Fatalf("never saw %q x%d; saw %q", want, n, c.output())
		}
	}
}

// leftReset checks that the window was left as a shell expects: the title
// pushed before the first output, the reset written last.
func (c *attachClient) leftReset() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := c.output()
		if strings.HasPrefix(out, attach.TitlePush) && strings.HasSuffix(out, attach.ResetSequence) {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the window was not reset: %q", out[max(0, len(out)-120):])
		}
		select {
		case <-c.changed:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *attachClient) typeText(text string) {
	c.t.Helper()
	if _, err := c.master.Write([]byte(text)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *attachClient) exit(within time.Duration) attach.Exit {
	c.t.Helper()
	select {
	case <-c.done:
		return c.result
	case <-time.After(within):
		c.t.Fatalf("attach did not end within %v; output %q", within, c.output())
	}
	return attach.Exit{}
}

func attachTo(s *Server, terminal string) attach.Config {
	return attach.Config{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, Terminal: terminal}
}

func startChild(t *testing.T, s *Server, mode string) *terminalSession {
	t.Helper()
	session, err := s.terminals.start(attachChildCommand(mode), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestAttachTypingResizeDetachAndReattach(t *testing.T) {
	running := true
	s := attachController(t, vmStatus(&running))
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	a.typeText("hello")
	a.waitFor("got:\"hello\"", 1)
	_ = pty.Setsize(a.slave, &pty.Winsize{Cols: 120, Rows: 40})
	a.resized <- struct{}{}
	a.typeText("?size")
	a.waitFor("size=120x40", 1)
	// Closing the window detaches; the terminal keeps running.
	a.cancel()
	if result := a.exit(5 * time.Second); result.Code != 0 || result.Message != "" {
		t.Fatalf("detach: %+v", result)
	}
	a.leftReset()
	if session.summary()["closed"] == true {
		t.Fatal("detaching ended the terminal")
	}
	// A new window shows the kept history once.
	b := startAttach(t, attachTo(s, session.id))
	out := b.waitFor("size=120x40", 1)
	if strings.Count(out, "got:\"hello\"") != 1 || strings.Count(out, "ready") != 1 {
		t.Fatalf("re-attach history: %q", out)
	}
	// As the only viewer, its own size applies.
	b.typeText("?size")
	b.waitFor("size=100x30", 1)
}

func TestAttachResumesWithoutLossOrDuplication(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	var mu sync.Mutex
	var streams []*controlwire.Conn
	attach.StreamHook = func(ws *controlwire.Conn) { mu.Lock(); streams = append(streams, ws); mu.Unlock() }
	t.Cleanup(func() { attach.StreamHook = nil })
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	a.typeText("?burst")
	a.waitFor("line-0400", 1)
	mu.Lock()
	_ = streams[0].NetConn().Close() // the network drops the stream
	mu.Unlock()
	out := a.waitFor("line-3000", 1)
	mu.Lock()
	reconnects := len(streams) - 1
	mu.Unlock()
	if reconnects < 1 {
		t.Fatal("the stream was never resumed")
	}
	for i := 1; i <= 3000; i++ {
		line := fmt.Sprintf("line-%04d\r\n", i)
		if strings.Count(out, line) != 1 {
			t.Fatalf("%q appears %d times after %d reconnects", line, strings.Count(out, line), reconnects)
		}
	}
	if strings.Contains(out, "\x1bc") {
		t.Fatal("a resumed stream cleared the screen")
	}
}

func TestAttachTwoViewersAnswerQueriesOnce(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	b := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	b.waitFor("ready", 1)
	a.typeText("x") // a owns the terminal
	a.waitFor("got:\"x\"", 1)
	a.typeText("?query")
	// Both applications see the query and answer it, like any terminal.
	for _, c := range []*attachClient{a, b} {
		c.waitFor("\x1b[c", 1)
		c.typeText("\x1b[?62;22c")
	}
	time.Sleep(200 * time.Millisecond)
	a.typeText("mark")
	out := a.waitFor("mark\"", 1)
	if n := strings.Count(out, "\\x1b[?62;22c"); n != 1 {
		t.Fatalf("the program read %d answers: %q", n, out)
	}
	// b types: now it owns, and its size is the terminal's.
	_ = pty.Setsize(b.slave, &pty.Winsize{Cols: 90, Rows: 20})
	b.resized <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	b.typeText("?size")
	b.waitFor("size=90x20", 1)
}

func TestAttachReplayActsOnce(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	a.typeText("?effects")
	live := a.waitFor("effects-shown", 1)
	// Live: the query reaches the application, the clipboard write and the
	// notification too (clipboard writes are on by default, as on the web).
	for _, effect := range []string{"\x1b[c", "\x1b]52;c;aGk=\x07", "\x1b]9;done\x07"} {
		if !strings.Contains(live, effect) {
			t.Fatalf("live output lost %q: %q", effect, live)
		}
	}
	a.cancel()
	a.exit(5 * time.Second)
	session.mu.Lock()
	before := session.written
	session.mu.Unlock()
	// Replayed: nothing acts again, so a real application answers nothing.
	b := startAttach(t, attachTo(s, session.id))
	replayed := b.waitFor("effects-shown", 1)
	for _, effect := range []string{"\x1b[c", "\x1b]52", "\x1b]9;", "\x07"} {
		if strings.Contains(replayed, effect) {
			t.Fatalf("replay repeated %q: %q", effect, replayed)
		}
	}
	time.Sleep(300 * time.Millisecond)
	session.mu.Lock()
	after := session.written
	session.mu.Unlock()
	if after != before {
		t.Fatalf("re-attaching sent input: the program wrote %d more bytes", after-before)
	}
}

func TestAttachFilterStats(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	var stats string
	a := startAttachWith(t, attachTo(s, session.id), attach.Options{Check: agentSameBuild, FilterReport: func(report string) { stats = report }})
	a.waitFor("ready", 1)
	a.typeText("?dropped")
	out := a.waitFor("dropped-shown", 1)
	for _, seq := range []string{"\x1b[>c", "\x1b[?2048h", "\x1b[?1004h"} {
		if strings.Contains(out, seq) {
			t.Fatalf("passed %q: %q", seq, out)
		}
	}
	a.cancel()
	a.exit(5 * time.Second)
	if !strings.Contains(stats, `"CSI >c"`) || !strings.Contains(stats, `"CSI ?2048h"`) || strings.Contains(stats, "1004") {
		t.Fatalf("stats %q", stats)
	}
}

func TestAttachObserverCannotType(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	cfg := attachTo(s, session.id)
	cfg.Observe = true
	a := startAttach(t, cfg)
	a.waitFor("ready", 1)
	a.typeText("rm -rf /\r")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(a.output(), "got:") {
		t.Fatal("an observer typed")
	}
	a.typeText("\x03")
	if result := a.exit(5 * time.Second); result.Code != 0 {
		t.Fatalf("%+v", result)
	}
	a.leftReset()
}

func TestAttachEndsWithAPlainMessage(t *testing.T) {
	running := true
	s := attachController(t, vmStatus(&running))
	// The program exits.
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	a.typeText("exit")
	if result := a.exit(5 * time.Second); result.Code != 0 || result.Message != "终端已结束。" {
		t.Fatalf("%+v", result)
	}
	a.leftReset()
	// The environment stops.
	session = startChild(t, s, "report")
	b := startAttach(t, attachTo(s, session.id))
	b.waitFor("ready", 1)
	running = false
	s.terminals.closeAll()
	if result := b.exit(5 * time.Second); result.Code != 0 || result.Message != "环境已停止，终端已随之结束。" {
		t.Fatalf("%+v", result)
	}
	b.leftReset()
	// The account locks: the attach ends at once and says why.
	running = true
	session = startChild(t, s, "report")
	c := startAttach(t, attachTo(s, session.id))
	c.waitFor("ready", 1)
	locked := time.Now()
	s.account.lock()
	result := c.exit(5 * time.Second)
	if result.Code != 3 || !strings.Contains(result.Message, "已锁定") {
		t.Fatalf("%+v", result)
	}
	c.leftReset()
	t.Logf("lock to attach end: %v", time.Since(locked))
	if time.Since(locked) > 2*time.Second {
		t.Fatal("too slow")
	}
}

// --new and --resume ask the workload whether it applies the viewer rules
// before creating anything: an old server gets no terminal it cannot show.
func TestAttachChecksTheServerBeforeCreating(t *testing.T) {
	for _, old := range []string{"not found", "page"} {
		var mu sync.Mutex
		posts := 0
		s := attachController(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && (r.URL.Path == "/api/terminals" || strings.HasPrefix(r.URL.Path, "/api/cli/sessions/")) {
					mu.Lock()
					posts++
					mu.Unlock()
				}
				if r.URL.Path == "/api/terminals/viewers" {
					// What a server from before the viewer rules answers.
					if old == "page" {
						w.Header().Set("Content-Type", "text/html")
						_, _ = io.WriteString(w, "<!doctype html><title>Super Cage</title>")
					} else {
						http.NotFound(w, r)
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/stream") {
					query := r.URL.Query()
					query.Del("viewer")
					r.URL.RawQuery = query.Encode()
				}
				next.ServeHTTP(w, r)
			})
		})
		before := len(s.terminals.snapshot())
		for _, cfg := range []attach.Config{
			{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, NewKind: "shell"},
			{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, Resume: "session-1"},
		} {
			a := startAttach(t, cfg)
			if result := a.exit(5 * time.Second); result.Code != 1 || !strings.HasPrefix(result.Message, "需要更新 Super Cage") {
				t.Fatalf("%s: %+v", old, result)
			}
		}
		mu.Lock()
		if posts != 0 || len(s.terminals.snapshot()) != before {
			t.Fatalf("%s: %d requests that create, %d terminals before, %d after", old, posts, before, len(s.terminals.snapshot()))
		}
		mu.Unlock()
	}
	// A current server answers the question itself.
	s := attachController(t, nil)
	w := request(s, "GET", "/api/terminals/viewers", s.origin, "", true)
	var answer struct {
		Protocol int `json:"protocol"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Protocol != attach.ViewerProtocol {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAttachRefusesAServerWithoutViewers(t *testing.T) {
	// A server from before this change ignores the viewer identity.
	s := attachController(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/stream") {
				query := r.URL.Query()
				query.Del("viewer")
				query.Del("observe")
				r.URL.RawQuery = query.Encode()
			}
			next.ServeHTTP(w, r)
		})
	})
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	result := a.exit(5 * time.Second)
	if result.Code != 1 || !strings.HasPrefix(result.Message, "需要更新 Super Cage") {
		t.Fatalf("%+v", result)
	}
	select {
	case <-a.raw:
		t.Fatal("the window was switched to raw mode before the server was checked")
	default:
	}
}

func TestAttachCreatesWithTheDesktopsRequest(t *testing.T) {
	var mu sync.Mutex
	var created []map[string]any
	s := attachController(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && r.URL.Path == "/api/terminals" {
				var body map[string]any
				data, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(data, &body)
				mu.Lock()
				created = append(created, body)
				mu.Unlock()
				r.Body = io.NopCloser(bytes.NewReader(data))
			}
			next.ServeHTTP(w, r)
		})
	})
	cfg := attach.Config{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, NewKind: "claude", Project: "/project", ClaudeAccount: "work"}
	a := startAttach(t, cfg)
	result := a.exit(10 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	// The test server has no environment, so the server refuses; the request
	// is the desktop's own, and the refusal is the server's own sentence.
	if len(created) != 1 || created[0]["kind"] != "claude" || created[0]["workspace"] != "/project" || created[0]["claude_account_id"] != "work" {
		t.Fatalf("%v", created)
	}
	if result.Code != 1 || result.Message != errTerminalNotReady.Error() {
		t.Fatalf("%+v", result)
	}
}

func writeTestImage(t *testing.T, path, format string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if format == "png" {
		err = png.Encode(f, img)
	} else {
		err = jpeg.Encode(f, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAttachUploadsDroppedImagesOnly(t *testing.T) {
	var mu sync.Mutex
	var uploads [][]byte
	s := attachController(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/input/paste/start":
				jsonReply(w, map[string]any{"available": true, "context": strings.Repeat("c", 32)})
			case "/api/input/image":
				var body struct {
					PNG    string `json:"png"`
					Target string `json:"target"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				data, _ := base64.StdEncoding.DecodeString(body.PNG)
				mu.Lock()
				uploads = append(uploads, data)
				n := len(uploads)
				mu.Unlock()
				jsonReply(w, map[string]any{"path": fmt.Sprintf("/home/cage/.local/share/cage/attachments/image-%d.png", n)})
			default:
				next.ServeHTTP(w, r)
			}
		})
	})
	dir := t.TempDir()
	picture := filepath.Join(dir, "My Picture.png")
	photo := filepath.Join(dir, "photo.jpg")
	key := filepath.Join(dir, "id_ed25519")
	writeTestImage(t, picture, "png")
	writeTestImage(t, photo, "jpeg")
	if err := os.WriteFile(key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	paste := func(text string) { a.typeText("\x1b[200~" + text + "\x1b[201~") }
	// Two images dropped, quoted the way applications quote them.
	paste(strings.ReplaceAll(picture, " ", "\\ ") + " '" + photo + "' ")
	a.waitFor("image-2.png", 1)
	out := a.output()
	if !strings.Contains(out, strconv.Quote("\x1b[200~'/home/cage/.local/share/cage/attachments/image-1.png' '/home/cage/.local/share/cage/attachments/image-2.png' \x1b[201~")) || strings.Contains(out, dir) {
		t.Fatalf("paste not replaced: %q", out)
	}
	mu.Lock()
	for _, data := range uploads {
		if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "png" {
			t.Fatal("upload is not a PNG", format, err)
		}
	}
	mu.Unlock()
	// A key file's path, a path among words, and a typed path: pasted as is.
	for _, text := range []string{key, picture + " and more", "file://" + picture + "?x"} {
		paste(text)
		a.waitFor(strconv.Quote("\x1b[200~"+text+"\x1b[201~"), 1)
	}
	a.typeText(picture)
	a.waitFor(strconv.Quote(picture), 1)
	mu.Lock()
	if len(uploads) != 2 {
		t.Fatalf("%d uploads", len(uploads))
	}
	mu.Unlock()
	// The per-computer switch turns even images off, from the next paste on:
	// it is read for each paste.
	settings := filepath.Join(filepath.Dir(s.configPath), "local-terminal.json")
	if err := os.WriteFile(settings, []byte("{\"image_upload\":false}"), 0o600); err != nil {
		t.Fatal(err)
	}
	paste(photo)
	a.waitFor(strconv.Quote("\x1b[200~"+photo+"\x1b[201~"), 1)
	mu.Lock()
	if len(uploads) != 2 {
		t.Fatal("uploaded with the switch off")
	}
	mu.Unlock()
	if err := os.WriteFile(settings, []byte("{\"image_upload\":true}"), 0o600); err != nil {
		t.Fatal(err)
	}
	paste(photo + " ")
	a.waitFor("image-3.png", 1)
}

func TestAttachDroppedPathParsing(t *testing.T) {
	for _, row := range []struct {
		in   string
		want []string
	}{
		{"/a/b.png", []string{"/a/b.png"}},
		{"/a/My\\ File.png /c/d.jpg ", []string{"/a/My File.png", "/c/d.jpg"}},
		{"'/a/it'\\''s.png'", []string{"/a/it's.png"}},
		// Typed or copied forms no application uses for a drop never upload.
		{"\"/a/x \\\"y\\\".png\"", nil},
		{"file:///a/b%20c.png", nil},
		{"~/Desktop/a.png", nil},
		{"/a/b.png\n/c.png", []string{"/a/b.png", "/c.png"}},
		{"relative.png", nil},
		// One unescaped path, as kitty and Alacritty insert it: the file
		// decides, and none is named that.
		{"/a/b.png and", []string{"/a/b.png and"}},
		{"", nil},
		{"file://host/a.png", nil},
		{"'/unterminated", nil},
	} {
		got, ok := attach.DroppedPaths(row.in)
		if ok != (row.want != nil) || strings.Join(got, "|") != strings.Join(row.want, "|") {
			t.Errorf("%q: %q %v", row.in, got, ok)
		}
	}
}

// TestAttachLatency measures what the attach hop adds per keystroke, with the
// method of design 5.13: 150 keys 30 ms apart through an echoing program,
// once over a direct stream (what the web terminal does) and once through
// cage attach in a pseudo-terminal. CAGE_ATTACH_BENCH=1 runs it.
func TestAttachLatency(t *testing.T) {
	if os.Getenv("CAGE_ATTACH_BENCH") == "" {
		t.Skip("set CAGE_ATTACH_BENCH=1")
	}
	s := attachController(t, nil)
	session := startChild(t, s, "echo")
	// Direct: a viewer on the stream itself.
	c, err := agentConnectTarget(context.Background(), s.configPath, AgentTarget{Scope: "local"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := agentDialSocket(context.Background(), c, AgentTarget{Scope: "local"}, "/api/terminals/"+session.id+"/stream?after=0&viewer=direct-bench-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	frames := make(chan []byte, 64)
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			if kind == websocket.BinaryMessage {
				frames <- data
			}
		}
	}()
	waitFrame := func(want string) {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case data := <-frames:
				if strings.Contains(string(data), want) {
					return
				}
			case <-deadline:
				t.Fatal("no echo")
			}
		}
	}
	waitFrame("ready")
	direct := make([]time.Duration, 0, 150)
	for i := 0; i < 150; i++ {
		key := string(rune('a' + i%26))
		start := time.Now()
		data, _ := json.Marshal(map[string]string{"type": "input", "data": key})
		if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatal(err)
		}
		waitFrame(key)
		direct = append(direct, time.Since(start))
		time.Sleep(30 * time.Millisecond)
	}
	ws.Close()
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	time.Sleep(200 * time.Millisecond)
	through := make([]time.Duration, 0, 150)
	for i := 0; i < 150; i++ {
		key := string(rune('A' + i%26))
		before := strings.Count(a.output(), key)
		start := time.Now()
		a.typeText(key)
		a.waitFor(key, before+1)
		through = append(through, time.Since(start))
		time.Sleep(30 * time.Millisecond)
	}
	stats := func(d []time.Duration) (time.Duration, time.Duration, time.Duration) {
		sorted := append([]time.Duration(nil), d...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[len(sorted)/2], sorted[len(sorted)*95/100], sorted[len(sorted)-1]
	}
	dm, dp, dx := stats(direct)
	am, ap, ax := stats(through)
	t.Logf("direct stream: median %v p95 %v max %v", dm, dp, dx)
	t.Logf("through cage attach: median %v p95 %v max %v", am, ap, ax)
	t.Logf("added by the attach hop: median %v p95 %v", am-dm, ap-dp)
}

// TestAttachClientProcess is cage attach as its own process, for the
// footprint measurement: CAGE_ATTACH_CLIENT holds its arguments.
func TestAttachClientProcess(t *testing.T) {
	args := os.Getenv("CAGE_ATTACH_CLIENT")
	if args == "" {
		return
	}
	os.Exit(attach.Main(strings.Fields(args), os.Stdin, os.Stdout, os.Stderr, attach.Options{Check: agentSameBuild}))
}

func processRSS(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return kb
}

// processFootprint is macOS's physical footprint of a process (what
// Activity Monitor shows as memory), or "" where footprint(1) is missing.
func processFootprint(pid int) string {
	out, err := exec.Command("/usr/bin/footprint", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Footprint:") {
			return strings.TrimSpace(line[strings.Index(line, "Footprint:")+len("Footprint:"):])
		}
	}
	return ""
}

// TestAttachProcessFootprint runs cage attach as a separate process in a
// pseudo-terminal and measures its start, keystroke latency, resident memory
// idle and after output, and optionally its idle CPU. CAGE_ATTACH_BENCH=1
// runs it; CAGE_ATTACH_BINARY runs a built program (cage-attach, or cage
// with "attach" as its first argument) instead of this test binary;
// CAGE_ATTACH_BASELINE names a program that only connects to the controller
// and prints, for the start baseline; CAGE_ATTACH_IDLE=60s samples idle CPU.
func TestAttachProcessFootprint(t *testing.T) {
	if os.Getenv("CAGE_ATTACH_BENCH") == "" {
		t.Skip("set CAGE_ATTACH_BENCH=1")
	}
	if stamp := os.Getenv("CAGE_ATTACH_STAMP"); stamp != "" {
		// A built cage-attach accepts the controller only with its stamp.
		previous := version.Stamp
		version.Stamp = stamp
		t.Cleanup(func() { version.Stamp = previous })
	}
	s := attachController(t, nil)
	if program := os.Getenv("CAGE_ATTACH_BINARY"); program != "" {
		// The controller verifies the cage-attach beside its executable.
		s.exe = filepath.Join(filepath.Dir(strings.Fields(program)[0]), "cage")
	}
	session := startChild(t, s, "echo")
	args := []string{"--config", s.configPath, "--terminal", session.id}
	record, err := controlclient.ReadRecord(filepath.Dir(s.configPath))
	if err != nil {
		t.Fatal(err)
	}
	// start runs a program on a pseudo-terminal and returns the time to its
	// first byte of output.
	type running struct {
		cmd    *exec.Cmd
		master *os.File
		first  time.Duration
		mu     sync.Mutex
		out    bytes.Buffer
		change chan struct{}
	}
	start := func(cmd *exec.Cmd) *running {
		r := &running{cmd: cmd, change: make(chan struct{}, 1)}
		began := time.Now()
		master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
		if err != nil {
			t.Fatal(err)
		}
		r.master = master
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); master.Close() })
		go func() {
			buffer := make([]byte, 64<<10)
			for {
				n, err := master.Read(buffer)
				r.mu.Lock()
				if n > 0 && r.first == 0 {
					r.first = time.Since(began)
				}
				r.out.Write(buffer[:n])
				r.mu.Unlock()
				select {
				case r.change <- struct{}{}:
				default:
				}
				if err != nil {
					return
				}
			}
		}()
		return r
	}
	count := func(r *running, want string) int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return strings.Count(r.out.String(), want)
	}
	wait := func(r *running, want string, n int) {
		deadline := time.After(20 * time.Second)
		for count(r, want) < n {
			select {
			case <-r.change:
			case <-deadline:
				r.mu.Lock()
				t.Fatalf("never saw %q: %q", want, r.out.String())
			}
		}
	}
	firstByte := func(r *running) time.Duration { r.mu.Lock(); defer r.mu.Unlock(); return r.first }
	if baseline := os.Getenv("CAGE_ATTACH_BASELINE"); baseline != "" {
		var starts []time.Duration
		for i := 0; i < 21; i++ {
			b := start(exec.Command(baseline, strconv.Itoa(record.Port)))
			wait(b, "ready", 1)
			starts = append(starts, firstByte(b))
			_ = b.cmd.Wait()
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
		t.Logf("baseline (connect to the controller, one request, print): first byte median %v min %v max %v", starts[len(starts)/2], starts[0], starts[len(starts)-1])
	}
	command := func() *exec.Cmd {
		if program := os.Getenv("CAGE_ATTACH_BINARY"); program != "" {
			fields := strings.Fields(program)
			return exec.Command(fields[0], append(fields[1:], args...)...)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestAttachClientProcess$")
		cmd.Env = append(os.Environ(), "CAGE_ATTACH_CLIENT="+strings.Join(args, " "))
		return cmd
	}
	// Starts: each attach replays the terminal's history, whose first bytes
	// are the program's "ready".
	var starts []time.Duration
	for i := 0; i < 21; i++ {
		a := start(command())
		wait(a, "ready", 1)
		starts = append(starts, firstByte(a))
		_ = a.cmd.Process.Signal(syscall.SIGHUP)
		_ = a.cmd.Wait()
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	t.Logf("cage attach: first byte median %v min %v max %v", starts[len(starts)/2], starts[0], starts[len(starts)-1])
	a := start(command())
	wait(a, "ready", 1)
	time.Sleep(2 * time.Second)
	pid := a.cmd.Process.Pid
	idle, idleFootprint := processRSS(t, pid), processFootprint(pid)
	latencies := make([]time.Duration, 0, 150)
	for i := 0; i < 150; i++ {
		key := string(rune('a' + i%26))
		before := count(a, key)
		began := time.Now()
		if _, err := a.master.Write([]byte(key)); err != nil {
			t.Fatal(err)
		}
		wait(a, key, before+1)
		latencies = append(latencies, time.Since(began))
		time.Sleep(30 * time.Millisecond)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	// 4 MB of output at once.
	if _, err := a.master.Write([]byte{6}); err != nil {
		t.Fatal(err)
	}
	wait(a, "flood-done", 1)
	time.Sleep(500 * time.Millisecond)
	flood, floodFootprint := processRSS(t, pid), processFootprint(pid)
	// 4 MB typed and echoed: 2000 chunks of 2 KB.
	chunk := strings.Repeat("0123456789abcdef", 128)
	for i := 0; i < 2000; i++ {
		if _, err := a.master.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if i%50 == 49 {
			wait(a, "0123456789abcdef", (i+1)*128)
		}
	}
	wait(a, "0123456789abcdef", 2000*128)
	time.Sleep(500 * time.Millisecond)
	typed, typedFootprint := processRSS(t, pid), processFootprint(pid)
	t.Logf("keystroke through the process: median %v p95 %v max %v", latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1])
	t.Logf("RSS: idle %d KB, after 4 MB of output %d KB, after 4 MB typed and echoed %d KB", idle, flood, typed)
	t.Logf("footprint: idle %s, after 4 MB of output %s, after 4 MB typed and echoed %s", idleFootprint, floodFootprint, typedFootprint)
	if sample := os.Getenv("CAGE_ATTACH_IDLE"); sample != "" {
		d, err := time.ParseDuration(sample)
		if err != nil {
			t.Fatal(err)
		}
		cpu := func() string {
			out, _ := exec.Command("/bin/ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
			return strings.TrimSpace(string(out))
		}
		// One top across the whole sample, counting events as deltas: its
		// second sample covers exactly d.
		cpu0 := cpu()
		out, _ := exec.Command("/usr/bin/top", "-l", "2", "-s", strconv.Itoa(int(d.Seconds())), "-c", "d", "-pid", strconv.Itoa(pid), "-stats", "pid,cpu,time,idlew,csw,threads").Output()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		t.Logf("idle %v: CPU time %s -> %s; top (delta) PID %%CPU TIME IDLEW CSW #TH: %q; RSS %d KB", d, cpu0, cpu(), lines[len(lines)-1], processRSS(t, pid))
	}
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

func TestAttachThroughTheDesktopEndsOnLockAndSwitch(t *testing.T) {
	desktop, workload, full := desktopBeforeWorkload(t)
	gateway := desktop.remote.desktop
	through := func(session *terminalSession) *attachClient {
		a := startAttach(t, attach.Config{ConfigPath: full, Target: AgentTarget{Scope: "selected", Connection: gateway.generation}, Terminal: session.id})
		a.waitFor("ready", 1)
		a.typeText("hello")
		a.waitFor("got:\"hello\"", 1)
		return a
	}
	// --new through the desktop: the workload answers the viewer question
	// itself, so the request reaches it (and the test workload, which has no
	// environment, refuses it in its own words).
	n := startAttach(t, attach.Config{ConfigPath: full, Target: AgentTarget{Scope: "selected", Connection: gateway.generation}, NewKind: "shell"})
	if result := n.exit(10 * time.Second); result.Code != 1 || result.Message != errTerminalNotReady.Error() {
		t.Fatalf("--new through the desktop: %+v", result)
	}
	// The workload locks: the window says so at once.
	a := through(startChild(t, workload, "report"))
	locked := time.Now()
	workload.account.lock()
	if result := a.exit(5 * time.Second); result.Code != 3 || !strings.Contains(result.Message, "已锁定") {
		t.Fatalf("%+v", result)
	}
	a.leftReset()
	t.Logf("lock to attach end through the desktop: %v", time.Since(locked))
	workload.account.setKey(make([]byte, 32))
	// The desktop switches to another environment: the window ends and says why.
	b := through(startChild(t, workload, "report"))
	gateway.mu.Lock()
	old := gateway.active
	gateway.generation = randomToken()
	gateway.mu.Unlock()
	old.cancel()
	result := b.exit(5 * time.Second)
	if result.Code != 1 || !strings.Contains(result.Message, "已切换") {
		t.Fatalf("%+v", result)
	}
	b.leftReset()
}

// A window left inside a clipboard write when the attach ends gets it
// spoiled before the reset, so no truncated clipboard is applied.
func TestAttachExitSpoilsAnOpenClipboardWrite(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	a.typeText("?clip-open")
	a.waitFor(strings.Repeat("QUFB", 64), 1)
	a.cancel()
	a.exit(5 * time.Second)
	a.leftReset()
	if out := a.output(); !strings.HasSuffix(out, "QUFB!\x07"+attach.ResetSequence) {
		t.Fatalf("not spoiled: %q", out[max(0, len(out)-len(attach.ResetSequence)-20):])
	}
}

// Reconnecting gives up after the retry window, and resets the window too.
func TestAttachGivesUpAndResets(t *testing.T) {
	window := attach.RetryWindow
	attach.RetryWindow = 500 * time.Millisecond
	t.Cleanup(func() { attach.RetryWindow = window })
	var mu sync.Mutex
	refuse := false
	s := attachController(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			busy := refuse
			mu.Unlock()
			if busy {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	session := startChild(t, s, "report")
	var streams []*controlwire.Conn
	attach.StreamHook = func(ws *controlwire.Conn) { mu.Lock(); streams = append(streams, ws); mu.Unlock() }
	t.Cleanup(func() { attach.StreamHook = nil })
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	mu.Lock()
	refuse = true
	_ = streams[0].NetConn().Close()
	mu.Unlock()
	if result := a.exit(10 * time.Second); result.Code != 1 || !strings.Contains(result.Message, "已停止重试") {
		t.Fatalf("%+v", result)
	}
	a.leftReset()
}

// TestAttachExitPathChild is cage attach as its own process for
// TestAttachProcessEndsReset: CAGE_ATTACH_EXIT_PATH says how it will end.
func TestAttachExitPathChild(t *testing.T) {
	mode := os.Getenv("CAGE_ATTACH_EXIT_PATH")
	if mode == "" {
		return
	}
	if mode == "panic" {
		attach.StreamHook = func(*controlwire.Conn) { panic("attach test panic") }
	}
	os.Exit(attach.Main(strings.Fields(os.Getenv("CAGE_ATTACH_ARGS")), os.Stdin, os.Stdout, os.Stderr, attach.Options{Check: agentSameBuild}))
}

// A signal or a panic ends cage attach with the window reset and back in
// line mode. kill -9 cannot be caught: the launch script runs
// cage attach --reset after it.
func TestAttachProcessEndsReset(t *testing.T) {
	for _, mode := range []string{"SIGTERM", "SIGHUP", "panic"} {
		t.Run(mode, func(t *testing.T) {
			s := attachController(t, nil)
			session := startChild(t, s, "report")
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			_ = pty.Setsize(slave, &pty.Winsize{Cols: 100, Rows: 30})
			cmd := exec.Command(os.Args[0], "-test.run=^TestAttachExitPathChild$")
			cmd.Env = append(os.Environ(), "CAGE_ATTACH_EXIT_PATH="+mode, "CAGE_ATTACH_ARGS=--config "+s.configPath+" --terminal "+session.id)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			var outMu sync.Mutex
			go func() {
				buffer := make([]byte, 4096)
				for {
					n, err := master.Read(buffer)
					outMu.Lock()
					out.Write(buffer[:n])
					outMu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			read := func() string { outMu.Lock(); defer outMu.Unlock(); return out.String() }
			if mode != "panic" {
				deadline := time.Now().Add(10 * time.Second)
				for !strings.Contains(read(), "ready") {
					if time.Now().After(deadline) {
						t.Fatalf("never attached: %q", read())
					}
					time.Sleep(20 * time.Millisecond)
				}
				signal := map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGHUP": syscall.SIGHUP}[mode]
				_ = cmd.Process.Signal(signal)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("did not end: %q", read())
			}
			if (mode == "panic") != (err != nil) {
				t.Fatalf("exit %v: %q", err, read())
			}
			time.Sleep(100 * time.Millisecond)
			got := read()
			push := strings.Index(got, attach.TitlePush)
			reset := strings.Index(got, attach.ResetSequence)
			if push < 0 || reset < push {
				t.Fatalf("not reset: %q", got)
			}
			if mode == "panic" && !strings.Contains(got[reset:], "attach test panic") {
				t.Fatalf("the panic was not reported after the reset: %q", got[reset:])
			}
			// Back in line mode: the terminal echoes what is typed again.
			before := len(got)
			if _, err := master.Write([]byte("echo-check")); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for !strings.Contains(read()[before:], "echo-check") {
				if time.Now().After(deadline) {
					t.Fatalf("left in raw mode: %q", read()[before:])
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestAttachResumesAConversationThroughCagesLauncher(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	var target string
	s := attachController(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/cli/sessions/") && strings.HasSuffix(r.URL.Path, "/open") {
				mu.Lock()
				opened = append(opened, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/cli/sessions/"), "/open"))
				id := target
				mu.Unlock()
				jsonReply(w, map[string]any{"desktop_terminal_id": id})
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	session := startChild(t, s, "report")
	mu.Lock()
	target = session.id
	mu.Unlock()
	a := startAttach(t, attach.Config{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, Resume: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"})
	a.waitFor("ready", 1)
	a.typeText("resumed")
	a.waitFor("got:\"resumed\"", 1)
	a.cancel()
	a.exit(5 * time.Second)
	mu.Lock()
	if len(opened) != 1 || opened[0] != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" {
		t.Fatal(opened)
	}
	target = ""
	mu.Unlock()
	// A conversation the launcher could not put in a terminal.
	b := startAttach(t, attach.Config{ConfigPath: s.configPath, Target: AgentTarget{Scope: "local"}, Resume: "another-session"})
	if result := b.exit(5 * time.Second); result.Code != 1 || !strings.Contains(result.Message, "没有在终端中打开") {
		t.Fatalf("%+v", result)
	}
}

// cage attach has no keepalive timer of its own: it answers the server's
// pings, so an idle window stays attached past the server's read timeout.
func TestAttachIdleStaysAttachedWithoutItsOwnTimers(t *testing.T) {
	interval, timeout := terminalHeartbeatInterval, terminalReadTimeout
	terminalHeartbeatInterval, terminalReadTimeout = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { terminalHeartbeatInterval, terminalReadTimeout = interval, timeout })
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	var mu sync.Mutex
	streams := 0
	attach.StreamHook = func(*controlwire.Conn) { mu.Lock(); streams++; mu.Unlock() }
	t.Cleanup(func() { attach.StreamHook = nil })
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	time.Sleep(time.Second)
	a.typeText("after-idle")
	a.waitFor("got:\"after-idle\"", 1)
	mu.Lock()
	defer mu.Unlock()
	if streams != 1 {
		t.Fatal("an idle stream was dropped", streams)
	}
}

// A stream that stops speaking (a sleeping laptop, a stalled tunnel) is
// replaced after the silence, without losing or repeating output.
func TestAttachReplacesASilentStream(t *testing.T) {
	interval, silence := terminalHeartbeatInterval, attach.Silence
	terminalHeartbeatInterval, attach.Silence = time.Hour, 300*time.Millisecond
	t.Cleanup(func() { terminalHeartbeatInterval, attach.Silence = interval, silence })
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	var mu sync.Mutex
	streams := 0
	attach.StreamHook = func(*controlwire.Conn) { mu.Lock(); streams++; mu.Unlock() }
	t.Cleanup(func() { attach.StreamHook = nil })
	a := startAttach(t, attachTo(s, session.id))
	a.waitFor("ready", 1)
	time.Sleep(time.Second)
	a.typeText("after-silence")
	out := a.waitFor("got:\"after-silence\"", 1)
	mu.Lock()
	defer mu.Unlock()
	if streams < 2 || strings.Count(out, "ready") != 1 {
		t.Fatalf("streams %d, output %q", streams, out)
	}
}

// cage-attach accepts only a controller that verified it; any other ends
// with the version sentence before anything is created.
func TestAttachRefusesAControllerThatDidNotVerifyIt(t *testing.T) {
	s := attachController(t, nil)
	session := startChild(t, s, "report")
	before := len(s.terminals.snapshot())
	result := attach.Run(context.Background(), attachTo(s, session.id), attach.TTY{Out: io.Discard, ErrOut: io.Discard}, attach.Options{Check: controlclient.SameCompanion("another-build")})
	if result.Code != 1 || !strings.Contains(result.Message, "不是同一个版本") {
		t.Fatalf("%+v", result)
	}
	cfg := attachTo(s, "")
	cfg.Terminal, cfg.NewKind = "", "shell"
	result = attach.Run(context.Background(), cfg, attach.TTY{Out: io.Discard, ErrOut: io.Discard}, attach.Options{Check: controlclient.SameCompanion("another-build")})
	if result.Code != 1 || len(s.terminals.snapshot()) != before {
		t.Fatalf("%+v, terminals %d -> %d", result, before, len(s.terminals.snapshot()))
	}
}
