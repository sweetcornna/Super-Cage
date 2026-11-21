package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
	"github.com/sylvinus/agent-vm/internal/cage"
)

// appUpdateGuestProbe is the one guest read an update decision makes. It
// runs only when a decision or a confirmation list needs it, never on a
// timer. For every session leader started by a workbench terminal
// (CAGE_WORKBENCH=1) it reports the terminal's foreground command when that
// is not the shell itself (or the leader itself when it is not a shell, as
// for an exec'd Codex session); it lists Claude and Codex processes outside those
// sessions, and whether Claude Desktop and Claude Science are running.
const appUpdateGuestProbe = `python3 -I - <<'CAGE_UPDATE_PROBE'
import json, os, subprocess
out = {"shells": [], "agents": [], "services": {}}
SHELLS = ("bash", "sh", "dash", "zsh", "fish")
def fields(pid):
    with open("/proc/%d/stat" % pid) as f:
        text = f.read()
    rest = text[text.rindex(")") + 2:].split()
    return int(rest[2]), int(rest[3]), int(rest[4]), int(rest[5])
def program(pid):
    try:
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            args = [a.decode("utf-8", "replace") for a in f.read().split(b"\0") if a]
    except OSError:
        return ""
    if not args:
        return ""
    name = os.path.basename(args[0]).lstrip("-")
    if name in ("node", "nodejs") and len(args) > 1:
        name = "claude" if "claude-code" in args[1] else os.path.basename(args[1])
    return name[:64]
procs = {}
for entry in os.listdir("/proc"):
    if entry.isdigit():
        pid = int(entry)
        try:
            procs[pid] = fields(pid)
        except (OSError, ValueError, IndexError):
            pass
sessions = set()
for pid, (pgrp, sid, tty, tpgid) in procs.items():
    if pid != sid or tty == 0:
        continue
    try:
        with open("/proc/%d/environ" % pid, "rb") as f:
            env = f.read().split(b"\0")
    except OSError:
        continue
    if b"CAGE_WORKBENCH=1" not in env:
        continue
    sessions.add(sid)
    if tpgid > 0 and tpgid != pgrp:
        foreground = program(tpgid) or "?"
    else:
        # A terminal whose leader is the program itself (exec codex, a
        # non-interactive script) has no separate foreground group.
        leader = program(pid)
        foreground = "" if leader in SHELLS else (leader or "?")
    out["shells"].append({"sid": sid, "foreground": foreground})
for pid, (pgrp, sid, tty, tpgid) in procs.items():
    if sid not in sessions:
        name = program(pid)
        if name in ("claude", "codex"):
            out["agents"].append(name)
for unit in ("cage-claude-desktop.service", "` + cage.ClaudeScienceUnit + `"):
    out["services"][unit] = subprocess.run(["systemctl", "is-active", "--quiet", unit]).returncode == 0
print(json.dumps(out))
CAGE_UPDATE_PROBE`

// appUpdateGuestTerminalLimit bounds the terminal blockers one guest answer
// can produce, however many shells it reports.
const appUpdateGuestTerminalLimit = 16

type appUpdateGuestReport struct {
	Shells []struct {
		SID        int    `json:"sid"`
		Foreground string `json:"foreground"`
	} `json:"shells"`
	Agents   []string        `json:"agents"`
	Services map[string]bool `json:"services"`
}

// appUpdateGuestBlockers turns the probe's answer into blockers. live is the
// number of workbench terminals the host still holds: a terminal the guest
// does not report cannot be proved idle, so it counts as unknown.
func appUpdateGuestBlockers(data []byte, live int) (policy.Reading, error) {
	var report appUpdateGuestReport
	if err := json.Unmarshal(data, &report); err != nil || report.Services == nil {
		return policy.Reading{}, errors.New("guest update probe answered invalid JSON")
	}
	// The guest's answer is data: it never chooses a blocker id or how many
	// there are, and its text reaches the dialog as short plain text. Ids
	// are made here from a terminal's position (by session id) and its
	// command, so confirming one command never covers another that took its
	// place. At most one blocker per terminal the host holds.
	var reading policy.Reading
	shells := report.Shells
	sort.Slice(shells, func(i, j int) bool { return shells[i].SID < shells[j].SID })
	busy := 0
	for position, shell := range shells {
		if shell.Foreground == "" {
			continue
		}
		if busy == live || busy == appUpdateGuestTerminalLimit {
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "unknown:terminals", Kind: policy.KindUnknown, Unknown: true, Interruptible: true, Label: "无法确认终端里是否还有命令在运行"})
			break
		}
		busy++
		command := plainLabel(shell.Foreground, 24)
		if command == "?" || command == "" {
			command = "未知程序"
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", position, command)))
		reading.Blockers = append(reading.Blockers, policy.Blocker{
			ID: "terminal-command:" + hex.EncodeToString(digest[:6]), Kind: policy.KindTerminal, Interruptible: true,
			Label: "终端里还有命令在运行（" + command + "）",
		})
	}
	if live > len(report.Shells) && (len(reading.Blockers) == 0 || reading.Blockers[len(reading.Blockers)-1].ID != "unknown:terminals") {
		reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "unknown:terminals", Kind: policy.KindUnknown, Unknown: true, Interruptible: true, Label: "无法确认终端里是否还有命令在运行"})
	}
	agents := map[string]string{"claude": "Claude", "codex": "Codex"}
	seen := map[string]bool{}
	for _, name := range report.Agents {
		seen[name] = true
	}
	for _, name := range []string{"claude", "codex"} {
		if seen[name] {
			reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "agent:" + name, Kind: policy.KindAgent, Interruptible: true,
				Label: "工作台终端之外有 " + agents[name] + " 进程在运行"})
		}
	}
	if report.Services["cage-claude-desktop.service"] {
		reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "app:claude-desktop", Kind: policy.KindApp, Interruptible: true, Label: "Claude Desktop 正在运行"})
	}
	if report.Services[cage.ClaudeScienceUnit] {
		reading.Blockers = append(reading.Blockers, policy.Blocker{ID: "app:claude-science", Kind: policy.KindApp, Interruptible: true, Label: "Claude Science 正在运行"})
	}
	return reading, nil
}

// readAppUpdateGuest makes the decision-time guest read.
func (s *Server) readAppUpdateGuest(ctx context.Context) (policy.Reading, error) {
	live := s.terminals.liveCount()
	if !s.vmRunning() {
		if live != 0 {
			return policy.Reading{Blockers: []policy.Blocker{{ID: "unknown:terminals", Kind: policy.KindUnknown, Unknown: true, Interruptible: true, Label: "无法确认终端里是否还有命令在运行"}}}, nil
		}
		return policy.Reading{}, nil
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		// No native guest read yet: a running native guest cannot be proved idle.
		return policy.Reading{Blockers: []policy.Blocker{{ID: "unknown:native-guest", Kind: policy.KindUnknown, Unknown: true, Interruptible: true, Label: "无法确认客机里的终端和应用是否空闲"}}}, nil
	}
	data, err := s.guestQuick(ctx, appUpdateGuestProbe)
	if err != nil {
		return policy.Reading{}, err
	}
	return appUpdateGuestBlockers(data, live)
}
