//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func startCountingSSHServer(t *testing.T, s *Server, home string) *atomic.Int64 {
	return startInterceptingSSHServer(t, s, home, nil)
}

func startInterceptingSSHServer(t *testing.T, s *Server, home string, onExec func(command string)) *atomic.Int64 {
	t.Helper()
	var spawns atomic.Int64
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, _ := ssh.NewPublicKey(public)
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), clientKey.Marshal()) {
			return nil, errors.New("unknown test client")
		}
		return nil, nil
	}}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	dir := s.instanceDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "fixture-key")
	block, err := ssh.MarshalPrivateKey(private, "cage-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	pub, _ := ssh.NewPublicKey(hostPublic)
	known := filepath.Join(dir, "fixture-known-hosts")
	if err := os.WriteFile(known, append([]byte(fmt.Sprintf("[127.0.0.1]:%d ", port)), ssh.MarshalAuthorizedKey(pub)...), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf("Host *\n HostName 127.0.0.1\n Port %d\n User cage\n StrictHostKeyChecking yes\n UserKnownHostsFile %q\n IdentityFile %q\n", port, known, keyFile)
	if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(dir, "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	pythonPath, _ := exec.LookPath("python3")
	if pythonPath == "" {
		pythonPath = "/usr/bin/python3"
	}
	env := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for target := range channels {
					if target.ChannelType() != "session" {
						_ = target.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					ch, reqs, err := target.Accept()
					if err != nil {
						return
					}
					func() {
						defer ch.Close()
						for req := range reqs {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							var in struct{ Command string }
							if ssh.Unmarshal(req.Payload, &in) != nil {
								return
							}
							_ = req.Reply(true, nil)
							spawns.Add(1)
							if onExec != nil {
								onExec(in.Command)
							}
							rewritten := strings.ReplaceAll(in.Command, "/usr/bin/python3", pythonPath)
							cmd := exec.Command("/bin/sh", "-c", rewritten)
							cmd.Dir, cmd.Env = home, env
							cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
							err := cmd.Run()
							code := uint32(0)
							if err != nil {
								code = 1
								if exitErr, ok := err.(*exec.ExitError); ok {
									code = uint32(exitErr.ExitCode())
								}
							}
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
							return
						}
					}()
				}
			}()
		}
	}()
	return &spawns
}

