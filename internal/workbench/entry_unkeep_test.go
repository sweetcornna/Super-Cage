//go:build !windows

package workbench

import (
	"testing"
	"time"
)

// R3-5: turning the entry off makes kept terminals ordinary Super Cage
// terminals. They run on, without 已保留 and without the idle check, so an
// entry that is off runs no timer; nothing is kept again until it is on.
func TestEntryOffMakesKeptTerminalsOrdinary(t *testing.T) {
	for _, connected := range []bool{true, false} {
		name := "connected"
		if !connected {
			name = "after its connection ended"
		}
		t.Run(name, func(t *testing.T) { entryOffMakesKeptOrdinary(t, connected) })
	}
}

func entryOffMakesKeptOrdinary(t *testing.T, connected bool) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	shell := entryOpenShell(t, client, 100, 30, "")
	shell.send("printf 'R=%s\\n' ready\n")
	shell.expect("R=")
	id := entrySSHTerminals(f.s)[0]["id"].(string)
	t.Cleanup(func() {
		f.s.terminals.remove(id)
		f.s.entryUnkept()
	})
	if err := f.call("POST", "/api/terminals/"+id+"/keep", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if f.s.terminals.keptCount() != 1 || !f.s.entryKeptTimerRunning() {
		t.Fatalf("before off: kept %d, idle check %v", f.s.terminals.keptCount(), f.s.entryKeptTimerRunning())
	}
	if !connected {
		_ = client.Close()
		entryAwait(t, 5*time.Second, "the kept terminal without a client", func() bool { return len(f.s.entryWaitingTerminals(f.computer.ID)) == 1 })
	}
	if err := f.call("POST", "/api/entry/off", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 5*time.Second, "the connection to end", func() bool {
		f.s.entry.registry.mu.Lock()
		defer f.s.entry.registry.mu.Unlock()
		return len(f.s.entry.registry.conns) == 0
	})
	check := func(when string) {
		t.Helper()
		terminals := entrySSHTerminals(f.s)
		if len(terminals) != 1 || terminals[0]["closed"] != false {
			t.Fatalf("%s: terminals %v", when, terminals)
		}
		if origin, _ := terminals[0]["origin"].(map[string]any); origin["kept"] != false {
			t.Fatalf("%s: origin %v", when, origin)
		}
		if f.s.terminals.keptCount() != 0 || len(f.s.entryKeptViews()) != 0 || len(f.s.entryWaitingTerminals(f.computer.ID)) != 0 {
			t.Fatalf("%s: kept %d, listed %v", when, f.s.terminals.keptCount(), f.s.entryKeptViews())
		}
		if f.s.entryKeptTimerRunning() {
			t.Fatalf("%s: the idle check still runs", when)
		}
	}
	check("after off")
	// Nothing is kept while the entry is off: not by 保留, not by a lock.
	if err := f.call("POST", "/api/terminals/"+id+"/keep", map[string]any{}, nil); err == nil {
		t.Fatal("保留 with the entry off was accepted")
	}
	if session := f.s.terminals.get(id); session == nil || f.s.entryKeep(session) == nil {
		t.Fatal("a keep with the entry off was accepted")
	}
	check("after a keep with the entry off")
}
