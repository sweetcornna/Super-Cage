package workbench

import (
	"fmt"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Keep stage measurements in memory until the round ends so log writes do not
// consume the time available to the next probe or the lease renewal.
type monitorTiming struct {
	started, checkpoint time.Time
	until, verified     time.Time
	result              string
	stages              []string
}

func (s *Server) startMonitorTiming() *monitorTiming {
	now := time.Now()
	timing := &monitorTiming{started: now, checkpoint: now, result: "completed"}
	s.mu.Lock()
	timing.verified = s.safety.VerifiedAt
	if s.safety.Gate != nil {
		timing.until = s.safety.Gate.Deadline()
	}
	s.mu.Unlock()
	return timing
}

func (t *monitorTiming) stage(name string) {
	now := time.Now()
	t.stages = append(t.stages, fmt.Sprintf("%s_ms=%d", name, now.Sub(t.checkpoint).Milliseconds()))
	t.checkpoint = now
}

const (
	monitorQuietSlowThreshold = 500 * time.Millisecond
	monitorQuietHeartbeat     = 5 * time.Minute
)

func quietMonitorResult(result string) bool {
	switch result {
	case "steady", "completed", "not_due", "environment_not_ready", "network_paused":
		return true
	default:
		return false
	}
}

func (s *Server) recordMonitorTiming(kind string, timing *monitorTiming) {
	if s.profile == "" {
		return
	}
	finished := time.Now()
	duration := finished.Sub(timing.started)
	// Suppress repeated identical steady-state records to avoid locking and
	// scanning events.jsonl every 5 s while healthy. State transitions,
	// failures, lock skips, slow rounds, and a 5-minute heartbeat are always kept.
	s.mu.Lock()
	if s.monitorLastResult == nil {
		s.monitorLastResult = map[string]string{}
		s.monitorLastLogged = map[string]time.Time{}
	}
	if quietMonitorResult(timing.result) &&
		duration < monitorQuietSlowThreshold &&
		s.monitorLastResult[kind] == timing.result &&
		!s.monitorLastLogged[kind].IsZero() &&
		finished.Sub(s.monitorLastLogged[kind]) < monitorQuietHeartbeat {
		s.mu.Unlock()
		return
	}
	s.monitorLastResult[kind] = timing.result
	s.monitorLastLogged[kind] = finished
	s.mu.Unlock()
	age, remainingStart, remainingEnd := int64(-1), int64(-1), int64(-1)
	if !timing.verified.IsZero() {
		age = timing.started.Sub(timing.verified).Milliseconds()
	}
	if !timing.until.IsZero() {
		remainingStart = timing.until.Sub(timing.started).Milliseconds()
		remainingEnd = timing.until.Sub(finished).Milliseconds()
	}
	_ = diagnostics.New(s.profile).Record("info", "network-monitor", fmt.Sprintf(
		"monitor=%s started=%s duration_ms=%d result=%s verified_age_ms=%d lease_known=%t lease_remaining_start_ms=%d lease_remaining_end_ms=%d %s",
		kind, timing.started.UTC().Format(time.RFC3339Nano), duration.Milliseconds(),
		timing.result, age, !timing.until.IsZero(), remainingStart, remainingEnd, strings.Join(timing.stages, " ")))
}