func TestDisabledT3CodeAndMagpieSpawnZeroSSH(t *testing.T) {
	s := testServer(t)
	s.ready = true
	spawns := startCountingSSHServer(t, s, t.TempDir())
	t.Cleanup(func() { _ = s.closeService() })

	for _, path := range []string{"/api/t3code", "/api/magpie", "/api/herdr"} {
		w := request(s, "GET", path, "", "", true)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if got := spawns.Load(); got != 0 {
		t.Fatalf("disabled T3Code/Magpie/Herdr GETs spawned %d SSH processes, want 0", got)
	}
}

func TestConcurrentStatusAndCodexProbesCoalesce(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if err := os.MkdirAll(s.instanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// Enable T3Code, Magpie, and Herdr so probes actually run.
	s.t3code.mu.Lock()
	cfgT3 := defaultT3CodeConfig()
	cfgT3.Enabled = true
	_ = s.saveT3CodeConfigLocked(cfgT3)
	s.t3code.mu.Unlock()
	s.magpie.mu.Lock()
	cfgM := defaultMagpieConfig()
	cfgM.Enabled = true
	_ = s.saveMagpieConfigLocked(cfgM)
	s.magpie.mu.Unlock()
	s.herdr.mu.Lock()
	cfgH := defaultHerdrConfig()
	cfgH.Enabled = true
	_ = s.saveHerdrConfigLocked(cfgH)
	s.herdr.mu.Unlock()

	var t3Calls, magpieCalls, herdrCalls, codexCalls atomic.Int64
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, _ any, _ any) error {
		time.Sleep(40 * time.Millisecond)
		switch subcmd {
		case "t3code-status":
			t3Calls.Add(1)
		case "magpie-status":
			magpieCalls.Add(1)
		case "herdr-status":
			herdrCalls.Add(1)
		}
		return nil
	}
	s.codexAccounts.probe = func(context.Context, string) (codexGuestSnapshot, error) {
		time.Sleep(40 * time.Millisecond)
		codexCalls.Add(1)
		return codexGuestSnapshot{LoggedIn: true, Email: "c@example.com", Organization: "acct-1"}, nil
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(4)
		go func() { defer wg.Done(); _ = request(s, "GET", "/api/t3code", "", "", true) }()
		go func() { defer wg.Done(); _ = request(s, "GET", "/api/magpie", "", "", true) }()
		go func() { defer wg.Done(); _ = request(s, "GET", "/api/herdr", "", "", true) }()
		go func() { defer wg.Done(); _ = request(s, "GET", "/api/codex-accounts", "", "", true) }()
	}
	wg.Wait()

	if t3Calls.Load() != 1 {
		t.Fatalf("concurrent /api/t3code ran %d probes, want 1", t3Calls.Load())
	}
	if magpieCalls.Load() != 1 {
		t.Fatalf("concurrent /api/magpie ran %d probes, want 1", magpieCalls.Load())
	}
	if herdrCalls.Load() != 1 {
		t.Fatalf("concurrent /api/herdr ran %d probes, want 1", herdrCalls.Load())
	}
	if codexCalls.Load() != 1 {
		t.Fatalf("concurrent /api/codex-accounts ran %d default probes, want 1", codexCalls.Load())
	}
}

func TestLinuxAgentRuntimeReusesSharedGuestSession(t *testing.T) {
	s := testServer(t)
	s.ready = true
	home := t.TempDir()
	spawns := startCountingSSHServer(t, s, home)
	t.Cleanup(func() { _ = s.closeService() })

	s.t3code.mu.Lock()
	cfgT3 := defaultT3CodeConfig()
	cfgT3.Enabled = true
	_ = s.saveT3CodeConfigLocked(cfgT3)
	s.t3code.mu.Unlock()
	s.magpie.mu.Lock()
	cfgM := defaultMagpieConfig()
	cfgM.Enabled = true
	_ = s.saveMagpieConfigLocked(cfgM)
	s.magpie.mu.Unlock()

	for _, path := range []string{
		"/api/agent-clis",
		"/api/agent-clis/antigravity/accounts",
		"/api/agent-clis/opencode/accounts",
		"/api/t3code",
		"/api/magpie",
	} {
		w := request(s, "GET", path, "", "", true)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// Delete the installed helper on disk to verify exit-86 self-healing also
	// reinstalls and retries over the same shared guestSession (still 1 SSH).
	scriptPath := filepath.Join(home, agentRuntimeSpec.entryRelPath())
	if err := os.Remove(scriptPath); err != nil {
		t.Fatalf("expected agent-runtime script at %s: %v", scriptPath, err)
	}
	s.invalidateAgentDefaultCache("antigravity")
	if w := request(s, "GET", "/api/agent-clis/antigravity/accounts", "", "", true); w.Code != http.StatusOK {
		t.Fatalf("self-healing GET after script removal failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("self-healing did not reinstall script at %s: %v", scriptPath, err)
	}

	if got := spawns.Load(); got != 1 {
		t.Fatalf("6 agent-runtime/ensure calls on Linux spawned %d SSH processes, want 1 shared guestSession", got)
	}
}

// F2 (Linux attach): automatic reattach outside any job sets s.ready = true
// and must wake the parked safety monitor immediately.
func TestLinuxAttachOutsideJobWakesParkedSafetyMonitor(t *testing.T) {
	s := transferMountServer(t, false, "current")
	home := t.TempDir()
	_ = startCountingSSHServer(t, s, home)
	t.Cleanup(func() { _ = s.closeService() })

	// Stub guestConnectionSnapshot so attach() reads valid credentials and manifest
	// over the test SSH server without touching real /etc/claude-cage.
	payload := makeValidGuestConnectionPayload(t)
	payloadFile := filepath.Join(home, "conn.txt")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	oldSnap := guestConnectionSnapshot
	guestConnectionSnapshot = `cat "$HOME/conn.txt"`
	t.Cleanup(func() { guestConnectionSnapshot = oldSnap })

	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       http.NoBody,
			Request:    r,
		}, nil
	}))
	s.safety.VerifiedAt = time.Now().Add(-time.Minute)

	// Start with VM stopped (no cage-running.json) so safety monitor parks.
	runningMarker := filepath.Join(s.instanceDir(), "cage-running.json")
	_ = os.Remove(runningMarker)
	s.startMonitors()
	time.Sleep(100 * time.Millisecond)

	// Now VM is running again and attach() runs outside any job.
	if err := writePrivate(runningMarker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	wake := s.monitorWakeCh()
	if err := s.attach(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("attach() set s.ready = true without waking parked monitors")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ran := s.safety.Error != ""
		s.mu.Unlock()
		if ran {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("parked safety monitor did not wake and run monitorSafety after attach()")
}

// F9: ensureAgentRuntime during terminal creation (terminal_service.go,
// codex_terminal.go) and Herdr viewer attach (herdr_terminals.go) must not
// hold s.op across the cold guest install.
func TestTerminalEnsureAgentRuntimeLeavesOpLockFree(t *testing.T) {
	runCase := func(t *testing.T, name string, invoke func(s *Server)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			s.ready = true
			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := writePrivate(filepath.Join(s.profile, "environment.json"), cage.EnvironmentRecord{Directory: ws}); err != nil {
				t.Fatal(err)
			}
			shareDone := make(chan struct{})
			s.mu.Lock()
			s.preferences.Workspace = ws
			s.projectShares = map[string]*projectShare{ws: {done: shareDone}}
			s.mu.Unlock()
			home := t.TempDir()
			ensureStarted := make(chan struct{})
			releaseEnsure := make(chan struct{})
			var startedOnce sync.Once
			// Block the shared control session open (the first SSH call inside
			// ensureAgentRuntime on a cold Linux guest) while we probe s.op.TryLock().
			_ = startInterceptingSSHServer(t, s, home, func(command string) {
				if strings.Contains(command, "CAGE-GUEST-SESSION") || strings.Contains(command, "NamedTemporaryFile") {
					startedOnce.Do(func() { close(ensureStarted) })
					select {
					case <-releaseEnsure:
					case <-time.After(3 * time.Second):
					}
				}
			})
			t.Cleanup(func() {
				select {
				case <-releaseEnsure:
				default:
					close(releaseEnsure)
				}
				close(shareDone)
				_ = s.closeService()
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				invoke(s)
			}()

			select {
			case <-ensureStarted:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s: ensureAgentRuntime never started over SSH", name)
			}

			free := s.op.TryLock()
			if free {
				s.op.Unlock()
			}
			close(releaseEnsure)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: call did not finish", name)
			}
			if !free {
				t.Fatalf("%s: s.op was held while ensureAgentRuntime ran", name)
			}
		})
	}

	runCase(t, "createTerminal_herdr", func(s *Server) {
		_, _ = s.createTerminal(terminalRequest{Kind: "herdr"})
	})

	runCase(t, "createTerminal_agent_login", func(s *Server) {
		_, _ = s.createTerminal(terminalRequest{Kind: "agent-login", AgentID: "codex"})
	})

	runCase(t, "createCodexTerminal_herdr_takeover", func(s *Server) {
		s.herdr.mu.Lock()
		cfg := defaultHerdrConfig()
		cfg.Enabled = true
		cfg.TakeoverTerminals = true
		cfg.InstalledHint = true
		_ = s.saveHerdrConfigLocked(cfg)
		s.herdr.mu.Unlock()
		_, _ = s.createTerminal(terminalRequest{Kind: "codex"})
	})

	runCase(t, "openHerdrViewer", func(s *Server) {
		s.herdr.mu.Lock()
		cfg := defaultHerdrConfig()
		cfg.Enabled = true
		cfg.InstalledHint = true
		_ = s.saveHerdrConfigLocked(cfg)
		s.herdr.mu.Unlock()
		id := herdrImportedID("t1")
		s.herdr.listMu.Lock()
		s.herdr.list = herdrList{
			Running: true,
			Terminals: []herdrEntry{{
				Address: "t1",
				Name:    "t1",
			}},
		}
		s.herdr.listAt = time.Now()
		s.herdr.listMu.Unlock()
		_, _ = s.openHerdrViewer(context.Background(), id)
	})
}

