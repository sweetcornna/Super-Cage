package workbench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/herdrlink"
	"github.com/sylvinus/agent-vm/internal/localterm"
)

// Herdr through the entry (design 5.4, 5.5; build step 3). The owner's
// Herdr core reaches its machine over one exec channel without a pty that
// runs Herdr's connect line. The endpoint recognises that line exactly and
// runs the environment's core in its place, through the guest's run script
// ("run herdr <session>"), with the Herdr filter (internal/herdrlink) between
// the two: the owner's host name never reaches the environment, the
// environment's call-backs into the owner's terminals are dropped, and every
// terminal byte goes through cage attach's escape filter.
//
// Herdr's install and update lines ("hc machine add" copies the core over an
// exec channel, then restarts it) run with the same Herdr environment, so the
// core they start is the one the link reaches. Herdr's TUI (herdr --remote)
// is refused: its bridge carries its own protocol, which this filter does not
// read (design R4).

var (
	// entryHerdrIdleAfter is how long a core waits after its last line
	// before the idle check (design 5.5); a variable for tests.
	entryHerdrIdleAfter = 10 * time.Minute
	// entryHerdrShortLine: a line the environment ends sooner than this
	// delays the device's next one (design 5.4, C2).
	entryHerdrShortLine  = 5 * time.Second
	entryHerdrBackoffMax = 30 * time.Second
)

const (
	// entryHerdrStderrLimit bounds what the environment may print to the
	// owner's Herdr through the link's error output.
	entryHerdrStderrLimit  = 16 << 10
	entryHerdrTUIRefusal   = "Super Cage：Herdr 的终端界面（herdr --remote）还不能经过 Super Cage 的过滤。请用 Herdr 应用添加这个环境，或用 ssh -t 登录后在环境里运行 herdr。"
	entryHerdrDriftRefusal = "Super Cage：这个版本的 Herdr 发来的连接命令和 Super Cage 认识的不同，为安全起见没有连接。请更新 Super Cage。"
)

// entryHerdrConnectLine is Herdr's connect line (herdr-core 0.1.21
// machines.rs pipe(), Unix), with %s for "HC_SESSION=<name> " or nothing.
const entryHerdrConnectLine = "sh -c 'p=$(command -v herdr-core) || p=$HOME/.local/bin/herdr-core; [ -x \"$p\" ] || { echo \"herdr-core is not installed here\" >&2; exit 127; }; %sexec \"$p\" connect'"

var (
	// entryHerdrSessionName is Herdr's machine session name (addr.rs
	// check_name).
	entryHerdrSessionName = regexp.MustCompile("^[a-z0-9][a-z0-9._-]{0,31}$")
	entryHerdrLinePattern = regexp.MustCompile("^" + strings.Replace(regexp.QuoteMeta(entryHerdrConnectLine), "%s", "(?:HC_SESSION=([a-z0-9][a-z0-9._-]{0,31}) )?", 1) + "$")
	// entryHerdrInstallPattern: the install and update of herdr-core
	// (machines.rs install(): the copy, then "server restart").
	entryHerdrInstallPattern = regexp.MustCompile("herdr-core\"? server restart|--hc-install-bundle")
	// entryHerdrBridgePattern: Herdr TUI's stdio bridge (herdr
	// src/remote/attach.rs bridge_command).
	entryHerdrBridgePattern = regexp.MustCompile("(^|[^A-Za-z0-9_-])remote-client-bridge([^A-Za-z0-9_-]|$)")
)

type entryHerdrStep int

const (
	entryHerdrNone    entryHerdrStep = iota
	entryHerdrConnect                // a machine link: filtered
	entryHerdrInstall                // install or update: Herdr's environment
	entryHerdrTUI                    // herdr --remote's bridge: refused
	entryHerdrDrift                  // a connect line Super Cage does not know: refused
)

// entryHerdrClassify says what an exec line without a pty is to Herdr.
func entryHerdrClassify(command string) (entryHerdrStep, string) {
	if match := entryHerdrLinePattern.FindStringSubmatch(command); match != nil {
		session := match[1]
		if session == "" {
			session = "default"
		}
		return entryHerdrConnect, session
	}
	switch {
	case entryHerdrBridgePattern.MatchString(command) && strings.Contains(command, "herdr"):
		return entryHerdrTUI, ""
	case entryHerdrLine(command):
		return entryHerdrDrift, ""
	case strings.Contains(command, "herdr-core") && entryHerdrInstallPattern.MatchString(command):
		return entryHerdrInstall, ""
	}
	return entryHerdrNone, ""
}

