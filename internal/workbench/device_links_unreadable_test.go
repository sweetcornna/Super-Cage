package workbench

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// An unreadable joined-computers.json is named in the list's answer (it no
// longer fails the list, which hid 加入其他电脑), and forgetting the list
// moves the file aside, private and never overwritten, so the person can join
// again. A readable list is never cleared this way, and only the window may
// clear it.
func TestJoinedComputerUnreadableListCanBeCleared(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := shortCodeJoinerForTest(t, relayOrigin)
	if w := deviceRequest(s, "DELETE", "/api/client/devices", "", deviceCall{}); w.Code != http.StatusConflict {
		t.Fatalf("clearing a readable list: HTTP %d %s", w.Code, w.Body.String())
	}
	damaged := `{"version":1,"computers":[{"id":"x"}]}`
	if err := os.WriteFile(s.joinedComputersPath(), []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	list := func() (int, struct {
		Computers []joinedComputerView `json:"computers"`
		Problem   string               `json:"problem"`
	}) {
		t.Helper()
		w := deviceRequest(s, "GET", "/api/client/devices", "", deviceCall{})
		var reply struct {
			Computers []joinedComputerView `json:"computers"`
			Problem   string               `json:"problem"`
		}
		if w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("list: %s", w.Body.String())
		}
		return w.Code, reply
	}
	if status, reply := list(); status != http.StatusOK || reply.Problem != errJoinedUnreadable.Error() || reply.Computers == nil || len(reply.Computers) != 0 {
		t.Fatalf("unreadable list: HTTP %d %+v", status, reply)
	}
	for _, call := range []deviceCall{{agent: true}, {untrusted: true}} {
		if w := deviceRequest(s, "DELETE", "/api/client/devices", "", call); w.Code != http.StatusForbidden {
			t.Fatalf("clear %+v: HTTP %d", call, w.Code)
		}
	}
	if w := deviceRequest(s, "DELETE", "/api/client/devices", "", deviceCall{}); w.Code != http.StatusOK {
		t.Fatalf("clear: HTTP %d %s", w.Code, w.Body.String())
	}
	aside, _ := filepath.Glob(s.joinedComputersPath() + ".unreadable-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
	if info, err := os.Stat(aside[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", aside[0], info, err)
	}
	if b, _ := os.ReadFile(aside[0]); string(b) != damaged {
		t.Fatalf("the file aside: %s", b)
	}
	if _, err := os.Stat(s.joinedComputersPath()); !os.IsNotExist(err) {
		t.Fatalf("joined-computers.json after clearing: %v", err)
	}
	if status, reply := list(); status != http.StatusOK || reply.Problem != "" || len(reply.Computers) != 0 {
		t.Fatalf("after clearing: HTTP %d %+v", status, reply)
	}
	id := joinComputerForTest(t, s, deviceHostCode(t, host))
	if record := readPrivateJoined(t, s); len(record.Computers) != 1 || record.Computers[0].ID != id {
		t.Fatalf("joined again: %+v", record)
	}
}
