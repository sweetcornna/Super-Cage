package workbench

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/agents"
	"github.com/sylvinus/agent-vm/internal/cage"
)

// Design row S17: whatever the presence line holds, the attach goes on; a
// line that is missing or malformed only leaves presence unknown.
func TestClientPresenceLineNeverBlocksAttach(t *testing.T) {
	manifest, err := json.Marshal(cage.RequiredGuestManifest())
	if err != nil {
		t.Fatal(err)
	}
	receipts := func(n int) string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = "r" + strings.Repeat("x", i%20) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		}
		return strings.Join(ids, ",")
	}
	for _, tc := range []struct {
		name, line string
		known      bool
		clients    []string
		receipts   int
	}{
		{"missing", "", false, nil, 0},
		{"empty line", "\n", false, nil, 0},
		{"nothing present", "agent-clients/1 present= receipts=\n", true, []string{}, 0},
		{"codex", "agent-clients/1 present=codex receipts=codex\n", true, []string{"codex"}, 1},
		{"codex installed by hand", "agent-clients/1 present=codex receipts=\n", true, []string{"codex"}, 0},
		{"both, sorted", "agent-clients/1 present=codex,antigravity-cli receipts=\n", true, []string{"antigravity-cli", "codex"}, 0},
		{"blank line before it", "\nagent-clients/1 present=codex receipts=\n", true, []string{"codex"}, 0},
		{"32 receipts", "agent-clients/1 present= receipts=" + receipts(32) + "\n", true, []string{}, 32},
		{"33 receipts", "agent-clients/1 present= receipts=" + receipts(33) + "\n", false, nil, 0},
		{"marker only", "agent-clients/1\n", false, nil, 0},
		{"oversized", "agent-clients/1 present= receipts=" + strings.Repeat("a", 40<<10) + "\n", false, nil, 0},
		{"newline in a name", "agent-clients/1 present=co\ndex receipts=\n", false, nil, 0},
		{"newline in a receipt", "agent-clients/1 present=codex receipts=co\ndex\n", false, nil, 0},
		{"carriage return", "agent-clients/1 present=codex\r receipts=\n", false, nil, 0},
		{"unknown client", "agent-clients/1 present=gemini receipts=\n", false, nil, 0},
		{"duplicate", "agent-clients/1 present=codex,codex receipts=\n", false, nil, 0},
		{"trailing comma", "agent-clients/1 present=codex, receipts=\n", false, nil, 0},
		{"upper case receipt", "agent-clients/1 present= receipts=Codex\n", false, nil, 0},
		{"extra field", "agent-clients/1 present=codex receipts= more\n", false, nil, 0},
		{"fields swapped", "agent-clients/1 receipts= present=codex\n", false, nil, 0},
		{"other version", "agent-clients/2 present=codex receipts=\n", false, nil, 0},
		{"shell noise", "agent-clients/1 present=codex receipts=\nbash: warning\n", false, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := "{\"token\":\"guest-test\"}\npassword\n" + tc.line + string(manifest) + "\n"
			credentials, current, presence, err := parseGuestConnection([]byte(read))
			if err != nil || current == nil || !current.Current() || credentials.Token != "guest-test" || credentials.VNCPassword != "password" {
				t.Fatalf("attach refused: %v (manifest %v)", err, current)
			}
			if presence.Known != tc.known {
				t.Fatalf("known = %v, want %v", presence.Known, tc.known)
			}
			if tc.known && (!slices.Equal(presence.Clients, tc.clients) || len(presence.Receipts) != tc.receipts) {
				t.Fatalf("presence = %+v", presence)
			}
		})
	}
	t.Run("a stale manifest is still reported and the reading kept", func(t *testing.T) {
		_, _, presence, err := parseGuestConnection([]byte("{\"token\":\"guest-test\"}\npassword\nagent-clients/1 present=codex receipts=\nnull\n"))
		if !errors.Is(err, errGuestUpdateRequired) || !presence.Known {
			t.Fatalf("err = %v, presence = %+v", err, presence)
		}
	})
	t.Run("no manifest output reads as before", func(t *testing.T) {
		_, _, _, err := parseGuestConnection([]byte("{\"token\":\"guest-test\"}\npassword\nagent-clients/1 present= receipts=\n"))
		if err == nil || errors.Is(err, errGuestUpdateRequired) {
			t.Fatalf("err = %v, want the missing-service error of the three-part read", err)
		}
	})
}

// The fixed table must name exactly the catalog's optional entries: the
// attach read cannot parse the catalog (guarantee G).
func TestClientPresenceTableMatchesCatalog(t *testing.T) {
	entries, err := agents.All()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, e := range entries {
		if e.Optional() {
			want = append(want, e.ID+":"+e.Launch.Command)
		}
	}
	for _, c := range presenceClients {
		got = append(got, c.id+":"+c.command)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("presence table %v, catalog %v", got, want)
	}
	if probe := clientPresenceProbe(clientReceiptDir); !strings.Contains(guestConnectionSnapshot, probe) ||
		strings.Index(guestConnectionSnapshot, probe) > strings.Index(guestConnectionSnapshot, "components.json") {
		t.Fatal("the presence line is not read before the manifest")
	}
}

func presenceReading(t *testing.T, line string) clientPresence {
	t.Helper()
	p, ok := parseClientPresence(line)
	if !ok {
		t.Fatalf("bad fixture line %q", line)
	}
	return p
}

func statusView(t *testing.T, s *Server) (string, map[string]json.RawMessage) {
	t.Helper()
	response := httptest.NewRecorder()
	s.status(response, httptest.NewRequest("GET", "/api/status", nil))
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	return response.Body.String(), fields
}

