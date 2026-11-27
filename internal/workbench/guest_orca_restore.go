package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

const guestOrcaRestoreRoute = "/api/orca/guest-recovery"

var guestOrcaResumeCommand = regexp.MustCompile(`^exec env CAGE_CODEX_SUBJECT=([0-9a-f]{64}) /usr/local/bin/cage-codex-account run ([0-9a-f]{24}) resume ([0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})$`)

type guestOrcaOp struct {
	Operation string `json:"operation"`
	Runtime   string `json:"runtime"`
	RuntimeID string `json:"runtime_id,omitempty"`
	Workspace string `json:"workspace"`
	Worktree  string `json:"worktree,omitempty"`
	Handle    string `json:"handle,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Account   string `json:"account,omitempty"`
	Thread    string `json:"thread,omitempty"`
}

type guestOrcaReply struct {
	RuntimeID string          `json:"runtime_id"`
	Result    json.RawMessage `json:"result,omitempty"`
}

func guestOrcaRuntime(connection string) string {
	switch connection {
	case "guest-independent":
		return "independent"
	case "guest-managed":
		return "managed"
	default:
		return ""
	}
}

func validGuestOrcaWorkspace(workspace string) bool {
	return externalSessionWorkspace(workspace) && strings.HasPrefix(workspace, "/") &&
		workspace != "/" && path.Clean(workspace) == workspace
}

func (s *Server) guestOrcaSupported() bool {
	return s.codexGuestSupported() && !s.clientOnly && !s.desktopEntry
}

func (s *Server) guestOrcaContext(parent context.Context) (context.Context, func(), error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	ready := s.runtimeRestoreAllowedLocked()
	s.mu.Unlock()
	if !ready {
		return nil, nil, errTerminalNotReady
	}
	epoch, unlocked := s.account.access()
	if requested, _ := parent.Value(accountEpochKey{}).(context.Context); !unlocked || requested != nil && requested != epoch {
		return nil, nil, errClaudeAccessLost
	}
	ctx, cancel := context.WithCancel(context.WithValue(parent, accountEpochKey{}, epoch))
	stop := context.AfterFunc(epoch, cancel)
	return ctx, func() { stop(); cancel() }, nil
}

func parseGuestOrcaCLI(watch externalSessionWatch, args []string) (guestOrcaOp, bool) {
	in := guestOrcaOp{Runtime: guestOrcaRuntime(watch.Connection), RuntimeID: watch.RuntimeID, Workspace: watch.Workspace}
	if len(args) < 5 || !slices.Equal(args[len(args)-3:], []string{"--environment", watch.Connection, "--json"}) {
		return in, false
	}
	args = args[:len(args)-3]
	switch {
	case slices.Equal(args, []string{"agent", "list", "--worktree", "path:" + watch.Workspace}):
		in.Operation = "agents"
	case slices.Equal(args, []string{"terminal", "list", "--worktree", "path:" + watch.Workspace, "--limit", "200"}):
		in.Operation = "terminals"
	case len(args) == 4 && slices.Equal(args[:3], []string{"worktree", "show", "--worktree"}) && strings.HasPrefix(args[3], "id:"):
		in.Operation, in.Worktree = "worktree", strings.TrimPrefix(args[3], "id:")
		return in, externalSessionText(in.Worktree, 1200)
	case len(args) == 8 && slices.Equal(args[:3], []string{"terminal", "wait", "--terminal"}) &&
		slices.Equal(args[4:], []string{"--for", "exit", "--timeout-ms", "30000"}):
		in.Operation, in.Handle = "wait", args[3]
		return in, externalTerminalHandle.MatchString(in.Handle)
	case len(args) == 7 && slices.Equal(args[:3], []string{"terminal", "create", "--worktree"}) &&
		strings.HasPrefix(args[3], "id:") && args[4] == "--command" && args[6] == "--keep-open":
		parts := guestOrcaResumeCommand.FindStringSubmatch(args[5])
		in.Operation, in.Worktree = "resume", strings.TrimPrefix(args[3], "id:")
		if len(parts) != 4 || !externalSessionText(in.Worktree, 1200) {
			return in, false
		}
		in.Subject, in.Account, in.Thread = parts[1], parts[2], parts[3]
	default:
		return in, false
	}
	return in, true
}

func (s *Server) guestOrcaClient(ctx context.Context, watch *externalSessionWatch) (*localorca.Client, error) {
	in := guestOrcaOp{Operation: "probe", Runtime: guestOrcaRuntime(watch.Connection), RuntimeID: watch.RuntimeID, Workspace: watch.Workspace}
	var reply guestOrcaReply
	if err := s.runAgentRuntime(ctx, "orca-guest-restore", in, &reply); err != nil {
		return nil, err
	}
	if !externalScopeID.MatchString(reply.RuntimeID) || watch.RuntimeID != "" && watch.RuntimeID != reply.RuntimeID {
		return nil, errors.New("原客机 Orca 未运行，或其运行实例身份已变化")
	}
	watch.RuntimeID = reply.RuntimeID
	scope := *watch
	return &localorca.Client{
		Install: localorca.Installation{Installed: true, CLIPath: "guest-orca"},
		Run: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			in, ok := parseGuestOrcaCLI(scope, args)
			if !ok {
				return nil, errors.New("客机 Orca 不支持此恢复操作")
			}
			ctx, finish, err := s.guestOrcaContext(ctx)
			if err != nil {
				return nil, err
			}
			defer finish()
			if in.Operation == "resume" {
				if !s.lockProjectOperation(ctx) {
					return nil, errTerminalBusy
				}
				defer s.op.Unlock()
				verify, release, err := s.guestOrcaContext(ctx)
				if err != nil {
					return nil, err
				}
				defer release()
				ctx = verify
			}
			var reply guestOrcaReply
			if err := s.runAgentRuntime(ctx, "orca-guest-restore", in, &reply); err != nil {
				return nil, err
			}
			if reply.RuntimeID != scope.RuntimeID || len(reply.Result) == 0 {
				return nil, errors.New("原客机 Orca 运行实例或命令结果无法确认")
			}
			return reply.Result, nil
		},
	}, nil
}

type guestOrcaWatchView struct {
	Runtime   string `json:"runtime"`
	Workspace string `json:"workspace"`
	RuntimeID string `json:"runtime_id"`
	Captured  int    `json:"captured"`
	Verified  int    `json:"verified"`
	Closed    int    `json:"closed"`
	Restored  int    `json:"restored"`
}

func (s *Server) guestOrcaWatches(instance string) ([]externalSessionWatch, []guestOrcaWatchView, error) {
	s.externalSessions.mu.Lock()
	data, err := s.loadExternalSessionJournal()
	s.externalSessions.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	var watches []externalSessionWatch
	views := make([]guestOrcaWatchView, 0)
	for _, watch := range data.Orca {
		runtime := guestOrcaRuntime(watch.Connection)
		if !watch.Guest || watch.CageInstance != instance || runtime == "" {
			continue
		}
		watches = append(watches, watch)
		view := guestOrcaWatchView{Runtime: runtime, Workspace: watch.Workspace, RuntimeID: watch.RuntimeID}
		for _, rec := range data.Records {
			if !rec.Guest || rec.CageInstance != instance || rec.Connection != watch.Connection ||
				rec.RuntimeID != watch.RuntimeID || rec.Workspace != watch.Workspace {
				continue
			}
			view.Captured++
			if rec.Closed {
				view.Closed++
			} else if rec.Native != nil && rec.Native.verified() {
				view.Verified++
				if rec.ResumedHandle != "" && rec.ResumeBoot == "" {
					view.Restored++
				}
			}
		}
		views = append(views, view)
	}
	return watches, views, nil
}

func (s *Server) restoreGuestOrcaSessions(parent context.Context) error {
	if !s.guestOrcaSupported() {
		return nil
	}
	ctx, finish, err := s.guestOrcaContext(parent)
	if err != nil {
		return nil // An unlock or ready event will requeue this pass.
	}
	defer finish()
	if s.busy() || !s.externalSessions.recovery.TryLock() {
		return errTerminalBusy
	}
	defer s.externalSessions.recovery.Unlock()
	instance, _ := s.externalSessionInstance()
	watches, _, err := s.guestOrcaWatches(instance)
	if err != nil || len(watches) == 0 {
		return err
	}
	s.watchExternalSessions()
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	var failures error
	for _, watch := range watches {
		if ctx.Err() != nil {
			break
		}
		client, err := s.guestOrcaClient(ctx, &watch)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		_, err = s.captureExternalOrcaWithClient(ctx, watch, false, client)
		if err == nil {
			err = s.reconcileExternalOrcaWithClient(ctx, watch, client)
		}
		failures = errors.Join(failures, err)
	}
	return failures
}

func (s *Server) guestOrcaRestoreHTTP(w http.ResponseWriter, r *http.Request) {
	_, phone := s.pairedDevice(r)
	if !s.guestOrcaSupported() || phone != nil {
		http.NotFound(w, r)
		return
	}
	instance, _ := s.externalSessionInstance()
	if r.Method == http.MethodGet {
		_, list, err := s.guestOrcaWatches(instance)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"workspaces": list})
		return
	}
	var in struct {
		Action    string `json:"action,omitempty"`
		Runtime   string `json:"runtime"`
		Workspace string `json:"workspace"`
		RuntimeID string `json:"runtime_id,omitempty"`
	}
	if !readJSON(w, r, &in, 4096) {
		return
	}
	connection := "guest-" + in.Runtime
	if guestOrcaRuntime(connection) == "" || !validGuestOrcaWorkspace(in.Workspace) {
		requestError(w, errors.New("请选择客机 Orca 实例和原工作区的绝对路径"))
		return
	}
	if !s.externalSessions.recovery.TryLock() {
		http.Error(w, "正在核对原 Orca 会话，请稍后重试", http.StatusConflict)
		return
	}
	defer s.externalSessions.recovery.Unlock()
	watch := externalSessionWatch{Guest: true, CageInstance: instance, Connection: connection, Workspace: in.Workspace}
	var err error
	switch in.Action {
	case "", "register":
		ctx, finish, accessErr := s.guestOrcaContext(r.Context())
		if accessErr != nil {
			err = accessErr
			break
		}
		defer finish()
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		var client *localorca.Client
		client, err = s.guestOrcaClient(ctx, &watch)
		if err == nil {
			_, err = s.captureExternalOrcaWithClient(ctx, watch, true, client)
		}
		if err == nil {
			s.watchExternalSessions()
			s.mu.Lock()
			s.queueWorkbenchRuntimeRestoreLocked(restoreRuntimeGuestOrca)
			s.mu.Unlock()
		}
	case "unregister":
		if !externalScopeID.MatchString(in.RuntimeID) {
			requestError(w, errors.New("请选择已登记的客机 Orca 实例"))
			return
		}
		watch.RuntimeID = in.RuntimeID
		err = s.removeGuestOrcaWatch(watch)
	default:
		requestError(w, errors.New("未知的客机 Orca 恢复操作"))
		return
	}
	if err != nil {
		requestError(w, err)
		return
	}
	_, list, err := s.guestOrcaWatches(instance)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]any{"workspaces": list})
}

func (s *Server) removeGuestOrcaWatch(watch externalSessionWatch) error {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return err
	}
	prior := len(data.Orca)
	data.Orca = slices.DeleteFunc(data.Orca, func(existing externalSessionWatch) bool { return existing == watch })
	if len(data.Orca) == prior {
		return nil
	}
	return s.saveExternalSessionJournal(data)
}
