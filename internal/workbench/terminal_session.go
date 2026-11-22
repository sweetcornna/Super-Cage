package workbench

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

var errTerminalInputFull = errors.New("terminal input queue is full")

type terminalPTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
	Kill() error
}

type terminalSession struct {
	mu                    sync.Mutex
	id, title             string
	sharedID              string
	connectionID          string
	conversationEnded     bool
	workspace             string
	kind, claudeAccountID string
	loginGuestID          string
	loginLinkMu           sync.Mutex
	loginLinkCleanup      sync.Once
	claudeAccountSource   string
	codexAccountID        string
	codexAccountSource    string
	cmd                   *exec.Cmd
	file                  terminalPTY
	exitCode              int
	history               []byte
	written               uint64        // bytes ever appended to history
	changed               chan struct{} // closed and replaced on output and exit
	ended                 bool          // exitCode is final
	sequence              int
	closed                bool
	clients               map[*terminalViewer]bool
	done                  chan struct{}
	input                 []string      // admitted input not yet taken by the writer; guarded by mu
	inputBytes            int           // what input costs against terminalInputBudget
	inputReady            chan struct{} // wakes the writer after input is admitted
	inputSpace            chan struct{} // closed when the writer frees space for a waiting viewer; nil when none waits
	stopping              chan struct{}
	closeOnce             sync.Once
	cliStartupDone        chan struct{}
	sizing                terminalSizing // viewers and the size owner; guarded by mu
	sizeMu                sync.Mutex     // serialises deciding and applying a size
	resizer               terminalResizer
	// origin is "ssh" for a terminal the local SSH entry opened, with the
	// device's name and id; kept is the owner's "保留", which keeps it past
	// the connection (a lock sets it too). sshViewers counts the ssh
	// clients showing it. originPID names the guest file with the shell's
	// process id of an interactive ssh terminal (entryKeptBusy). released
	// is a kept one that turning the entry off made ordinary: no hang-up
	// ends it.
	origin, originDevice, originID string
	originPID                      string
	kept, released                 bool
	sshViewers                     int
	// lastInput is when input was last admitted, from any viewer.
	lastInput time.Time
}

func (t *terminalSession) ownsConversation() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && !t.conversationEnded
}

func newTerminalSession(file terminalPTY) *terminalSession {
	t := &terminalSession{file: file, exitCode: -1, clients: map[*terminalViewer]bool{}, done: make(chan struct{}), inputReady: make(chan struct{}, 1), stopping: make(chan struct{}), changed: make(chan struct{})}
	t.sizing.init(0, 0)
	return t
}

// exitStatus is the process's exit code after the session ends; -1 while it
// runs, after a signal, or when the status is unknown.
func (t *terminalSession) exitStatus() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode
}

func terminalExitCode(err error) int {
	var exit *exec.ExitError
	if err == nil {
		return 0
	}
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func (t *terminalSession) summary() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	closed := t.closed
	if t.done != nil {
		select {
		case <-t.done:
			closed = true
		default:
			closed = false
		}
	}
	summary := map[string]any{"id": t.id, "title": t.title, "closed": closed, "workspace": t.workspace, "kind": t.kind,
		"claude_account_id": t.claudeAccountID, "claude_account_source": t.claudeAccountSource, "cli_session_id": t.sharedID,
		"codex_account_id": t.codexAccountID, "codex_account_source": t.codexAccountSource,
		"cli_starting": t.cliStartupPendingLocked()}
	// Clients that know Codex sessions open a bound Codex terminal without the
	// Claude startup veil and account header.
	if t.kind == "codex" && t.sharedID != "" {
		summary["cli_session_provider"] = codexProvider
	}
	if t.origin != "" {
		summary["origin"] = map[string]any{"kind": t.origin, "device": t.originDevice, "kept": t.kept}
	}
	// How it ended, once it has: a Codex launch the guest refused ends with 75
	// and must not look like an ordinary close. Absent while it runs and when
	// the status is unknown (a signal). The exit code is final before done closes.
	if closed && t.exitCode >= 0 {
		summary["exit_code"] = t.exitCode
	}
	return summary
}

