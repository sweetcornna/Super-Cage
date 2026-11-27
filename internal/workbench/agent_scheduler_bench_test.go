package workbench

import (
	"fmt"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func BenchmarkAgentAccountSelection(b *testing.B) {
	spec, _ := agentcatalog.Lookup("opencode", nil)
	for _, size := range []int{1, 4, 16, 64} {
		now := time.Now().Truncate(time.Second)
		pool := agentAccountPool{Scheduling: defaultAgentScheduling(), Accounts: make([]agentAccount, size)}
		for i := range pool.Accounts {
			pool.Accounts[i] = agentAccount{
				ID: fmt.Sprintf("%024x", i+1), Label: fmt.Sprintf("Account %d", i+1),
				Source: "managed", AuthMode: "oauth", Enabled: true, Status: "ready",
				Weight: 10 + i%3, MaxConcurrency: 4, HealthEWMA: 0.92,
				Quota: &claudeQuota{
					ObservedAt: now.Unix(),
					FiveHour:   &claudeWindow{UsedPercentage: float64(25 + i%10), ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()},
					SevenDay:   &claudeWindow{UsedPercentage: float64(65 + i%10), ResetsAt: now.Add(72 * time.Hour).Unix(), ObservedAt: now.Unix()},
				},
			}
		}
		b.Run(fmt.Sprintf("launch/%d", size), func(b *testing.B) {
			s := &Server{}
			s.agentCLIs.pools, s.agentCLIs.poolsLoaded = map[string]*agentAccountPool{spec.ID: &pool}, true
			s.terminals.sessions = map[string]*terminalSession{
				"current": {agentID: spec.ID, accountID: pool.Accounts[0].ID, kind: "agent", done: make(chan struct{})},
			}
			b.ReportAllocs()
			for b.Loop() {
				sel, _, err := s.selectAgentAccount(spec, "auto", false, "/project")
				if err != nil || sel.ID == "" {
					b.Fatalf("selection failed: %+v, %v", sel, err)
				}
			}
		})
		b.Run(fmt.Sprintf("preview/%d", size), func(b *testing.B) {
			active := map[string]int{pool.Accounts[0].ID: 1}
			b.ReportAllocs()
			for b.Loop() {
				decision := pickAgentAccount(pool, active, nil, "/project", "", "", time.Time{}, now)
				if decision.AccountID == "" || len(decision.Candidates) != size {
					b.Fatalf("incomplete preview: %+v", decision)
				}
			}
		})
	}
}
