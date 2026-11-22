package workbench

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTerminalOutputAfterOffsets(t *testing.T) {
	session := &terminalSession{history: []byte("abcdef"), written: 10}
	for _, tc := range []struct {
		after, next uint64
		limit       int
		data        string
		reset       bool
	}{
		{after: 2, next: 10, limit: 100, data: "abcdef", reset: true},
		{after: 11, next: 10, limit: 100, data: "abcdef", reset: true},
		{after: 6, next: 10, limit: 100, data: "cdef"},
		{after: 4, next: 6, limit: 2, data: "ab"},
		{after: 10, next: 10, limit: 100, data: ""},
	} {
		out := session.outputAfter(tc.after, tc.limit)
		if string(out.Data) != tc.data || out.Next != tc.next || out.Reset != tc.reset || out.Closed || out.Data == nil {
			t.Fatalf("after %d: %+v", tc.after, out)
		}
	}
	waiting := session.outputAfter(10, 100).changed
	session.mu.Lock()
	session.exitCode, session.ended = 3, true
	session.notifyLocked()
	session.mu.Unlock()
	select {
	case <-waiting:
	default:
		t.Fatal("exit did not wake a waiting reader")
	}
	if out := session.outputAfter(6, 2); out.Closed || out.Next != 8 {
		t.Fatalf("unread output reported as closed: %+v", out)
	}
	if out := session.outputAfter(10, 100); !out.Closed || out.ExitCode == nil || *out.ExitCode != 3 {
		t.Fatalf("exit not reported: %+v", out)
	}
}

type mobileTerminalReply struct {
	Data     []byte `json:"data"`
	Next     uint64 `json:"next"`
	Reset    bool   `json:"reset"`
	Closed   bool   `json:"closed"`
	ExitCode *int   `json:"exit_code"`
}

func readMobileTerminal(t *testing.T, s *Server, id string, after uint64, needle string) (string, mobileTerminalReply) {
	t.Helper()
	var text string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := request(s, "GET", "/api/mobile/terminals/"+id+"/output?after="+strconv.FormatUint(after, 10), "", "", true)
		if w.Code == http.StatusNoContent {
			continue
		}
		var reply mobileTerminalReply
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("output: %d %s", w.Code, w.Body.String())
		}
		text += string(reply.Data)
		after = reply.Next
		if strings.Contains(text, needle) || reply.Closed {
			return text, reply
		}
	}
	t.Fatalf("terminal did not produce %q: %q", needle, text)
	return "", mobileTerminalReply{}
}