func (t *terminalSession) cliStartupPendingLocked() bool {
	if t.cliStartupDone == nil || t.closed {
		return false
	}
	select {
	case <-t.cliStartupDone:
		return false
	default:
		return true
	}
}

func (t *terminalSession) finishCLIStartup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cliStartupPendingLocked() {
		close(t.cliStartupDone)
	}
}

func (t *terminalSession) stopPTY() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		if t.sizing.grace != nil {
			t.sizing.grace.Stop()
			t.sizing.grace = nil
		}
		t.mu.Unlock()
		if t.stopping != nil {
			close(t.stopping)
		}
		if t.file != nil {
			// Close may wait inside ConPTY. Never hold the session mutex while
			// the reader needs it to drain the final output and finish.
			_ = t.file.Kill()
			_ = t.file.Close()
		}
	})
}

func (t *terminalSession) close() {
	t.stopPTY()
	if t.done != nil {
		<-t.done
	}
}

// terminalInputBudget bounds the input one session holds for its PTY writer
// while the guest program does not read: a stalled SSH window, a busy program,
// a paste into a shell that is still starting. The bound is in bytes, because
// what must survive is typing and pastes, however they were split into
// messages. A message is always admitted into an empty queue, so one frame
// larger than the budget still gets through.
var terminalInputBudget = 8 << 20

// terminalInputOverhead is what each queued message costs beyond its bytes, so
// a flood of tiny messages is bounded too.
const terminalInputOverhead = 64

// admitInputLocked queues input, or reports errTerminalInputFull. Caller holds
// the session lock; admission is sealed by closed under the same lock.
func (t *terminalSession) admitInputLocked(input string) error {
	if t.closed {
		return os.ErrClosed
	}
	if input == "" {
		return nil
	}
	cost := len(input) + terminalInputOverhead
	if t.inputBytes > 0 && t.inputBytes+cost > terminalInputBudget {
		return errTerminalInputFull
	}
	t.input = append(t.input, input)
	t.inputBytes += cost
	t.lastInput = time.Now()
	select {
	case t.inputReady <- struct{}{}:
	default:
	}
	return nil
}

// Input belongs to the session once admitted. The caller never waits inside a
// PTY write, so disconnecting or locking the account can drain its handler
// while a guest process is not reading input. When the queue is over its byte
// budget the call refuses at once instead of waiting: callers that hold the
// account lock or that answer a request (the phone) must not block. The
// terminal stream waits for space itself, see enqueueInputWait.
func (t *terminalSession) enqueueInput(ctx context.Context, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.admitInputLocked(input)
}

// enqueueInputWait is enqueueInput for a stream that can wait: while the
// queue is over its budget it keeps the message and retries as the writer
// makes room, so nothing is dropped and the caller stops reading its socket,
// which pushes back on the sender. It gives up when ctx ends, when gone
// closes (the viewer's sender has exited), or when the session stops.
func (t *terminalSession) enqueueInputWait(ctx context.Context, input string, gone <-chan struct{}) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		err := t.admitInputLocked(input)
		var space chan struct{}
		if errors.Is(err, errTerminalInputFull) {
			if t.inputSpace == nil {
				t.inputSpace = make(chan struct{})
			}
			space = t.inputSpace
		}
		t.mu.Unlock()
		if space == nil {
			return err
		}
		select {
		case <-space:
		case <-ctx.Done():
			return ctx.Err()
		case <-gone:
			return os.ErrClosed
		case <-t.stopping:
			return os.ErrClosed
		}
	}
}

// takeInput removes the oldest admitted input and wakes viewers waiting for
// room. It does not block.
func (t *terminalSession) takeInput() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.input) == 0 {
		return "", false
	}
	input := t.input[0]
	t.input[0] = ""
	if t.input = t.input[1:]; len(t.input) == 0 {
		t.input = nil
	}
	t.inputBytes -= len(input) + terminalInputOverhead
	t.releaseInputWaitersLocked()
	return input, true
}

func (t *terminalSession) releaseInputWaitersLocked() {
	if t.inputSpace != nil {
		close(t.inputSpace)
		t.inputSpace = nil
	}
}

