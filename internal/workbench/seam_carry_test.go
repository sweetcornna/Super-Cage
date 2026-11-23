//go:build !windows

package workbench

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Finding 8 of the 0.1.17 review: a /clear in the handoff's target before it
// went idle changed the entry's id, and the messages the source never took
// were never delivered and nothing said so. They now follow the /clear link;
// when the target's conversation has left the entry they are shown instead.
func TestCarriedMessagesFollowAClearInTheTarget(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, handoffGuest)
	limitedSession(t, s, id, record)
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	carried, target := stored.Switch.Carry[0].ID, stored.Switch.TargetID
	// The target took over, then a /clear in its terminal was linked.
	cleared := uuid.NewString()
	stored.Switch.Phase, stored.Connection = "completed", uuid.NewString()
	stored.Executions = append(stored.Executions, claudeExecution{CLIID: target, Account: stored.Account})
	stored.Cleared = append(stored.Cleared, clearedPart{CLIID: target, ClearedAt: time.Now().UTC()})
	stored.CLIID = cleared
	if err := writePrivate(mobileRecordPath(s.profile, id), stored); err != nil {
		t.Fatal(err)
	}
	ready := true
	for range 2 {
		latest, _ := s.mobileCLI.record(s.profile, id)
		view := latest.snapshot()
		view.CanSend, view.Active = &ready, false
		s.deliverClaudeCarry(claudeSwitchContext(s), id, view)
	}
	sent := 0
	for _, call := range guestCalls(t, home, "rpc") {
		if call["request"] == carried {
			sent++
		}
	}
	if latest, _ := s.mobileCLI.record(s.profile, id); sent != 1 || !latest.Switch.Carry[0].Delivered {
		t.Fatalf("carried message after /clear: sent %d times, record %+v", sent, latest.Switch)
	}
	// The target's conversation separated into its own entry before delivery:
	// nothing is sent, the person is shown what was not delivered.
	latest, _ := s.mobileCLI.record(s.profile, id)
	latest.Switch.Carry[0].Delivered = false
	latest.Cleared = nil
	view := latest.snapshot()
	if view.AccountSwitch.carry || !strings.Contains(view.ConnectionNotice, "「waiting behind the limit」") || !strings.Contains(view.ConnectionNotice, "重新发送") {
		t.Fatalf("undeliverable carry: carry %v notice %q", view.AccountSwitch.carry, view.ConnectionNotice)
	}
}