func TestMobileTerminalReadWriteAndClose(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	previous := mobileTerminalHold
	mobileTerminalHold = 50 * time.Millisecond
	t.Cleanup(func() { mobileTerminalHold = previous })
	session, err := s.terminals.start(terminalChildCommand("echo"), "/project", "")
	if err != nil {
		t.Fatal(err)
	}
	login, err := s.terminals.start(terminalChildCommand("echo"), "/project", "codex-login")
	if err != nil {
		t.Fatal(err)
	}
	w := request(s, "GET", "/api/mobile/terminals", "", "", true)
	var list struct {
		Terminals []map[string]any `json:"terminals"`
		Kinds     []string         `json:"kinds"`
		Limit     int              `json:"limit"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(list.Terminals) != 1 || list.Terminals[0]["id"] != session.id || list.Terminals[0]["kind"] != "shell" || list.Terminals[0]["exit_code"] != nil || list.Limit != 12 {
		t.Fatalf("terminal list: %s", w.Body.String())
	}
	if strings.Join(list.Kinds, ",") != "shell,claude,codex" {
		t.Fatalf("kinds: %v", list.Kinds)
	}
	if w := request(s, "GET", "/api/mobile/terminals/"+login.id+"/output?after=0", "", "", true); w.Code != 404 {
		t.Fatalf("login terminal readable from phone: %d", w.Code)
	}
	text, reply := readMobileTerminal(t, s, session.id, 0, "terminal-ready")
	if reply.Reset || !strings.Contains(text, "terminal-ready") {
		t.Fatalf("first read: %q %+v", text, reply)
	}
	if w := request(s, "GET", "/api/mobile/terminals/"+session.id+"/output?after="+strconv.FormatUint(reply.Next, 10), "", "", true); w.Code != http.StatusNoContent {
		t.Fatalf("idle read: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "GET", "/api/mobile/terminals/"+session.id+"/output?after=999999", "", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"reset":true`) {
		t.Fatalf("read ahead of output: %d %s", w.Code, w.Body.String())
	}
	for _, query := range []string{"after=01", "after=-1", "after=1&after=2", "x=1"} {
		if w := request(s, "GET", "/api/mobile/terminals/"+session.id+"/output?"+query, "", "", true); w.Code != 400 {
			t.Fatalf("query %q: %d", query, w.Code)
		}
	}
	input, _ := json.Marshal(map[string]string{"data": terminalInputLine("from-phone")})
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/input", s.origin, string(input), true); w.Code != http.StatusNoContent {
		t.Fatalf("input: %d %s", w.Code, w.Body.String())
	}
	if text, _ := readMobileTerminal(t, s, session.id, reply.Next, "child:from-phone"); !strings.Contains(text, "child:from-phone") {
		t.Fatalf("input not echoed: %q", text)
	}
	for body, status := range map[string]int{
		`{"data":""}`: 400, `{"data":"x","extra":1}`: 400, `{"data":"` + strings.Repeat("x", (32<<10)+1) + `"}`: 400,
	} {
		if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/input", s.origin, body, true); w.Code != status {
			t.Fatalf("input %.20s: %d", body, w.Code)
		}
	}
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/resize", s.origin, `{"cols":120,"rows":40}`, true); w.Code != http.StatusNoContent {
		t.Fatalf("resize: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/resize", s.origin, `{"cols":1,"rows":40}`, true); w.Code != 400 {
		t.Fatalf("invalid resize: %d", w.Code)
	}
	if w := request(s, "POST", "/api/mobile/terminals/ffffffffffffffff/input", s.origin, string(input), true); w.Code != 404 {
		t.Fatalf("unknown terminal: %d", w.Code)
	}
	exit, _ := json.Marshal(map[string]string{"data": terminalInputLine("exit")})
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/input", s.origin, string(exit), true); w.Code != http.StatusNoContent {
		t.Fatalf("exit input: %d", w.Code)
	}
	_, reply = readMobileTerminal(t, s, session.id, 0, "\x00never")
	if !reply.Closed || reply.ExitCode == nil || *reply.ExitCode != 0 {
		t.Fatalf("exit not delivered: %+v", reply)
	}
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/input", s.origin, string(input), true); w.Code != 409 {
		t.Fatalf("input after exit: %d", w.Code)
	}
	if w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/close", s.origin, "{}", true); w.Code != http.StatusNoContent {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	if s.terminals.get(session.id) != nil {
		t.Fatal("closed terminal kept")
	}
	s.account.lock()
	if w := request(s, "GET", "/api/mobile/terminals", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked list: %d", w.Code)
	}
}

func TestMobileTerminalInputQueueFull(t *testing.T) {
	// The queue is bounded by bytes; shrink the bound so the test stays small.
	setTerminalInputBudget(t, 256<<10)
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	session, err := s.terminals.start(terminalChildCommand("blocked-input"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"data": strings.Repeat("input\n", (32<<10)/6)})
	for range 200 {
		w := request(s, "POST", "/api/mobile/terminals/"+session.id+"/input", s.origin, string(body), true)
		if w.Code == http.StatusTooManyRequests {
			return
		}
		if w.Code != http.StatusNoContent {
			t.Fatalf("input: %d %s", w.Code, w.Body.String())
		}
	}
	t.Fatal("a full input queue was not reported")
}

func TestMobileTerminalCreateChecksKindAndEnvironment(t *testing.T) {
	s := testServer(t)
	for body, status := range map[string]int{
		`{"kind":"claude-login","workspace":"/p"}`:                  400,
		`{"kind":"codex-login","workspace":"/p"}`:                   400,
		`{"kind":"shell","workspace":""}`:                           400,
		`{"kind":"shell","workspace":"/p","claude_account_id":"x"}`: 400,
		`{"kind":"shell","workspace":"/p"}`:                         409,
	} {
		if w := request(s, "POST", "/api/mobile/terminals", s.origin, body, true); w.Code != status {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}