func (t *terminalSession) writeInput() {
	for {
		select {
		case <-t.stopping:
			return
		default:
		}
		input, ok := t.takeInput()
		if !ok {
			select {
			case <-t.stopping:
				return
			case <-t.inputReady:
			}
			continue
		}
		if _, err := io.WriteString(t.file, input); err != nil {
			t.stopPTY()
			return
		}
	}
}

// terminalHistoryLimit is the output a session keeps for viewers that connect
// or resume later, and for the phone.
const terminalHistoryLimit = 512 << 10

// terminalViewerBudget bounds the live output queued for one viewer that is
// not keeping up. Past it the queue is dropped and the viewer is caught up
// from the kept history instead of being disconnected.
var terminalViewerBudget = 4 << 20

// terminalViewer is the live output queued for one attached stream. The PTY
// reader only appends under the session lock and never waits for a viewer.
type terminalViewer struct {
	pending  []byte        // queued output, contiguous from offset
	offset   uint64        // absolute output offset of pending[0]
	lagged   bool          // the queue overflowed; resynchronise from history
	closed   bool          // the session ended; nothing more is queued
	detached bool          // the viewer left
	wake     chan struct{} // signalled after every change
}

func (v *terminalViewer) signal() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

// push queues live output. Caller holds the session lock.
func (v *terminalViewer) push(chunk []byte) {
	if v.lagged {
		return
	}
	if len(v.pending)+len(chunk) > terminalViewerBudget {
		v.lagged, v.pending = true, nil
	} else {
		v.pending = append(v.pending, chunk...)
	}
	v.signal()
}

type terminalSubscription struct {
	// History is the output sent before live output. HistoryOffset is the
	// absolute offset of its first byte; HistoryReset reports that it does
	// not continue from the offset the viewer asked to resume after.
	History       []byte
	HistoryOffset uint64
	HistoryReset  bool
	CLIStartup    <-chan struct{}
	CLIStarting   bool
	session       *terminalSession
	viewer        *terminalViewer
}

// terminalBatch is the next output for one viewer.
type terminalBatch struct {
	Data   []byte
	Offset uint64 // absolute offset of Data[0]
	Resync bool   // the viewer fell behind; Data restarts it from history
	Reset  bool   // Data does not continue from the output sent before
	Done   bool   // nothing more will arrive
	Ended  bool   // the session ended
}

// subscribe attaches a viewer that receives the whole kept history first.
func (t *terminalSession) subscribe() *terminalSubscription { return t.subscribeFrom(nil) }

// subscribeFrom captures history and registers live output under the same
// lock. Every byte belongs to exactly one side of this history/live boundary.
// With after, history starts at that absolute offset while it is still kept;
// otherwise it is everything kept, marked as a reset.
func (t *terminalSession) subscribeFrom(after *uint64) *terminalSubscription {
	t.mu.Lock()
	defer t.mu.Unlock()
	viewer := &terminalViewer{offset: t.written, wake: make(chan struct{}, 1)}
	if t.closed {
		viewer.closed = true
	} else {
		t.clients[viewer] = true
	}
	subscription := &terminalSubscription{CLIStartup: t.cliStartupDone, CLIStarting: t.cliStartupPendingLocked(), session: t, viewer: viewer}
	switch {
	case t.kind == "claude-login":
		subscription.History, subscription.HistoryReset = []byte("请在 Super Cage 浏览器的临时登录页完成授权。若页面提供验证码，可返回此终端输入。\r\n"), true
	case after != nil:
		out := t.outputAfterLocked(*after, len(t.history))
		subscription.History, subscription.HistoryOffset, subscription.HistoryReset = out.Data, out.Next-uint64(len(out.Data)), out.Reset
	default:
		subscription.History, subscription.HistoryOffset, subscription.HistoryReset = append([]byte(nil), t.history...), t.oldestLocked(), true
	}
	return subscription
}

