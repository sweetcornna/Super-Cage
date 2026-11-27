//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

const (
	crashOrcaConnection = "fixture-ssh"
	crashOrcaRuntime    = "fixture-orca-runtime"
	crashOrcaSession    = "orca_session_id:430e0677-b505-4ad6-994a-2a4382396f11"
	crashOrcaWorktree   = "fixture-original-worktree"
	crashOrcaTab        = "fixture-tab"
	crashOrcaLeaf       = "fixture-leaf"
	crashOrcaPane       = crashOrcaTab + ":" + crashOrcaLeaf
	crashOrcaHost       = "fixture-original-host"
	crashOrcaTerminal   = "term_fixture"
)

type crashLocalState struct {
	Project       string
	FocusConfirms bool
	Orphaned      bool
	FocusAttempts int
	OrcaOpens     int
	DeepSeekOpens int
}

type crashLocalClient struct {
	mu   sync.Mutex
	path string
}

func (c *crashLocalClient) read() (crashLocalState, error) {
	var state crashLocalState
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

func (c *crashLocalClient) openDeepSeek(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.read()
	if err != nil {
		return err
	}
	state.DeepSeekOpens++
	return writePrivate(c.path, state)
}

func (c *crashLocalClient) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.read()
	if err != nil {
		return nil, err
	}
	if binary != "synthetic-orca" {
		return nil, fmt.Errorf("fixture refused binary %q", binary)
	}
	scoped := func(parts ...string) bool {
		return slices.Equal(args, append(parts, "--environment", crashOrcaConnection, "--json"))
	}
	place := localorca.AgentPlacement{SessionID: crashOrcaSession, Provider: "codex", Kind: "terminal",
		HostID: crashOrcaHost, WorktreeID: crashOrcaWorktree, PaneKey: crashOrcaPane}
	switch {
	case slices.Equal(args, []string{"environment", "list", "--json"}):
		env := localorca.Environment{ID: crashOrcaConnection, RuntimeID: crashOrcaRuntime}
		return json.Marshal(map[string]any{"environments": []localorca.Environment{env}})
	case scoped("agent", "list", "--worktree", "path:"+state.Project):
		return json.Marshal(map[string]any{"agents": []localorca.AgentPlacement{place}})
	case scoped("agent", "show", "--id", crashOrcaSession):
		return json.Marshal(map[string]any{"orcaSessionId": crashOrcaSession, "places": []localorca.AgentPlacement{place}})
	case scoped("terminal", "list", "--worktree", "path:"+state.Project, "--limit", "200"):
		row := localorca.TerminalPlacement{Handle: crashOrcaTerminal, WorktreeID: crashOrcaWorktree, WorktreePath: state.Project,
			TabID: crashOrcaTab, LeafID: crashOrcaLeaf, ExecutionHostID: crashOrcaHost, Connected: true, Writable: true, Orphaned: state.Orphaned}
		return json.Marshal(map[string]any{"terminals": []localorca.TerminalPlacement{row}})
	case scoped("terminal", "switch", "--terminal", crashOrcaTerminal):
		state.FocusAttempts++
		if err := writePrivate(c.path, state); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"focus": map[string]any{
			"handle": crashOrcaTerminal, "worktreeId": crashOrcaWorktree, "navigated": state.FocusConfirms,
		}})
	case scoped("worktree", "activate", "--worktree", "path:"+state.Project):
		return []byte("{}"), nil
	case slices.Equal(args, []string{"open", "--json"}):
		state.OrcaOpens++
		return []byte("{}"), writePrivate(c.path, state)
	default:
		return nil, fmt.Errorf("fixture refused changed Orca route: %q", args)
	}
}

