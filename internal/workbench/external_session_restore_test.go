package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

func TestExternalOrcaNullIDAutomaticRecovery(t *testing.T) {
	ctx := context.Background()
	s := testServer(t)
	s.instanceID = "cage-fixture"
	workspace := t.TempDir()
	const (
		thread         = "1b34d357-883b-409a-afad-0ebd3d07710b"
		before         = "250a60d2-8ac6-47dc-b4a7-c7f5ec4861be"
		after          = "d965e2bb-3ccd-4372-84a9-6143143a23f2"
		env            = "verified-ssh-pair"
		runtimeID      = "paired-orca-runtime"
		originalHandle = "term_original"
		resumedHandle  = "term_resumed"
		worktree       = "original-worktree"
		host           = "original-host"
	)
	profile := codexFixture(strings.Repeat("b", 24), "qa@example.test")
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{profile}}); err != nil {
		t.Fatal(err)
	}
	boot, active, tab := before, originalHandle, "original-tab"
	claims, starts, focuses := 0, 0, 0
	s.agentCLIs.runtimeHook = func(_ context.Context, subcommand string, input, output any) error {
		in := input.(externalOriginProbe)
		out := output.(*externalNativeOrigin)
		if subcommand != "external-session-origin" || in.Provider != "codex" || in.Workspace != workspace {
			return fmt.Errorf("unexpected origin request")
		}
		if in.BootOnly {
			*out = externalNativeOrigin{State: "boot", BootID: boot}
		} else if active != "" && in.Handle == active && in.TabID == tab && in.WorktreeID == worktree {
			*out = externalNativeOrigin{State: "found", Thread: thread, Account: profile.ID, Subject: profile.subject(), BootID: boot}
		} else {
			*out = externalNativeOrigin{State: "unavailable"}
		}
		return nil
	}
	s.externalSessions.runGuestClaim = func(_ context.Context, path, id, epoch string) (bool, error) {
		if path != workspace || id != thread || epoch != after {
			t.Fatalf("claim escaped the original scope: %q %q %q", path, id, epoch)
		}
		claims++
		return claims == 1, nil
	}
	install := localorca.Installation{Installed: true, CLIPath: "fixture-orca"}
	s.orca.localDetect = func() localorca.Installation { return install }
	s.orca.localClient = localorca.NewClient(install)
	s.orca.localClient.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if slices.Equal(args, []string{"environment", "list", "--json"}) {
			return json.Marshal(map[string]any{"environments": []localorca.Environment{{ID: env, RuntimeID: runtimeID}}})
		}
		scoped := func(parts ...string) bool {
			return slices.Equal(args, append(parts, "--environment", env, "--json"))
		}
		switch {
		case scoped("agent", "list", "--worktree", "path:"+workspace):
			agents := []map[string]any{}
			if active != "" {
				agents = append(agents, map[string]any{
					"orcaSessionId": nil, "worktreeId": worktree, "hostId": host, "kind": "terminal",
					"agent": "codex", "paneKey": tab + ":leaf", "state": "working",
				})
			}
			return json.Marshal(map[string]any{"agents": agents})
		case scoped("terminal", "list", "--worktree", "path:"+workspace, "--limit", "200"):
			terminals := []localorca.TerminalPlacement{}
			if active != "" {
				terminals = append(terminals, localorca.TerminalPlacement{Handle: active, TabID: tab, LeafID: "leaf",
					WorktreeID: worktree, WorktreePath: workspace, ExecutionHostID: host,
					IncarnationID: "incarnation-" + active, Connected: true, Writable: true})
			}
			return json.Marshal(map[string]any{"terminals": terminals})
		case scoped("terminal", "switch", "--terminal", active):
			focuses++
			return json.Marshal(map[string]any{"focus": map[string]any{"handle": active, "worktreeId": worktree, "navigated": true}})
		case scoped("worktree", "show", "--worktree", "id:"+worktree):
			return json.Marshal(map[string]any{"worktree": map[string]any{"id": worktree, "path": workspace, "hostId": host}})
		default:
			command := "exec env CAGE_CODEX_SUBJECT=" + profile.subject() + " /usr/local/bin/cage-codex-account run " + profile.ID + " resume " + thread
			if !scoped("terminal", "create", "--worktree", "id:"+worktree, "--command", command, "--keep-open") {
				return nil, fmt.Errorf("unknown Orca request: %v", args)
			}
			starts++
			active, tab = resumedHandle, "resumed-tab"
			return json.Marshal(map[string]any{"terminal": localorca.TerminalPlacement{
				Handle: resumedHandle, WorktreeID: worktree, ExecutionHostID: host,
			}})
		}
	}
	watch := externalSessionWatch{CageInstance: s.instanceID, Connection: env, Workspace: workspace}
	if count, err := s.captureExternalOrca(ctx, watch, true); err != nil || count != 1 {
		t.Fatalf("capture = %d, %v", count, err)
	}
	records := func() []externalSessionRecord {
		t.Helper()
		rows, err := s.externalSessionRecords(s.instanceID)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := records()
	if len(rows) != 1 || rows[0].Native == nil || rows[0].Native.Thread != thread ||
		rows[0].Continuation != "pane:original-tab:leaf" || rows[0].RuntimeID != runtimeID {
		t.Fatalf("exact Orca/native origin not recorded: %+v", rows)
	}
	id := rows[0].ID
	result, err := s.recoverExternalSession(ctx, s.instanceID, id, "")
	if err != nil || result.State != "reattached" || focuses != 1 || claims != 0 || starts != 0 {
		t.Fatalf("live recovery = %+v, %v; focus %d, claims %d, starts %d", result, err, focuses, claims, starts)
	}
	active = ""
	if err := s.restoreExternalSessions(ctx); err != nil || claims != 0 || starts != 0 {
		t.Fatalf("same-boot absence launched a terminal: %v, claims %d, starts %d", err, claims, starts)
	}
	boot = after
	changedAccount := codexFixture(profile.ID, "different@example.test")
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{changedAccount}}); err != nil {
		t.Fatal(err)
	}
	if err := s.restoreExternalSessions(ctx); err != nil || claims != 0 || starts != 0 {
		t.Fatalf("a changed account launched the original thread: %v, claims %d, starts %d", err, claims, starts)
	}
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{profile}}); err != nil {
		t.Fatal(err)
	}
	if err := s.restoreExternalSessions(ctx); err != nil || claims != 1 || starts != 1 {
		t.Fatalf("reboot did not start exact native recovery: %v, claims %d, starts %d", err, claims, starts)
	}
	rows = records()
	if len(rows) != 1 || rows[0].ResumeBoot != after || rows[0].Native.BootID != before {
		t.Fatalf("terminal-create receipt overclaimed native confirmation: %+v", rows)
	}
	if err := s.restoreExternalSessions(ctx); err != nil || claims != 1 || starts != 1 {
		t.Fatalf("repeat startup duplicated a native launch: %v, claims %d, starts %d", err, claims, starts)
	}
	rows = records()
	if len(rows) != 1 || rows[0].ResumeBoot != "" || rows[0].Native.BootID != after {
		t.Fatalf("live provider proof did not confirm native recovery: %+v", rows)
	}
	if err := s.closeExternalSessionRecord(s.instanceID, id); err != nil {
		t.Fatal(err)
	}
	active = ""
	if err := s.restoreExternalSessions(ctx); err != nil || len(records()) != 0 || starts != 1 {
		t.Fatalf("explicit closure was revived: %v, starts %d", err, starts)
	}
}

func TestExternalOrcaGuestClaimIsAtomic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a Linux kernel boot identity")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	home := t.TempDir()
	workspace := t.TempDir()
	const thread = "1b34d357-883b-409a-afad-0ebd3d07710b"
	command := func() *exec.Cmd {
		cmd := exec.Command(python, "-I", "-c", externalOrcaGuestClaim, workspace, thread, strings.TrimSpace(string(boot)))
		cmd.Env = append(os.Environ(), "HOME="+home)
		return cmd
	}
	type reply struct {
		out []byte
		err error
	}
	done := make(chan reply, 2)
	for range 2 {
		go func() {
			out, err := command().CombinedOutput()
			done <- reply{out: out, err: err}
		}()
	}
	var states []string
	for range 2 {
		r := <-done
		if r.err != nil {
			t.Fatalf("guest claim failed: %v %s", r.err, r.out)
		}
		states = append(states, strings.TrimSpace(string(r.out)))
	}
	slices.Sort(states)
	if !slices.Equal(states, []string{"already_claimed", "claimed"}) {
		t.Fatalf("cross-process submissions were not guarded: %v", states)
	}
}