// entryHerdrState is the endpoint's memory of Herdr lines: the per-device
// delay after lines the environment ended at once.
type entryHerdrState struct {
	mu    sync.Mutex
	delay map[string]entryHerdrDelay
}

type entryHerdrDelay struct {
	until time.Time
	next  time.Duration
}

// wait is how long the device must still wait before its next line.
func (h *entryHerdrState) wait(device string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Until(h.delay[device].until)
}

// ended records how a line ended: one the environment ended within
// entryHerdrShortLine doubles the device's delay (1, 2, 4 … 30 s); a line
// the owner ended, or one that lasted, clears it. The owner's own hang-ups
// (hc machine add probes, then hangs up) never count.
func (h *entryHerdrState) ended(device string, lasted time.Duration, ownerEnded bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.delay == nil {
		h.delay = map[string]entryHerdrDelay{}
	}
	if ownerEnded || lasted >= entryHerdrShortLine {
		delete(h.delay, device)
		return
	}
	next := h.delay[device].next * 2
	if next == 0 {
		next = time.Second
	}
	next = min(next, entryHerdrBackoffMax)
	h.delay[device] = entryHerdrDelay{until: time.Now().Add(next), next: next}
}

// entryHerdrLink serves one Herdr machine link.
func (s *Server) entryHerdrLink(ctx context.Context, conn *entryConn, channel ssh.Channel, session string) {
	conn.herdrs.Add(1)
	defer conn.herdrs.Add(-1)
	status := 255
	defer func() {
		entryExitStatus(channel, status)
		_ = channel.Close()
	}()
	if wait := s.entry.herdr.wait(conn.device); wait > 0 {
		_, _ = fmt.Fprintf(channel.Stderr(), "Super Cage：环境刚刚断开了 Herdr 的连接，%d 秒后再连接\n", int(wait.Seconds()+0.999))
		return
	}
	if err := s.entryEnsureWrapper(ctx); err != nil {
		_, _ = io.WriteString(channel.Stderr(), "Super Cage："+err.Error()+"\n")
		return
	}
	settings := localterm.DefaultSettings()
	if paths, err := s.entryPaths(); err == nil {
		var loaded error
		if settings, loaded = localterm.LoadSettings(paths.ComputerDir); loaded != nil {
			// An unreadable choice allows nothing.
			settings.ClipboardWrite = false
		}
	}
	hopCtx, stopHop := context.WithCancel(ctx)
	defer stopHop()
	cmd := s.sshCommand(hopCtx, "-T", s.sshTarget(), "--", s.entryRunScript("herdr", session))
	cmd.WaitDelay = entryHopWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		_, _ = io.WriteString(channel.Stderr(), "Super Cage：无法连接环境\n")
		return
	}
	started := time.Now()
	errorsDone := make(chan struct{})
	go func() {
		defer close(errorsDone)
		_, _ = io.Copy(&entryPlainWriter{w: channel.Stderr(), left: entryHerdrStderrLimit}, stderr)
	}()
	counts := map[string]int{}
	var countsMu sync.Mutex
	result, linkErr := herdrlink.Run(herdrlink.Options{
		ClipboardWrite: settings.ClipboardWrite,
		Window:         attach.ClipboardWindow,
		Count: func(event string) {
			countsMu.Lock()
			counts[event]++
			countsMu.Unlock()
		},
	}, channel, channel, stdout, stdin)
	if linkErr != nil {
		// A broken bound ends the link at once; the hop goes with it.
		stopHop()
	}
	_ = stdin.Close()
	<-errorsDone
	err = cmd.Wait()
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
		status = 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		status = exit.ExitCode()
	}
	lasted := time.Since(started)
	if ctx.Err() == nil {
		s.entry.herdr.ended(conn.device, lasted, result.OwnerEnded)
	}
	if result.Framed {
		s.entryHerdrSeen()
	}
	countsMu.Lock()
	summary := entryHerdrSummary(counts)
	countsMu.Unlock()
	diagnostics.Log("info", "entry", fmt.Sprintf("Herdr line of device %s ended after %s (framed %v, owner ended %v, error %v)%s", entryShortID(conn.device), lasted.Round(time.Second), result.Framed, result.OwnerEnded, linkErr != nil, summary))
	// Even a probe's connect starts the core; with none installed there
	// is nothing to stop.
	if status != 127 {
		s.entryHerdrIdle(session)
	}
}

