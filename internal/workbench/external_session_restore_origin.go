package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

const externalSessionOriginRoute = "/api/external-session-restore/origin"

type externalOriginProbe struct {
	Handle     string                `json:"terminal_handle,omitempty"`
	Provider   string                `json:"provider"`
	Workspace  string                `json:"working_directory"`
	TabID      string                `json:"tab_id,omitempty"`
	WorktreeID string                `json:"worktree_id,omitempty"`
	ClaimBoot  string                `json:"claim_boot,omitempty"`
	BootOnly   bool                  `json:"boot_only,omitempty"`
	Original   *externalNativeOrigin `json:"original,omitempty"`
}

type externalNativeOrigin struct {
	State   string `json:"state"`
	Thread  string `json:"thread_id,omitempty"`
	Account string `json:"account_id,omitempty"`
	Subject string `json:"subject,omitempty"`
	BootID  string `json:"boot_id,omitempty"`
}

var externalThreadID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var externalSubject = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (o externalNativeOrigin) verified() bool {
	return o.State == "found" && externalThreadID.MatchString(o.Thread) &&
		validClaudeID(o.Account) && externalSubject.MatchString(o.Subject)
}

func (o externalNativeOrigin) sameIdentity(original externalNativeOrigin) bool {
	return o.verified() && original.verified() && o.Thread == original.Thread &&
		o.Subject == original.Subject && o.Account == original.Account
}

func (s *Server) externalOriginHTTP(w http.ResponseWriter, r *http.Request) {
	if _, phone := s.pairedDevice(r); phone != nil {
		http.NotFound(w, r)
		return
	}
	var in externalOriginProbe
	if !readJSON(w, r, &in) {
		return
	}
	result, err := s.verifyExternalOrigin(r.Context(), in)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, result)
}

func (s *Server) verifyExternalOrigin(ctx context.Context, in externalOriginProbe) (externalNativeOrigin, error) {
	unavailable := externalNativeOrigin{State: "unavailable"}
	if !s.codexGuestSupported() || in.Provider != "codex" || !externalSessionWorkspace(in.Workspace) {
		return externalNativeOrigin{State: "unsupported"}, nil
	}
	if in.BootOnly {
		if in.Original != nil || in.Handle != "" || in.TabID != "" || in.WorktreeID != "" || in.ClaimBoot != "" {
			return unavailable, nil
		}
		var observed externalNativeOrigin
		err := s.runAgentRuntime(ctx, "external-session-origin", in, &observed)
		if err != nil || observed.State != "boot" || !externalThreadID.MatchString(observed.BootID) {
			return unavailable, err
		}
		return externalNativeOrigin{State: "boot", BootID: observed.BootID}, nil
	}
	recheckAccount := in.Original != nil
	if recheckAccount {
		if !in.Original.verified() || in.ClaimBoot != "" && !externalThreadID.MatchString(in.ClaimBoot) {
			return unavailable, nil
		}
	} else {
		if in.ClaimBoot != "" || !externalTerminalHandle.MatchString(in.Handle) || !externalSessionText(in.TabID, 128) || !externalSessionText(in.WorktreeID, 1200) {
			return unavailable, nil
		}
		var observed externalNativeOrigin
		err := s.runAgentRuntime(ctx, "external-session-origin", in, &observed)
		if err != nil || !observed.verified() || !externalThreadID.MatchString(observed.BootID) {
			return unavailable, err
		}
		in.Original = &observed
	}
	s.codexAccounts.mu.Lock()
	list, err := s.loadCodexAccounts()
	s.codexAccounts.mu.Unlock()
	if err != nil {
		return unavailable, err
	}
	if account := list.find(in.Original.Account); account == nil || account.Status != "ready" ||
		account.subject() != in.Original.Subject || s.codexLoginActive(in.Original.Account) {
		return unavailable, nil
	}
	if !recheckAccount {
		return *in.Original, nil
	}
	var boot externalNativeOrigin
	err = s.runAgentRuntime(ctx, "external-session-origin", externalOriginProbe{
		Provider: in.Provider, Workspace: in.Workspace, BootOnly: true,
	}, &boot)
	if err != nil || boot.State != "boot" || !externalThreadID.MatchString(boot.BootID) {
		return unavailable, err
	}
	if in.ClaimBoot != "" {
		if in.ClaimBoot != boot.BootID {
			return unavailable, nil
		}
		taken, err := s.claimExternalGuestLaunch(ctx, in.Workspace, in.Original.Thread, boot.BootID)
		if err != nil {
			return unavailable, err
		}
		state := "already_claimed"
		if taken {
			state = "claimed"
		}
		return externalNativeOrigin{State: state, BootID: boot.BootID}, nil
	}
	origin := *in.Original
	origin.BootID = boot.BootID
	return origin, nil
}