func TestWorkbenchCrashRecoveryExternalSessions(t *testing.T) {
	f := newCrashFixture(t)
	local := crashLocalClient{path: filepath.Join(filepath.Dir(f.profile), "external-client.json")}
	f.writeFile(t, local.path, crashLocalState{Project: f.project, FocusConfirms: true})
	state := func() crashLocalState {
		t.Helper()
		got, err := local.read()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	post := func(c *crashController, in externalSessionRequest, want int) string {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return c.run(t, "POST", externalSessionRoute, string(raw), want)
	}
	records := func(c *crashController) []externalSessionRecord {
		t.Helper()
		raw := c.run(t, "GET", externalSessionRoute, "", http.StatusOK)
		var view struct{ Records []externalSessionRecord }
		if err := json.Unmarshal([]byte(raw), &view); err != nil {
			t.Fatal(err)
		}
		return view.Records
	}
	owner := startCrashController(t, f.profile)
	owner.createAccount(t)
	owner.run(t, "attach", "", "", http.StatusOK)
	owner.run(t, "settled", "", "", http.StatusOK)
	post(owner, externalSessionRequest{Action: "watch_orca", Connection: crashOrcaConnection, Workspace: f.project}, http.StatusOK)
	const dshAccount = "fixture-account"
	addDeepSeek := func(application, connection, id, profile string) externalSessionRecord {
		t.Helper()
		body := post(owner, externalSessionRequest{Action: "record_deepseek", Application: application, Connection: connection,
			Workspace: f.project, Continuation: id, Profile: profile, Account: dshAccount}, http.StatusOK)
		var record externalSessionRecord
		if err := json.Unmarshal([]byte(body), &record); err != nil || record.ID == "" {
			t.Fatalf("invalid DeepSeek recording: %v %s", err, body)
		}
		return record
	}
	tui := addDeepSeek("deepseek-tui", "fixture-ssh", "fixture-continuation", "fixture-profile")
	desktop := addDeepSeek("deepseek-desktop", "local", "fixture-desktop", "")
	closed := addDeepSeek("deepseek-tui", "fixture-ssh", "fixture-closed", "fixture-profile")
	post(owner, externalSessionRequest{Action: "close", ID: closed.ID}, http.StatusOK)
	owner.kill(t)

	reopened := startCrashController(t, f.profile)
	reopened.run(t, "attach", "", "", http.StatusOK)
	reopened.run(t, "GET", externalSessionRoute, "", http.StatusLocked)
	reopened.unlock(t)
	reopened.run(t, "settled", "", "", http.StatusOK)
	list := records(reopened)
	if len(list) != 3 {
		t.Fatalf("external records lost, duplicated, or revived a closed session: %+v", list)
	}
	var orca externalSessionRecord
	for _, record := range list {
		if record.App == "orca" {
			orca = record
		} else if record.Account != dshAccount || record.Workspace != f.project {
			t.Fatalf("DeepSeek account or project changed: %+v", record)
		}
	}
	if orca.Continuation != crashOrcaSession || orca.Connection != crashOrcaConnection || orca.RuntimeID != crashOrcaRuntime ||
		orca.Provider != "codex" || orca.WorktreeID != crashOrcaWorktree || orca.Workspace != f.project {
		t.Fatalf("Orca record lost its client/worktree binding: %+v", orca)
	}
	if got := state(); got.FocusAttempts != 0 || got.OrcaOpens != 0 || got.DeepSeekOpens != 0 {
		t.Fatalf("readiness reported or opened a local application without consent: %+v", got)
	}
	resume := func(id, account, want string) externalSessionResult {
		t.Helper()
		raw := post(reopened, externalSessionRequest{Action: "resume", ID: id, Account: account}, http.StatusOK)
		var result externalSessionResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result.State != want {
			t.Fatalf("native recovery of %s: %v %s, want %s", id, err, raw, want)
		}
		return result
	}
	resume(orca.ID, "", "reattached")
	if got := state(); got.FocusAttempts != 1 || got.OrcaOpens != 0 {
		t.Fatalf("Orca recovery did not focus precisely one existing pane: %+v", got)
	}
	refused := state()
	refused.FocusConfirms = false
	f.writeFile(t, local.path, refused)
	resume(orca.ID, "", "client_opened")
	if got := state(); got.FocusAttempts != 2 || got.OrcaOpens != 1 {
		t.Fatalf("unconfirmed focus was treated as verified: %+v", got)
	}
	orphan := state()
	orphan.Orphaned, orphan.FocusConfirms = true, true
	f.writeFile(t, local.path, orphan)
	resume(orca.ID, "", "client_opened")
	if got := state(); got.FocusAttempts != 2 || got.OrcaOpens != 2 {
		t.Fatalf("an orphaned terminal was treated as attached: %+v", got)
	}
	post(reopened, externalSessionRequest{Action: "resume", ID: tui.ID, Account: "fixture-other-account"}, http.StatusBadRequest)
	post(reopened, externalSessionRequest{Action: "resume", ID: closed.ID, Account: dshAccount}, http.StatusBadRequest)
	command := resume(tui.ID, dshAccount, "native_command")
	if command.Dir != f.project || !slices.Equal(command.Argv, []string{"dsh", "--profile", "fixture-profile", "--resume", tui.Continuation}) {
		t.Fatalf("DeepSeek recovery constructed a different continuation: %+v", command)
	}
	deepSeekOpens := 0
	if runtime.GOOS == "darwin" {
		deepSeekOpens = 1
	}
	manual := resume(desktop.ID, dshAccount, "awaiting_client")
	if manual.Message == "" || len(manual.Argv) != 0 || state().DeepSeekOpens != deepSeekOpens {
		t.Fatalf("DeepSeek desktop overclaimed native restoration: %+v", manual)
	}
	if repeated := records(reopened); len(repeated) != 3 {
		t.Fatalf("repeat recovery changed the record count: %+v", repeated)
	}
	reopened.kill(t)
}
