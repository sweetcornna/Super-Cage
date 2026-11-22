package workbench

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Round-2 N6: a window that is killed (or whose browser crashes) with a
// frame open never clears that session's storage itself, and every session
// has its own origin, so nothing else would: the next start clears it, from
// the journal of framed origins the window wrote before it attached.
func TestScienceBrowserStorageOfAKilledWindowIsCleared(t *testing.T) {
	env := newTestScienceFront(t)
	env.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceStoringPage, nonce) }
	harness := newScienceFrameHarness(t, scienceTestFrameSources(env))
	env.front.hooks.ancestor = func() string { return harness.origin }
	proxyPort, _ := newScienceCanaryProxy(t)
	profile := t.TempDir()
	journal := filepath.Join(profile, "cage-science-framed.json")
	first := launchTestBrowserIn(t, profile, nil, scienceHostEnv, scienceBackstopArgs(proxyPort)...)
	shell := newScienceShell(t, first, first.page(t))
	shell.journal = journal
	shell.load(t, harness.origin+"/")
	entry := env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
	origin := scienceSessionOrigin(entry)
	shell.open(t, entry)
	waitFor(t, 30*time.Second, "the page's storage", func() bool { return len(scienceDocReports(env.daemon)) > 0 })
	// Local storage reaches the disk a moment after it is set.
	time.Sleep(2 * time.Second)
	first.kill()

	second := launchTestBrowserIn(t, profile, nil, scienceHostEnv, scienceBackstopArgs(proxyPort)...)
	next := newScienceShell(t, second, second.page(t))
	next.journal = journal
	next.load(t, harness.origin+"/")
	next.sweep(t)
	left := scienceStorageLeft(t, second, next, origin)
	t.Logf("the second launch found %v", left)
	if left != scienceStorageEmpty {
		t.Fatalf("the killed window's session origin kept %v", left)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("the journal outlived the sweep: %v", err)
	}
}