func entryHerdrSummary(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(counts))
	for kind, n := range counts {
		// The log would take "terminal.send" for a host name and hide it.
		parts = append(parts, strings.ReplaceAll(kind, ".", "_")+"="+strconv.Itoa(n))
	}
	slices.Sort(parts)
	return "; " + strings.Join(parts, ", ")
}

// entryHerdrInstallSessionPattern finds the session an install restarts
// (machines.rs install(): "HC_SESSION=<name> " before the restart).
var entryHerdrInstallSessionPattern = regexp.MustCompile("HC_SESSION=([a-z0-9][a-z0-9._-]{0,31}) ")

func entryHerdrInstallSession(command string) string {
	if match := entryHerdrInstallSessionPattern.FindStringSubmatch(command); match != nil {
		return match[1]
	}
	return "default"
}

// entryRunScript is the guest command of a hop: the run script with mode
// and arguments, behind a bash that reads no profile (entryRunHop).
func (s *Server) entryRunScript(mode string, args ...string) string {
	run := "exec " + shellescape.Quote(s.entryRunPath()) + " " + mode
	for _, arg := range args {
		run += " " + shellescape.Quote(arg)
	}
	return "exec /bin/bash --noprofile --norc -c " + shellescape.Quote(run)
}

// entryHerdrIdle arms the guest's idle check for a Herdr session: when the
// timer fires and no line is open and no terminal lives in that core, the
// core stops. Nothing runs while it waits (a transient systemd timer).
func (s *Server) entryHerdrIdle(session string) {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing || !s.vmRunning() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), 15*time.Second)
		defer cancel()
		seconds := strconv.Itoa(max(1, int(entryHerdrIdleAfter/time.Second)))
		cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", s.entryRunScript("herdr-idle", session, seconds))
		cmd.WaitDelay = entryHopWaitDelay
		if out, err := cmd.CombinedOutput(); err != nil {
			diagnostics.Log("warn", "entry", "arming Herdr's idle stop: "+strings.TrimSpace(string(out)))
		}
	}()
}

// entryHerdrSeen records that a Herdr core on this computer added this
// environment as a machine: the page then shows Herdr as set up.
func (s *Server) entryHerdrSeen() {
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	st, err := entryLoadState(s.profile)
	if err != nil || !st.Enabled {
		return
	}
	now := time.Now().UTC()
	st.HerdrSeen = &now
	_ = entrySaveState(s.profile, st)
}

// entryPlainWriter passes printable text and line breaks, at most left
// bytes: what the environment writes to a Herdr link's error output reaches
// the owner's Herdr (and a terminal running hc) as plain words only.
type entryPlainWriter struct {
	w    io.Writer
	left int
}

func (p *entryPlainWriter) Write(b []byte) (int, error) {
	n := len(b)
	if p.left <= 0 {
		return n, nil
	}
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == '\n' || r == '\t' || r != utf8.RuneError && unicode.IsPrint(r) {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	if len(out) > p.left {
		out = out[:p.left]
	}
	p.left -= len(out)
	_, err := p.w.Write(out)
	return n, err
}

// entryHerdrView is the Herdr row of the page.
type entryHerdrView struct {
	// State is not_set_up (no Herdr core on this computer added this
	// environment yet), ready (added, not connected now) or connected.
	State   string "json:\"state\""
	Command string "json:\"command\""
	Lines   int    "json:\"lines\""
}

// entryHerdrCommand is the one step that adds this environment to Herdr on
// this computer: hc machine add with the Host the entry wrote, or, without
// the Include in ~/.ssh/config, with Herdr's --via and Super Cage's file.
func entryHerdrCommand(alias string, included bool, clientFile string) string {
	if included {
		return "hc machine add " + alias
	}
	return "hc machine add --via " + shellescape.Quote("ssh -F "+shellescape.Quote(clientFile)+" -T "+alias) + " --name " + alias
}