// next takes the viewer's next output, at most limit bytes, without waiting.
// A viewer whose queue overflowed restarts from the kept history at the first
// byte it has not received, or from the oldest kept byte when that is gone.
func (s *terminalSubscription) next(limit int) terminalBatch {
	t, viewer := s.session, s.viewer
	t.mu.Lock()
	defer t.mu.Unlock()
	if viewer.lagged {
		if t.kind == "claude-login" {
			// Login output is never kept, so it cannot be resent.
			return terminalBatch{Done: true}
		}
		out := t.outputAfterLocked(viewer.offset, len(t.history))
		viewer.lagged, viewer.pending, viewer.offset = false, nil, out.Next
		return terminalBatch{Data: out.Data, Offset: out.Next - uint64(len(out.Data)), Resync: true, Reset: out.Reset}
	}
	if len(viewer.pending) > 0 {
		n := min(len(viewer.pending), limit)
		batch := terminalBatch{Data: viewer.pending[:n:n], Offset: viewer.offset}
		viewer.offset += uint64(n)
		if viewer.pending = viewer.pending[n:]; len(viewer.pending) == 0 {
			viewer.pending = nil
		}
		return batch
	}
	return terminalBatch{Done: viewer.closed || viewer.detached, Ended: viewer.closed}
}

// Wake is signalled whenever next may return something new.
func (s *terminalSubscription) Wake() <-chan struct{} { return s.viewer.wake }

// Close detaches the viewer; the PTY and its process continue running.
func (s *terminalSubscription) Close() {
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	delete(s.session.clients, s.viewer)
	s.viewer.detached = true
	s.viewer.signal()
}

func (t *terminalSession) run() {
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); t.writeInput() }()
	t.read()
	// An SSH process can remain alive after its output side closes. Once no
	// output can be read, terminate and reap it before completing the session.
	t.stopPTY()
	waitErr := t.file.Wait()
	<-writerDone
	t.mu.Lock()
	// Admission is sealed under mu before stopping the writer. Release queued
	// input even when the closed terminal remains visible with its history.
	t.input, t.inputBytes = nil, 0
	t.releaseInputWaitersLocked()
	t.exitCode = terminalExitCode(waitErr)
	t.ended = true
	t.notifyLocked()
	for viewer := range t.clients {
		viewer.closed = true
		viewer.signal()
		delete(t.clients, viewer)
	}
	t.mu.Unlock()
	close(t.done)
}

func (t *terminalSession) read() {
	buffer := make([]byte, 16<<10)
	for {
		n, err := t.file.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			t.mu.Lock()
			t.sizing.noteOutput(chunk)
			if t.kind != "claude-login" {
				t.history = append(t.history, chunk...)
				t.written += uint64(len(chunk))
				if len(t.history) > terminalHistoryLimit {
					t.history = t.history[len(t.history)-terminalHistoryLimit:]
				}
				t.notifyLocked()
			}
			for viewer := range t.clients {
				viewer.push(chunk)
			}
			t.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
}

// notifyLocked wakes every phone reader waiting for output or the exit.
func (t *terminalSession) notifyLocked() {
	if t.changed != nil {
		close(t.changed)
	}
	t.changed = make(chan struct{})
}

// terminalOutput is the scrollback after a byte offset of the whole output.
type terminalOutput struct {
	Data     []byte
	Next     uint64
	Reset    bool
	Closed   bool
	ExitCode *int
	changed  <-chan struct{}
}

// outputAfter returns at most limit kept bytes after the absolute offset.
// An offset outside the kept scrollback restarts from its oldest byte.
func (t *terminalSession) outputAfter(after uint64, limit int) terminalOutput {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.changed == nil {
		t.changed = make(chan struct{})
	}
	return t.outputAfterLocked(after, limit)
}

// oldestLocked is the absolute offset of the first kept byte.
func (t *terminalSession) oldestLocked() uint64 {
	if t.written < uint64(len(t.history)) {
		return 0
	}
	return t.written - uint64(len(t.history))
}

// Caller holds t.mu.
func (t *terminalSession) outputAfterLocked(after uint64, limit int) terminalOutput {
	oldest := t.oldestLocked()
	out := terminalOutput{changed: t.changed}
	if after < oldest || after > t.written {
		after, out.Reset = oldest, true
	}
	start := int(after - oldest)
	end := min(len(t.history), start+limit)
	out.Data = make([]byte, end-start)
	copy(out.Data, t.history[start:end])
	out.Next = after + uint64(end-start)
	if t.ended && out.Next == t.written {
		out.Closed = true
		if t.exitCode >= 0 {
			code := t.exitCode
			out.ExitCode = &code
		}
	}
	return out
}
