package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Presence of the optional agent clients (其他客户端, design 2.3).
//
// A client is present when its command is found where Codex's own helpers
// look: ~/.local/bin first, then the PATH of the SSH session. The attach read
// (guestConnectionSnapshot) carries one line for it, before the manifest, so
// knowing costs no request of its own:
//
//	agent-clients/1 present=<ids> receipts=<ids>
//
// <ids> are comma-separated catalog ids, empty when there are none: present
// names the entries whose command was found, receipts the receipt files under
// /var/lib/agent-clients (at most 32). A receipt never makes a client present.
//
// Three states: unknown (no good reading, here or on file: the window shows
// the Codex surfaces as before), absent and present. A missing or malformed
// line keeps the last known state and never fails the attach. The last good
// reading is kept beside codex-accounts.json, so a stopped environment and a
// fresh window know it without asking the guest.

const (
	clientPresenceMarker  = "agent-clients/1"
	clientPresenceFile    = "agent-clients.json"
	clientPresenceMaxLine = 2048
	clientReceiptLimit    = 32
	clientReceiptDir      = "/var/lib/agent-clients"
)

// presenceClients are the optional catalog entries by launcher command. The
// attach read must not parse the catalog (guarantee G, design 9.1), so the
// table is fixed here; a test pins it to internal/agents/catalog.json.
var presenceClients = []struct{ id, command string }{
	{"codex", "codex"},
	{"antigravity-cli", "agy"},
}

// clientIDPattern is the catalog's id rule (internal/agents/validate.go).
var clientIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// clientPresenceProbe prints the presence line. It uses shell builtins only,
// needs no sudo and writes nothing to stderr. Names are filtered by explicit
// character lists (a range would follow the guest's locale), so no newline
// and nothing longer than an id reaches the output.
func clientPresenceProbe(receiptDir string) string {
	const lower = "abcdefghijklmnopqrstuvwxyz"
	pairs := make([]string, 0, len(presenceClients))
	for _, c := range presenceClients {
		pairs = append(pairs, c.id+":"+c.command)
	}
	return `p=; for e in ` + strings.Join(pairs, " ") + `; do c=${e#*:}; x=$HOME/.local/bin:$PATH:; ` +
		`while [ -n "$x" ]; do d=${x%%:*}; x=${x#*:}; ` +
		`if [ -n "$d" ] && [ -f "$d/$c" ] && [ -x "$d/$c" ]; then p=$p${p:+,}${e%%:*}; break; fi; done; done; ` +
		`r=; n=0; for f in ` + receiptDir + `/*.json; do i=${f##*/}; i=${i%.json}; ` +
		`case $i in [` + lower + `]*) ;; *) continue ;; esac; ` +
		`case $i in *[!` + lower + `0123456789-]*) continue ;; esac; ` +
		`if [ ${#i} -le 32 ] && [ -f "$f" ]; then n=$((n+1)); [ $n -le ` + strconv.Itoa(clientReceiptLimit) + ` ] || break; r=$r${r:+,}$i; fi; done; ` +
		`printf '` + clientPresenceMarker + ` present=%s receipts=%s\n' "$p" "$r"`
}

type clientPresence struct {
	// Known is false until a good reading, now or on file.
	Known    bool
	Clients  []string
	Receipts []string
}

// splitClientPresence peels the presence line off the front of the attach
// read's third part. Without the marker the part is the manifest, exactly as
// before. With it, the manifest is the last line (the guest writes it as one
// line of JSON) and everything between is the presence reading, so even a
// broken reading cannot reach the manifest.
func splitClientPresence(rest string) (line, manifest string, marked bool) {
	trimmed := strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(trimmed, "agent-clients/") {
		return "", rest, false
	}
	end := strings.LastIndexByte(trimmed, '\n')
	if end < 0 {
		return trimmed, "", true
	}
	return trimmed[:end], trimmed[end+1:], true
}