// The two supported controllers can use different Orca connection selectors
// for the same guest. Claim by native thread, workspace and kernel boot in
// their shared guest so concurrent requests can create only one terminal.
const externalOrcaGuestClaim = `
import hashlib, os, stat, sys
from pathlib import Path
workspace, thread, boot = sys.argv[1:]
if Path("/proc/sys/kernel/random/boot_id").read_text().strip() != boot:
    raise SystemExit(1)
root = Path.home() / ".local" / "state" / "cage" / "external-orca-resume"
root.mkdir(mode=0o700, parents=True, exist_ok=True)
directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
try:
    info = os.fstat(directory)
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise SystemExit(1)
    key = hashlib.sha256((boot + "\0" + workspace + "\0" + thread).encode()).hexdigest()
    try:
        marker = os.open(key, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
    except FileExistsError:
        print("already_claimed")
    else:
        try:
            os.fsync(marker)
            os.fsync(directory)
        finally:
            os.close(marker)
        print("claimed")
finally:
    os.close(directory)
`

func (s *Server) claimExternalGuestLaunch(ctx context.Context, workspace, thread, boot string) (bool, error) {
	if run := s.externalSessions.runGuestClaim; run != nil {
		return run(ctx, workspace, thread, boot)
	}
	command := "exec " + shellescape.QuoteCommand([]string{
		s.guestPythonBinary(), "-I", "-c", externalOrcaGuestClaim, workspace, thread, boot,
	})
	result, err := s.guest(ctx, command)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(result)) {
	case "claimed":
		return true, nil
	case "already_claimed":
		return false, nil
	}
	return false, errors.New("原 Orca 恢复凭据未返回可验证的领取状态")
}

// The selected Cage connection is separate from the saved Orca connection.
// Only the former may read its own guest's process and account metadata.
func (s *Server) externalOriginOnCage(ctx context.Context, instance string, in externalOriginProbe) (externalNativeOrigin, error) {
	owner, generation := s.externalSessionInstance()
	if owner != instance {
		return externalNativeOrigin{}, errors.New("原 Super Cage 连接已更改")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if s.remote == nil || s.remote.desktop == nil {
		return s.verifyExternalOrigin(ctx, in)
	}
	session, _, err := s.remote.desktop.acquire(generation)
	if err != nil {
		return externalNativeOrigin{}, err
	}
	if session.Ref.InstanceID != instance {
		return externalNativeOrigin{}, errors.New("原 Super Cage 服务的身份已变化")
	}
	stop := context.AfterFunc(session.Context, cancel)
	defer stop()
	data, err := json.Marshal(in)
	if err != nil {
		return externalNativeOrigin{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.Origin+externalSessionOriginRoute, bytes.NewReader(data))
	if err != nil {
		return externalNativeOrigin{}, err
	}
	req.Header.Set("Origin", session.Origin)
	req.Header.Set("Content-Type", "application/json")
	response, err := session.Client.Do(req)
	if err != nil {
		return externalNativeOrigin{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return externalNativeOrigin{}, errors.New("原 Super Cage 环境未确认外部会话")
	}
	var out externalNativeOrigin
	err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&out)
	if owner, currentGeneration := s.externalSessionInstance(); owner != instance || currentGeneration != generation {
		return externalNativeOrigin{}, errors.New("原 Super Cage 连接已更改")
	}
	return out, err
}
