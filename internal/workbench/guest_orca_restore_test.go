package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

func TestGuestOrcaCLIReducesArgumentsToScopedOperations(t *testing.T) {
	watch := externalSessionWatch{Guest: true, Connection: "guest-independent", RuntimeID: "instance-a", Workspace: "/projects/quant"}
	env := []string{"--environment", watch.Connection, "--json"}
	for _, tc := range []struct {
		args []string
		op   string
	}{
		{[]string{"agent", "list", "--worktree", "path:/projects/quant"}, "agents"},
		{[]string{"terminal", "list", "--worktree", "path:/projects/quant", "--limit", "200"}, "terminals"},
		{[]string{"terminal", "wait", "--terminal", "term_fixture", "--for", "exit", "--timeout-ms", "30000"}, "wait"},
		{[]string{"worktree", "show", "--worktree", "id:fixture::/projects/quant"}, "worktree"},
		{[]string{"terminal", "create", "--worktree", "id:fixture", "--command", "sh -c whoami", "--keep-open"}, ""},
		{[]string{"agent", "list", "--worktree", "path:/projects/another"}, ""},
		{[]string{"open"}, ""},
	} {
		op, ok := parseGuestOrcaCLI(watch, append(tc.args, env...))
		if (tc.op != "") != ok || (ok && (op.Operation != tc.op || op.Workspace != watch.Workspace ||
			op.Runtime != "independent" || op.RuntimeID != watch.RuntimeID)) {
			t.Errorf("%q produced %+v, accepted=%t", tc.args, op, ok)
		}
	}
}