// Unknown, absent and present, and the last known state on file.
func TestClientPresenceThreeStates(t *testing.T) {
	s := testServer(t)
	s.clientsTracked = true
	s.clients = loadClientPresence(s.profile)
	view := func() map[string]any {
		s.mu.Lock()
		defer s.mu.Unlock()
		view := map[string]any{}
		s.clientPresenceViewLocked(view)
		return view
	}
	stored := func() string {
		data, err := os.ReadFile(filepath.Join(s.profile, clientPresenceFile))
		if err != nil {
			return ""
		}
		return string(data)
	}
	if got := view(); len(got) != 1 || got["clients_unknown"] != true {
		t.Fatalf("first start: %v", got)
	}
	if stored() != "" {
		t.Fatal("unknown wrote a file")
	}
	s.recordClientPresence(presenceReading(t, "agent-clients/1 present= receipts="))
	if got := view(); len(got) != 0 {
		t.Fatalf("absent: %v", got)
	}
	if !strings.Contains(stored(), "\"version\": 1") {
		t.Fatalf("absent not kept: %q", stored())
	}
	s.recordClientPresence(presenceReading(t, "agent-clients/1 present=codex receipts=codex"))
	if got := view(); len(got) != 1 || !slices.Equal(got["clients"].([]string), []string{"codex"}) {
		t.Fatalf("present: %v", got)
	}
	// A malformed line keeps the last known state.
	_, _, broken, _ := parseGuestConnection([]byte("{\"token\":\"t\"}\np\nagent-clients/1 present=co\ndex receipts=\nnull"))
	s.recordClientPresence(broken)
	if got := view(); len(got) != 1 || got["clients"] == nil {
		t.Fatalf("malformed line changed the state: %v", got)
	}
	// The same reading again does not write the file.
	if err := os.WriteFile(filepath.Join(s.profile, clientPresenceFile), []byte(`{"version":1,"clients":["codex"],"receipts":["sentinel"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.recordClientPresence(presenceReading(t, "agent-clients/1 present=codex receipts=codex"))
	if !strings.Contains(stored(), "sentinel") {
		t.Fatal("an unchanged reading rewrote the file")
	}
	// A fresh host knows the last good reading without asking the guest.
	s.recordClientPresence(presenceReading(t, "agent-clients/1 present=codex receipts="))
	if p := loadClientPresence(s.profile); !p.Known || !slices.Equal(p.Clients, []string{"codex"}) || len(p.Receipts) != 0 {
		t.Fatalf("reloaded %+v", p)
	}
	for name, content := range map[string]string{
		"corrupt":        "{",
		"other version":  `{"version":2,"clients":[],"receipts":[]}`,
		"unknown client": `{"version":1,"clients":["gemini"],"receipts":[]}`,
		"missing list":   `{"version":1,"clients":["codex"]}`,
	} {
		if err := os.WriteFile(filepath.Join(s.profile, clientPresenceFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if p := loadClientPresence(s.profile); p.Known {
			t.Fatalf("%s file read as %+v", name, p)
		}
	}
}

// /status of a Claude-only environment is byte-identical to the view
// without presence; unknown and present add exactly one key each.
func TestClientPresenceStatusBytes(t *testing.T) {
	s := testServer(t)
	before, beforeFields := statusView(t, s)
	if again, _ := statusView(t, s); again != before {
		t.Skip("the fixture's status is not stable between two reads")
	}
	s.clientsTracked = true
	s.clients = presenceReading(t, "agent-clients/1 present= receipts=codex")
	if absent, _ := statusView(t, s); absent != before {
		t.Fatalf("absent changed /status:\n%s\n%s", before, absent)
	}
	for _, tc := range []struct {
		presence clientPresence
		key      string
		value    string
	}{
		{clientPresence{}, "clients_unknown", "true"},
		{presenceReading(t, "agent-clients/1 present=codex receipts="), "clients", `["codex"]`},
	} {
		s.clients = tc.presence
		_, fields := statusView(t, s)
		added := maps.Clone(fields)
		delete(added, tc.key)
		if string(fields[tc.key]) != tc.value || len(added) != len(beforeFields) {
			t.Fatalf("%s: got %s, %d keys (before %d)", tc.key, fields[tc.key], len(fields), len(beforeFields))
		}
		for key, value := range beforeFields {
			if string(added[key]) != string(value) {
				t.Fatalf("%s changed %s", tc.key, key)
			}
		}
	}
}

// The phone's Codex kind and flag follow presence; unknown is as before.
func TestPhoneCodexFollowsClientPresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracked bool
		line    string
		codex   bool
	}{
		{"not tracked", false, "", true},
		{"unknown", true, "", true},
		{"absent", true, "agent-clients/1 present= receipts=", false},
		{"receipt only", true, "agent-clients/1 present= receipts=codex", false},
		{"present", true, "agent-clients/1 present=codex receipts=", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			s.clientsTracked = tc.tracked
			if tc.line != "" {
				s.clients = presenceReading(t, tc.line)
			}
			if got := slices.Contains(s.mobileTerminalKinds(), "codex"); got != tc.codex {
				t.Fatalf("kinds %v", s.mobileTerminalKinds())
			}
			response := httptest.NewRecorder()
			s.mobileProjects(response, httptest.NewRequest("GET", "/api/mobile/cli/projects", nil))
			var answer struct {
				Codex *bool `json:"codex"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || answer.Codex == nil || *answer.Codex != tc.codex {
				t.Fatalf("codex flag: %s (%v)", response.Body, err)
			}
		})
	}
}