// F10: Cold ensureAgentRuntime after guest boot must have its own timeout
// budget separate from the 6 s status probe timeout in herdr, t3code, and magpie.
func TestStatusProbesGiveColdEnsureOwnBudget(t *testing.T) {
	const coldDelay = 6200 * time.Millisecond
	cases := []struct {
		name      string
		path      string
		enable    func(*Server)
		installed func(t *testing.T, body []byte) (bool, string)
	}{
		{
			name: "herdr",
			path: "/api/herdr",
			enable: func(s *Server) {
				s.herdr.mu.Lock()
				cfg := defaultHerdrConfig()
				cfg.Enabled = true
				_ = s.saveHerdrConfigLocked(cfg)
				s.herdr.mu.Unlock()
			},
		},
		{
			name: "t3code",
			path: "/api/t3code",
			enable: func(s *Server) {
				s.t3code.mu.Lock()
				cfg := defaultT3CodeConfig()
				cfg.Enabled = true
				_ = s.saveT3CodeConfigLocked(cfg)
				s.t3code.mu.Unlock()
			},
		},
		{
			name: "magpie",
			path: "/api/magpie",
			enable: func(s *Server) {
				s.magpie.mu.Lock()
				cfg := defaultMagpieConfig()
				cfg.Enabled = true
				_ = s.saveMagpieConfigLocked(cfg)
				s.magpie.mu.Unlock()
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testServer(t)
			s.ready = true
			tc.enable(s)
			home := t.TempDir()
			// Delay dedicated SSH install (shared session times out/fails opening
			// or falls back to dedicated SSH) by > 6 s so cold ensure exceeds the
			// 6 s probe timeout if they share one budget.
			s.controlSession.mu.Lock()
			s.controlSession.retryAt = time.Now().Add(time.Minute)
			s.controlSession.openErr = errors.New("shared session unavailable in cold fallback test")
			s.controlSession.mu.Unlock()

			_ = startInterceptingSSHServer(t, s, home, func(command string) {
				if strings.Contains(command, "NamedTemporaryFile") {
					time.Sleep(coldDelay)
				}
			})
			t.Cleanup(func() { _ = s.closeService() })

			w := request(s, "GET", tc.path, "", "", true)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d %s", tc.path, w.Code, w.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode %s: %v", tc.path, err)
			}
			if probeErr, _ := payload["error"].(string); probeErr != "" {
				t.Fatalf("GET %s failed under cold ensure (%v): error=%q", tc.path, coldDelay, probeErr)
			}
		})
	}
}