func TestGuestOrcaRegistrationRestoresWithNoDesktop(t *testing.T) {
	s := testServer(t)
	s.ready = true
	t.Cleanup(func() { s.beginClose(); s.activities.Wait() })
	const firstBoot = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	const secondBoot = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	const thread = "12345678-1234-4abc-abcd-123456789abc"
	const workspace = "/home/cage/quant"
	const worktree = "repo-1::/home/cage/quant"
	const originalHandle = "term_original_fixture"
	const resumedHandle = "term_recovery_fixture"
	account := codexFixture(strings.Repeat("a", 24), "origin@example.test")
	if err := writePrivate(filepath.Join(s.profile, "codex-accounts.json"), codexAccountList{Accounts: []codexAccount{account}}); err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	bindClaimSSH := func(server *Server) {
		home := t.TempDir()
		mobileTestSSHRewrite(t, server, home, []string{"HOME=" + home}, func(command string) string {
			if strings.Contains(command, "external-orca-resume") && strings.Contains(command, thread) {
				claims.Add(1)
				return "printf claimed"
			}
			return "exit 78"
		})
	}
	var mu sync.Mutex
	boot, instance, handle := firstBoot, "original-guest-orca", originalHandle
	var launches int
	var waitForExit chan struct{}
	var waiterReady chan struct{}
	hook := func(ctx context.Context, command string, input, output any) error {
		mu.Lock()
		defer mu.Unlock()
		switch command {
		case "external-session-origin":
			in := input.(externalOriginProbe)
			if in.Workspace != workspace || in.Provider != "codex" {
				return errors.New("origin probe left the original scope")
			}
			if in.BootOnly {
				*output.(*externalNativeOrigin) = externalNativeOrigin{State: "boot", BootID: boot}
			} else if in.Handle == handle && in.TabID == "tab" && in.WorktreeID == worktree {
				*output.(*externalNativeOrigin) = externalNativeOrigin{
					State: "found", BootID: boot, Thread: thread, Account: account.ID, Subject: account.subject(),
				}
			}
			return nil
		case "orca-guest-restore":
			in := input.(guestOrcaOp)
			if in.Workspace != workspace || in.Runtime != "independent" || in.RuntimeID != "" && in.RuntimeID != instance {
				return errors.New("wrong guest Orca scope")
			}
			reply := output.(*guestOrcaReply)
			reply.RuntimeID = instance
			terminal := localorca.TerminalPlacement{Handle: handle, TabID: "tab", LeafID: "leaf", WorktreeID: worktree,
				WorktreePath: workspace, IncarnationID: handle, Connected: true, Writable: true}
			var result any
			switch in.Operation {
			case "probe":
				return nil
			case "agents":
				rows := []localorca.AgentPlacement{}
				if handle != "" {
					rows = append(rows, localorca.AgentPlacement{Kind: "terminal", Provider: "codex", PaneKey: "tab:leaf", WorktreeID: worktree})
				}
				result = map[string]any{"agents": rows}
			case "terminals":
				rows := []localorca.TerminalPlacement{}
				if handle != "" {
					rows = append(rows, terminal)
				}
				result = map[string]any{"terminals": rows}
			case "worktree":
				result = map[string]any{"worktree": map[string]any{"id": worktree, "path": workspace}}
			case "resume":
				if in.Account != account.ID || in.Thread != thread || in.Subject != account.subject() || in.Worktree != worktree {
					return errors.New("attempt to substitute the original thread, account, or worktree")
				}
				launches++
				handle, terminal.Handle = resumedHandle, resumedHandle
				result = map[string]any{"terminal": terminal}
			case "wait":
				wait, ready := waitForExit, waiterReady
				if wait == nil {
					return context.DeadlineExceeded
				}
				waitForExit = nil
				close(ready)
				mu.Unlock()
				select {
				case <-wait:
					result = map[string]any{"wait": map[string]any{
						"handle": in.Handle, "condition": "exit", "status": "exited", "satisfied": true, "exitCode": 0,
					}}
				case <-ctx.Done():
					mu.Lock()
					return ctx.Err()
				}
				mu.Lock()
			default:
				return errors.New("guest Orca command is outside the allowlist")
			}
			reply.Result, _ = json.Marshal(result)
			return nil
		}
		return errors.New("unexpected runtime command")
	}
	s.agentCLIs.runtimeHook = hook
	if w := request(s, "GET", guestOrcaRestoreRoute, s.origin, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Guest Orca records: %d", w.Code)
	}
	if w := request(s, "POST", guestOrcaRestoreRoute, s.origin, `{"runtime":"independent","workspace":"/projects/../quant"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("noncanonical workspace registered: %d", w.Code)
	}
	if w := request(s, "POST", guestOrcaRestoreRoute, s.origin, `{"runtime":"independent","workspace":"/home/cage/quant"}`, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"verified":1`) || strings.Contains(w.Body.String(), account.subject()) {
		t.Fatalf("registering an Orca pane without a session ID: %d %s", w.Code, w.Body.String())
	}
	desktop, err := s.externalSessionRecords("local")
	if err != nil || len(desktop) != 0 {
		t.Fatalf("guest record leaked into desktop inventory: %#v %v", desktop, err)
	}
	// A new controller reads the same journal and adopts the original pane.
	next := testServer(t)
	next.profile, next.configPath, next.ready, next.agentCLIs.runtimeHook = s.profile, s.configPath, true, hook
	t.Cleanup(func() { next.beginClose(); next.activities.Wait() })
	bindClaimSSH(next)
	rescan := func() {
		t.Helper()
		retry, err := next.restoreWorkbenchRuntime(t.Context(), restoreRuntimeGuestOrca)
		if retry != 0 || err != nil {
			t.Fatalf("normal startup restore: retry=%b err=%v", retry, err)
		}
	}
	rescan()
	mu.Lock()
	handle = ""
	mu.Unlock()
	rescan()
	mu.Lock()
	if launches != 0 || claims.Load() != 0 {
		mu.Unlock()
		t.Fatal("same-boot disappearance recreated the pane")
	}
	boot = secondBoot
	mu.Unlock()
	rescan()
	rescan()
	mu.Lock()
	count := launches
	mu.Unlock()
	if count != 1 || claims.Load() != 1 {
		t.Fatalf("cold restore launched %d times with %d shared claims", count, claims.Load())
	}
	closed := make(chan struct{})
	ready := make(chan struct{})
	mu.Lock()
	waitForExit, waiterReady = closed, ready
	mu.Unlock()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
waiting:
	for {
		rescan()
		select {
		case <-ready:
			break waiting
		case <-tick.C:
		case <-timer.C:
			t.Fatal("no watcher attached to the resumed terminal")
		}
	}
	close(closed)
	for {
		_, views, err := next.guestOrcaWatches("local")
		if err != nil {
			t.Fatal(err)
		}
		if len(views) == 1 && views[0].Closed == 1 {
			break
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatal("successful same-boot exit was not recorded")
		}
	}
	mu.Lock()
	boot, handle = "cccccccc-cccc-4ccc-accc-cccccccccccc", ""
	mu.Unlock()
	rescan()
	mu.Lock()
	count = launches
	mu.Unlock()
	if count != 1 {
		t.Fatal("a normally closed pane reappeared after the next reboot")
	}
	next.mu.Lock()
	next.ready = false
	next.mu.Unlock()
	if w := request(next, "POST", guestOrcaRestoreRoute, next.origin, `{"action":"unregister","runtime":"independent","runtime_id":"original-guest-orca","workspace":"/home/cage/quant"}`, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workspaces":[]`) {
		t.Fatalf("could not stop watching while the guest is offline: %d %s", w.Code, w.Body.String())
	}
}
