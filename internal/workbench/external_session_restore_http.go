package workbench

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const externalSessionRoute = "/api/external-session-restore"
const externalSessionClientPath = "external-session-restore"

type externalSessionRequest struct {
	Action       string `json:"action"`
	ID           string `json:"id,omitempty"`
	Application  string `json:"application,omitempty"`
	Connection   string `json:"connection,omitempty"`
	Workspace    string `json:"workspace,omitempty"`
	Continuation string `json:"continuation_id,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Account      string `json:"account,omitempty"`
}

func (s *Server) registerExternalSessionRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET "+externalSessionRoute, s.externalSessionRestoreHTTP)
	mux.HandleFunc("POST "+externalSessionRoute, s.externalSessionRestoreHTTP)
	mux.HandleFunc("POST "+externalSessionOriginRoute, s.externalOriginHTTP)
}

func (s *Server) externalSessionInstance() (instance, generation string) {
	if s.remote != nil && s.remote.desktop != nil {
		current := s.remote.context()
		if current.ConnectionID != "" && current.Mode != "none" {
			return current.InstanceID, current.ConnectionID
		}
		return "", ""
	}
	if s.clientOnly {
		return "", ""
	}
	if id := s.instanceID; id != "" {
		return id, "local"
	}
	return "local", "local"
}

func (s *Server) externalSessionLocalScope(ctx context.Context) (string, error) {
	instance, generation := s.externalSessionInstance()
	if instance == "" {
		return "", errors.New("请先连接原 Super Cage 环境")
	}
	if s.remote != nil && s.remote.desktop != nil {
		unlocked, err := s.localTerminalUnlocked(ctx, generation)
		currentInstance, currentGeneration := s.externalSessionInstance()
		if err != nil || !unlocked || instance != currentInstance || generation != currentGeneration {
			return "", errors.New("无法确认原工作环境已连接并解锁")
		}
	} else if _, unlocked := s.account.access(); !unlocked {
		return "", errors.New("请先解锁 Super Cage")
	}
	return instance, nil
}

func (s *Server) externalSessionRestoreHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) || r.Header.Get("X-Cage-Remote-Client") == "1" {
		http.Error(w, "原应用会话恢复只能从本机 Super Cage 桌面窗口操作", http.StatusForbidden)
		return
	}
	_, generation := s.externalSessionInstance()
	if requested := requestConnection(r); requested != "" && requested != generation {
		http.Error(w, "工作环境连接已更改，请重新打开页面", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	instance, err := s.externalSessionLocalScope(ctx)
	if err != nil {
		requestError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		records, err := s.externalSessionRecords(instance)
		if err != nil {
			requestError(w, err)
			return
		}
		if s.hasExternalOrcaWatches(instance) {
			s.watchExternalSessions()
		}
		reply := map[string]any{"records": records}
		if r.URL.Query().Get("connections") == "1" {
			reply["orca_connections"] = s.externalOrcaChoices(ctx)
		}
		jsonReply(w, reply)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "请求方式无效", http.StatusMethodNotAllowed)
		return
	}
	var in externalSessionRequest
	if !readJSON(w, r, &in) {
		return
	}
	switch in.Action {
	case "watch_orca":
		if !externalSessionWorkspace(in.Workspace) || !externalScopeID.MatchString(in.Connection) {
			requestError(w, errors.New("请选择 Orca 中已有的连接和原工作区绝对路径"))
			return
		}
		count, err := s.captureExternalOrca(ctx, externalSessionWatch{
			CageInstance: instance, Connection: in.Connection, Workspace: in.Workspace,
		}, true)
		if err != nil {
			requestError(w, err)
			return
		}
		s.watchExternalSessions()
		jsonReply(w, map[string]any{"captured": count})
	case "record_deepseek":
		record, err := s.recordExternalDeepSeek(instance, in)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, record)
	case "close":
		if err := s.closeExternalSessionRecord(instance, in.ID); err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]bool{"closed": true})
	case "resume":
		result, err := s.recoverExternalSession(ctx, instance, in.ID, in.Account)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, result)
	default:
		requestError(w, errors.New("未知的原应用会话操作"))
	}
}

func (s *Server) externalOrcaChoices(ctx context.Context) []map[string]string {
	installation := s.detectLocalOrca()
	choices := make([]map[string]string, 0)
	if !installation.Installed || installation.CLIPath == "" {
		return choices
	}
	choices = append(choices, map[string]string{"id": "local", "name": "本机 Orca"})
	known, err := s.localOrcaClient(installation).ListEnvironments(ctx)
	if err == nil {
		for _, env := range known {
			if externalScopeID.MatchString(env.ID) && externalScopeID.MatchString(env.RuntimeID) && externalSessionText(env.Name, 100) {
				choices = append(choices, map[string]string{"id": env.ID, "name": env.Name})
			}
		}
	}
	return choices
}

func (s *Server) recordExternalDeepSeek(instance string, in externalSessionRequest) (externalSessionRecord, error) {
	account := strings.TrimSpace(in.Account)
	if (in.Application != "deepseek-desktop" && in.Application != "deepseek-tui") ||
		!externalScopeID.MatchString(in.Connection) || !externalSessionWorkspace(in.Workspace) ||
		!externalScopeID.MatchString(in.Continuation) || !externalSessionText(account, 100) {
		return externalSessionRecord{}, errors.New("需要原应用、连接、工作区、会话 ID 和不含凭据的账号标记")
	}
	if in.Application == "deepseek-tui" && !externalProfileID.MatchString(in.Profile) || in.Application == "deepseek-desktop" && in.Profile != "" {
		return externalSessionRecord{}, errors.New("请指定原 dsh TUI profile")
	}
	if in.Application == "deepseek-desktop" && in.Connection != "local" {
		return externalSessionRecord{}, errors.New("只能从运行原 DeepSeek Harness 桌面版的电脑登记桌面会话")
	}
	now := time.Now().Unix()
	record := externalSessionRecord{
		App: in.Application, CageInstance: instance, Connection: in.Connection, Workspace: in.Workspace,
		Continuation: in.Continuation, Account: account, Profile: in.Profile, Provider: "dsh", UpdatedAt: now,
	}
	record.ID = externalSessionKey(record)
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return externalSessionRecord{}, err
	}
	changed, err := updateExternalSessionRecord(&data, record, now)
	if err == nil && changed {
		err = s.saveExternalSessionJournal(data)
	}
	return record, err
}

func (s *Server) recoverExternalSession(ctx context.Context, instance, id, account string) (externalSessionResult, error) {
	s.externalSessions.recovery.Lock()
	defer s.externalSessions.recovery.Unlock()
	records, err := s.externalSessionRecords(instance)
	if err != nil {
		return externalSessionResult{}, err
	}
	if current, err := s.externalSessionLocalScope(ctx); err != nil || current != instance {
		return externalSessionResult{}, errors.New("原工作环境连接已改变")
	}
	for _, record := range records {
		if record.ID != id {
			continue
		}
		if record.App == "orca" {
			return s.recoverExternalOrca(ctx, record)
		}
		result, err := externalDshResume(record, strings.TrimSpace(account))
		if err != nil || result.State != "needs_desktop_open" {
			return result, err
		}
		if err := openExternalDeepSeek(ctx); err != nil {
			return externalSessionResult{}, errors.New("无法打开这台电脑的 DeepSeek Harness")
		}
		return externalSessionResult{
			State: "awaiting_client", Message: "已打开 DeepSeek Harness。应用会尝试载入上次选中的会话，请在原账号和工作区核对会话 ID。",
		}, nil
	}
	return externalSessionResult{}, errors.New("未找到可恢复的原应用会话")
}
