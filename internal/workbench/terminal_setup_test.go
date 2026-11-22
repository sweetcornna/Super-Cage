package workbench

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The project connection and the account selection both wait on the guest and
// do not need each other, so they must overlap.
func TestConnectAndSelectOverlap(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	connect := func() error { started <- "connect"; <-release; return nil }
	choose := func() (claudeSelection, error) {
		started <- "select"
		<-release
		return claudeSelection{ID: "chosen"}, nil
	}
	type result struct {
		selection claudeSelection
		err       error
	}
	done := make(chan result, 1)
	go func() {
		selection, err := connectAndSelect(connect, choose)
		done <- result{selection, err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the second step did not start while the first was still waiting")
		}
	}
	close(release)
	select {
	case got := <-done:
		if got.err != nil || got.selection.ID != "chosen" {
			t.Fatalf("result: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connectAndSelect did not finish")
	}
}

func TestConnectAndSelectJoinsBothAndKeepsErrorOrder(t *testing.T) {
	projectErr, accountErr := errors.New("project"), errors.New("account")
	var connected atomic.Bool
	slowConnect := func() error { time.Sleep(100 * time.Millisecond); connected.Store(true); return nil }
	failingChoice := func() (claudeSelection, error) { return claudeSelection{}, accountErr }
	// A selection that fails early does not release the caller, and the
	// environment lock it holds, while the project step is still running.
	if _, err := connectAndSelect(slowConnect, failingChoice); err != accountErr || !connected.Load() {
		t.Fatalf("returned before the project step finished, or lost the selection error: %v connected=%v", err, connected.Load())
	}
	// A project error is reported first, as when the steps ran in sequence.
	var chosen atomic.Bool
	choose := func() (claudeSelection, error) {
		time.Sleep(50 * time.Millisecond)
		chosen.Store(true)
		return claudeSelection{}, accountErr
	}
	if _, err := connectAndSelect(func() error { return projectErr }, choose); err != projectErr || !chosen.Load() {
		t.Fatalf("project error was not reported first: %v chosen=%v", err, chosen.Load())
	}
	if selection, err := connectAndSelect(func() error { return nil }, func() (claudeSelection, error) { return claudeSelection{ID: "x"}, nil }); err != nil || selection.ID != "x" {
		t.Fatalf("both steps succeeded: %+v %v", selection, err)
	}
}

// Choosing an account for a launch reads the managed profiles' quotas and
// re-verifies the default login. They run together, so a launch waits for the
// slower one only.
func TestClaudeAllocationReadsQuotasAndDefaultLoginTogether(t *testing.T) {
	s := testServer(t)
	managed := strings.Repeat("a", 24)
	list := claudeAccountList{Accounts: []claudeAccount{
		{ID: managed, Source: "managed", Enabled: true, Status: "ready", Email: "managed@example.org"},
		{ID: strings.Repeat("b", 24), Source: "default", Enabled: true, Status: "ready", Email: "default@example.org"},
	}}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	probeStarted, readStarted := make(chan struct{}), make(chan struct{})
	var overlapped atomic.Bool
	overlapped.Store(true)
	meet := func(mine, other chan struct{}) {
		close(mine)
		select {
		case <-other:
		case <-time.After(3 * time.Second):
			overlapped.Store(false)
		}
	}
	s.claudeAccounts.probe = func(context.Context, string) (claudeGuestSnapshot, error) {
		meet(probeStarted, readStarted)
		return claudeGuestSnapshot{LoggedIn: true, Email: "default@example.org"}, nil
	}
	read := func(context.Context, []string) (map[string]claudeGuestQuota, error) {
		meet(readStarted, probeStarted)
		return nil, nil
	}
	selection, err := s.allocateClaudeAccount(list, "", read)
	if err != nil || selection.ID == "" {
		t.Fatalf("allocation: %+v %v", selection, err)
	}
	if !overlapped.Load() {
		t.Fatal("the quota read and the default login check ran one after the other")
	}
}

// prepareProject only reads local state: a path that was never added fails
// before anything reaches the guest, and a client mount needs no guest step.
func TestPrepareProjectReadsOnlyLocalState(t *testing.T) {
	s := testServer(t)
	if _, connect, err := s.prepareProject(t.TempDir(), false); err == nil || connect != nil {
		t.Fatalf("an unauthorized folder was prepared: %v", err)
	}
	if _, connect, err := s.prepareProject(clientMountRoot+"/missing", false); !errors.Is(err, errClientMountGone) || connect == nil {
		t.Fatalf("a missing client mount: %v", err)
	}
}
