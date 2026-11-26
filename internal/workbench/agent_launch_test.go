package workbench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestCodexAndAgentLaunchScheduleFollowsSchedulerRanking(t *testing.T) {
	id := func(c byte) string { return strings.Repeat(string(c), 24) }
	now := time.Now()

	// 1. Native Linux Codex schedule matches Claude preferred-first ordering.
	codexList := codexAccountList{
		Scheduling: claudeScheduling{Mode: "balanced", Preferred: id('2')},
		Accounts: []codexAccount{
			{ID: id('1'), Enabled: true, Status: "ready", Email: "first@example.test", Organization: "org1"},
			{ID: id('2'), Enabled: false, Status: "ready", Email: "pref@example.test", Organization: "org2"},
			{ID: id('3'), Enabled: true, Status: "signed_out", Email: "out@example.test"},
			{ID: id('4'), Enabled: true, Status: "ready", Email: "fourth@example.test"},
		},
	}
	codexSched := codexLaunchScheduleOf(codexList)
	if codexSched.Mode != "balanced" || len(codexSched.Accounts) != 3 {
		t.Fatalf("unexpected codex schedule: %+v", codexSched)
	}
	if codexSched.Accounts[0].ID != id('2') || !codexSched.Accounts[0].Preferred {
		t.Fatalf("preferred codex account not placed first: %+v", codexSched.Accounts)
	}
	if codexSched.Accounts[1].ID != id('1') || codexSched.Accounts[2].ID != id('4') {
		t.Fatalf("unexpected codex candidate order: %+v", codexSched.Accounts)
	}

	// 2. Universal Agent CLI schedule ranks by multi-factor score, excludes
	// disabled/signed-out/exhausted accounts, and maps project pins to guest paths.
	pools := map[string]*agentAccountPool{
		"opencode": {
			Scheduling: agentScheduling{
				Mode:        "smart",
				ProjectPins: map[string]string{"/host/proj": "account:" + id('a')},
			},
			Accounts: []agentAccount{
				{
					ID: id('a'), Label: "LowHealth", Source: "api_key", AuthMode: "api_key",
					Enabled: true, Status: "ready", Weight: 10, HealthEWMA: 0.4,
				},
				{
					ID: id('b'), Label: "Healthy", Source: "api_key", AuthMode: "api_key",
					Enabled: true, Status: "ready", Weight: 20, HealthEWMA: 1.0,
				},
				{
					ID: id('c'), Label: "Exhausted", Source: "managed", AuthMode: "oauth",
					Enabled: true, Status: "ready", Weight: 50, HealthEWMA: 1.0,
					Quota: &claudeQuota{
						ObservedAt: now.Unix(),
						FiveHour:   &claudeWindow{UsedPercentage: 100, ResetsAt: now.Unix() + 3600, ObservedAt: now.Unix()},
					},
				},
				{
					ID: id('d'), Label: "Disabled", Source: "api_key", AuthMode: "api_key",
					Enabled: false, Status: "ready", Weight: 50,
				},
			},
		},
	}
	sched := agentLaunchScheduleOf(pools, agentcatalog.BuiltIn(), func(ws string) string {
		if ws == "/host/proj" {
			return "/workspace/proj"
		}
		return ws
	}, now)
	entry, ok := sched.Agents["opencode"]
	if !ok {
		t.Fatalf("opencode missing from schedule: %+v", sched)
	}
	if len(entry.Candidates) != 2 {
		t.Fatalf("expected 2 eligible opencode candidates, got %+v", entry.Candidates)
	}
	if entry.Candidates[0].ID != id('b') || entry.Candidates[1].ID != id('a') {
		t.Fatalf("expected Healthy ranked ahead of LowHealth, got %+v", entry.Candidates)
	}
	if entry.ProjectPins["/workspace/proj"] != "account:"+id('a') {
		t.Fatalf("guest project pin missing: %+v", entry.ProjectPins)
	}
	if entry.Spec.Binary != "opencode" {
		t.Fatalf("compact spec missing binary: %+v", entry.Spec)
	}
}

func TestResolveToolAccountBindingsDynamicAutoAndNativeCodex(t *testing.T) {
	s := testServer(t)
	s.ready = true
	claudeID := strings.Repeat("a", 24)
	codexID := strings.Repeat("b", 24)
	agyID := strings.Repeat("c", 24)

	s.claudeAccounts.mu.Lock()
	_ = s.saveClaudeAccounts(claudeAccountList{
		Accounts:   []claudeAccount{{ID: claudeID, Label: "Claude Pro", Source: "managed", Email: "c@example.test", Enabled: true, Status: "ready"}},
		Scheduling: (claudeScheduling{}).defaults(),
	})
	s.claudeAccounts.mu.Unlock()

	s.codexAccounts.mu.Lock()
	_ = s.saveCodexAccounts(codexAccountList{
		Accounts:   []codexAccount{{ID: codexID, Label: "Codex Plus", Email: "x@example.test", Enabled: true, Status: "ready"}},
		Scheduling: (claudeScheduling{}).defaults(),
	})
	s.codexAccounts.mu.Unlock()

	s.agentCLIs.mu.Lock()
	_ = s.saveAgentPoolsLocked(map[string]*agentAccountPool{
		"opencode": {
			Scheduling: defaultAgentScheduling(),
			Accounts:   []agentAccount{{ID: agyID, Label: "OpenCode Key", Source: "api_key", AuthMode: "api_key", Enabled: true, Status: "ready", Weight: 10, HealthEWMA: 1.0}},
		},
	})
	s.agentCLIs.mu.Unlock()

	// 1. Auto bindings resolve to live mode="auto" across Claude, native Linux Codex, and Agent CLIs.
	bindings, labels, err := s.resolveToolAccountBindings(map[string]string{
		"claude":   "auto",
		"codex":    "auto",
		"opencode": "auto",
		"gemini":   "default",
	}, "/proj", "Orca Server")
	if err != nil {
		t.Fatalf("resolve auto failed: %v", err)
	}
	raw, _ := json.Marshal(bindings)
	for _, agent := range []string{"claude", "codex", "opencode"} {
		m, _ := bindings[agent].(map[string]any)
		if m["mode"] != "auto" {
			t.Fatalf("%s binding = %+v (all=%s), want mode=auto", agent, m, raw)
		}
		if !strings.Contains(labels[agent], "自动调度") {
			t.Fatalf("%s label = %q, want 自动调度", agent, labels[agent])
		}
	}
	if gem, _ := bindings["gemini"].(map[string]any); gem["mode"] != "default" {
		t.Fatalf("gemini binding = %+v, want mode=default", gem)
	}

	// 2. Explicit native Linux Codex account ID binds with codex_native=true and subject hash.
	explicit, expLabels, err := s.resolveToolAccountBindings(map[string]string{
		"codex": codexID,
	}, "/proj", "T3 Code")
	if err != nil {
		t.Fatalf("resolve explicit native codex failed: %v", err)
	}
	cm, _ := explicit["codex"].(map[string]any)
	if cm["guest_id"] != codexID || cm["codex_native"] != true || cm["subject"] != claudeIdentityHash("x@example.test", "") || expLabels["codex"] != "Codex Plus" {
		t.Fatalf("unexpected explicit native codex binding: %+v labels=%+v", cm, expLabels)
	}
}