// parseClientPresence reads one presence line; ok is false for anything but
// a well-formed line.
func parseClientPresence(line string) (clientPresence, bool) {
	if len(line) > clientPresenceMaxLine || strings.ContainsAny(line, "\r\n") {
		return clientPresence{}, false
	}
	fields := strings.Split(line, " ")
	if len(fields) != 3 || fields[0] != clientPresenceMarker {
		return clientPresence{}, false
	}
	present, ok := strings.CutPrefix(fields[1], "present=")
	if !ok {
		return clientPresence{}, false
	}
	receipts, ok := strings.CutPrefix(fields[2], "receipts=")
	if !ok {
		return clientPresence{}, false
	}
	p := clientPresence{Known: true}
	if p.Clients, ok = clientIDList(present, len(presenceClients), true); !ok {
		return clientPresence{}, false
	}
	if p.Receipts, ok = clientIDList(receipts, clientReceiptLimit, false); !ok {
		return clientPresence{}, false
	}
	return p, true
}

// clientIDList parses a comma-separated id list, sorted and never nil.
func clientIDList(list string, limit int, known bool) ([]string, bool) {
	ids := []string{}
	if list == "" {
		return ids, true
	}
	for _, id := range strings.Split(list, ",") {
		if !clientIDPattern.MatchString(id) || slices.Contains(ids, id) || known && !presenceClient(id) {
			return nil, false
		}
		ids = append(ids, id)
	}
	if len(ids) > limit {
		return nil, false
	}
	slices.Sort(ids)
	return ids, true
}

func presenceClient(id string) bool {
	for _, c := range presenceClients {
		if c.id == id {
			return true
		}
	}
	return false
}

func (p clientPresence) same(other clientPresence) bool {
	return p.Known == other.Known && slices.Equal(p.Clients, other.Clients) && slices.Equal(p.Receipts, other.Receipts)
}

type storedClientPresence struct {
	Version  int      `json:"version"`
	Clients  []string `json:"clients"`
	Receipts []string `json:"receipts"`
}

// loadClientPresence reads the last good reading; anything unreadable is
// unknown, never an error.
func loadClientPresence(profile string) clientPresence {
	data, err := os.ReadFile(filepath.Join(profile, clientPresenceFile))
	var stored storedClientPresence
	if err != nil || len(data) > 8<<10 || json.Unmarshal(data, &stored) != nil || stored.Version != 1 || stored.Clients == nil || stored.Receipts == nil {
		return clientPresence{}
	}
	p, _ := parseClientPresence(clientPresenceMarker + " present=" + strings.Join(stored.Clients, ",") + " receipts=" + strings.Join(stored.Receipts, ","))
	return p
}

// clientPresenceSave orders the writes of agent-clients.json.
var clientPresenceSave sync.Mutex

// recordClientPresence keeps a good reading. The file is written only when
// the reading changed, outside s.mu.
func (s *Server) recordClientPresence(p clientPresence) {
	if !p.Known {
		return
	}
	s.mu.Lock()
	changed := !s.clients.same(p)
	s.clients = p
	s.mu.Unlock()
	if !changed {
		return
	}
	clientPresenceSave.Lock()
	defer clientPresenceSave.Unlock()
	s.mu.Lock()
	current := s.clients
	s.mu.Unlock()
	_ = writePrivate(filepath.Join(s.profile, clientPresenceFile), storedClientPresence{Version: 1, Clients: current.Clients, Receipts: current.Receipts})
}

// clientPresenceViewLocked adds the presence to /status: nothing when no
// optional client is present (the bytes stay as before), "clients" when one
// is, "clients_unknown" before the first reading. Only a Linux-guest
// workload tracks presence; any other view is left as it was.
func (s *Server) clientPresenceViewLocked(view map[string]any) {
	switch {
	case !s.clientsTracked:
	case !s.clients.Known:
		view["clients_unknown"] = true
	case len(s.clients.Clients) > 0:
		view["clients"] = s.clients.Clients
	}
}

// codexOfferedLocked: the phone's Codex kind and flag follow presence
// (choice C); unknown shows them as before.
func (s *Server) codexOfferedLocked() bool {
	if !s.codexGuestSupported() {
		return false
	}
	return !s.clientsTracked || !s.clients.Known || slices.Contains(s.clients.Clients, "codex")
}
