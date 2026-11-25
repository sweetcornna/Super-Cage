//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
)

func startCountingSSHServer(t *testing.T, s *Server, home string) *atomic.Int64 {
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