// F12: ensureGuestScript must cap the encoded shared-session script below
// Linux's 128 KiB MAX_ARG_STRLEN so oversized bundles go straight to dedicated
// SSH upload without wasting a shared-session E2BIG round trip, and the dead
// "prepare-home" entry must not be listed in agentRuntimeUsesSharedSession.
func TestEnsureGuestScriptCapsSharedSessionArgLenAndRemovesDeadPrepareHome(t *testing.T) {
	if agentRuntimeUsesSharedSession("prepare-home") {
		t.Error(`agentRuntimeUsesSharedSession("prepare-home") = true, want false (dead subcommand)`)
	}

	// Build an incompressible ~110 KiB payload: its gzipped size is ~110 KiB
	// (below the old 256 KiB guard), but its base64-encoded sharedInstallScript
	// is ~147 KiB, exceeding Linux's 128 KiB MAX_ARG_STRLEN.
	raw := make([]byte, 110<<10)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	spec := newGuestScriptBundle(".local/share/cage/test-big", strings.Repeat("a", 64), 0o700, ".bin", []guestScriptFile{
		{suffix: ".bin", content: raw},
	})
	if encoded := len(spec.sharedInstallScript("python3")); encoded <= 128<<10 {
		t.Fatalf("fixture sharedInstallScript len = %d, want > 128 KiB", encoded)
	}

	s := testServer(t)
	s.ready = true
	home := t.TempDir()
	var sharedOpens atomic.Int64
	spawns := startInterceptingSSHServer(t, s, home, func(command string) {
		if strings.Contains(command, "CAGE-GUEST-SESSION") {
			sharedOpens.Add(1)
		}
	})
	t.Cleanup(func() { _ = s.closeService() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ensureGuestScript(ctx, spec, "python3"); err != nil {
		t.Fatalf("ensureGuestScript: %v", err)
	}
	if sharedOpens.Load() != 0 {
		t.Fatalf("ensureGuestScript attempted shared guestSession %d times for >128 KiB encoded script, want 0 (go straight to dedicated SSH)", sharedOpens.Load())
	}
	if spawns.Load() != 1 {
		t.Fatalf("ensureGuestScript spawned %d SSH processes, want 1 dedicated upload", spawns.Load())
	}
}
