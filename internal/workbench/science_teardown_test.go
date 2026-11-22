package workbench

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Round-2 N1 and N2 as browser tests: what the frame's workers do while the
// window tears the frame down.

// scienceZombiePage keeps the frame's document from going quietly (its
// pagehide spins for 3 s, which keeps the frame's process and its workers
// running after the element is gone) and runs a dedicated worker that
// starts a nested one. All three report the identity they see in a tight
// loop (the reviewer's N1 reproduction).
const scienceZombiePage = `<!doctype html><title>Claude Science</title><script nonce="%s">
const id = () => [Intl.DateTimeFormat().resolvedOptions().timeZone, new Date().getTimezoneOffset(), navigator.language, navigator.userAgent].join("|");
const report = (why) => fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ why, v: btoa(id()) }) });
const loop = async (why) => { for (;;) { try { await report(why); } catch {} await new Promise((resolve) => setTimeout(resolve, 2)); } };
loop("doc");
new Worker("/assets/direct.js");
addEventListener("pagehide", () => { const start = Date.now(); while (Date.now() - start < 3000) {} });
</script>`

const scienceZombieWorker = `const id = () => [Intl.DateTimeFormat().resolvedOptions().timeZone, new Date().getTimezoneOffset(), navigator.language, navigator.userAgent].join("|");
const report = (why) => fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ why, v: btoa(id()) }) });
const loop = async (why) => { for (;;) { try { await report(why); } catch {} await new Promise((resolve) => setTimeout(resolve, 2)); } };
`

type scienceZombieEnv struct {
	env     *testScienceFront
	harness *scienceFrameHarness
	browser *testBrowser
	shell   *scienceShell
}

func newScienceZombieEnv(t *testing.T) *scienceZombieEnv {
	t.Helper()
	env := newTestScienceFront(t)
	env.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceZombiePage, nonce) }
	env.daemon.assets["/assets/direct.js"] = scienceZombieWorker + `new Worker("/assets/nested.js"); loop("direct");`
	env.daemon.assets["/assets/nested.js"] = scienceZombieWorker + `loop("nested");`
	harness := newScienceFrameHarness(t, scienceTestFrameSources(env))
	env.front.hooks.ancestor = func() string { return harness.origin }
	proxyPort, _ := newScienceCanaryProxy(t)
	browser := launchTestBrowser(t, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	shell := newScienceShell(t, browser, browser.page(t))
	shell.load(t, harness.origin+"/")
	shell.open(t, env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	waitFor(t, 30*time.Second, "the session", func() bool { return env.front.snapshot().Session })
	waitFor(t, 15*time.Second, "reports from the document and both workers", func() bool {
		seen := map[string]bool{}
		for _, report := range scienceIdentityReports(t, env.daemon) {
			seen[report.Why] = true
		}
		return seen["doc"] && seen["direct"] && seen["nested"]
	})
	return &scienceZombieEnv{env: env, harness: harness, browser: browser, shell: shell}
}

// hostReports are the reports since index from whose values are not the
// guest's.
func (z *scienceZombieEnv) hostReports(t *testing.T, from int) []scienceIdentityReport {
	t.Helper()
	guest := scienceGuestValues()
	var host []scienceIdentityReport
	reports := scienceIdentityReports(t, z.env.daemon)
	for _, report := range reports[min(from, len(reports)):] {
		if report.Value != guest {
			host = append(host, report)
		}
	}
	return host
}

// N1: a frame whose document hangs in pagehide and whose workers outlive
// it: once the window starts its teardown, no host value reaches the
// daemon, and once the teardown has run nothing at all does. The window
// ends the front first and crashes the frame before it removes it; each
// step alone would already keep the host values away (end only, crash
// only).
func TestScienceBrowserTeardownOutlivesNoWorker(t *testing.T) {
	for _, variant := range []string{"top-level navigation", "pane unmounts", "pane gone without a release", "end only", "crash only", "neither (control)"} {
		t.Run(variant, func(t *testing.T) {
			z := newScienceZombieEnv(t)
			before := len(scienceIdentityReports(t, z.env.daemon))
			switch variant {
			case "top-level navigation":
				z.shell.navigateTop(t, z.harness.origin+"/slow")
			case "pane unmounts":
				z.shell.close(t)
			case "pane gone without a release":
				z.shell.vanish(t)
			case "end only":
				z.shell.noCrash = true
				z.shell.vanish(t)
			case "crash only":
				z.shell.noEnd = true
				z.shell.vanish(t)
			case "neither (control)":
				z.shell.noEnd, z.shell.noCrash = true, true
				z.shell.vanish(t)
			}
			torn := len(z.env.daemon.seen())
			time.Sleep(5 * time.Second)
			host := z.hostReports(t, before)
			t.Logf("%d reports after the teardown began, %d with host values; front ends %v", len(scienceIdentityReports(t, z.env.daemon))-before, len(host), z.shell.endResults)
			if variant == "neither (control)" {
				// Without both steps the page's workers do reach the daemon
				// with host values: the reproduction still reproduces.
				if len(host) == 0 {
					t.Fatal("no host value reached the daemon without the end and the crash")
				}
				return
			}
			for i, report := range host {
				if i < 5 {
					t.Errorf("the daemon got a host value after the teardown: %s %.60q", report.Why, report.Value)
				}
			}
			if seen := z.env.daemon.seen()[torn:]; len(seen) != 0 {
				t.Errorf("the daemon got %d requests after the teardown ran, first %s %s", len(seen), seen[0].Method, seen[0].Path)
			}
		})
	}
}

// A late teardown of a frame the pane already replaced ends nothing and
// removes nothing of the new frame.
func TestScienceBrowserLateTeardownSparesTheNextFrame(t *testing.T) {
	z := newScienceZombieEnv(t)
	z.shell.mu.Lock()
	first := z.shell.current
	z.shell.mu.Unlock()
	z.shell.close(t)
	next := z.env.front.issue(scienceTicket{epoch: z.env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
	z.shell.open(t, next)
	waitFor(t, 30*time.Second, "the next session", func() bool { return z.env.front.snapshot().Session })
	z.shell.endFront(first)
	z.shell.eval(t, scienceRemoveFrameScript(first))
	time.Sleep(time.Second)
	if results := z.shell.endResults; len(results) != 2 || results[1] != "stale" {
		t.Fatalf("front ends answered %v", results)
	}
	label := strings.TrimSuffix(strings.TrimPrefix(scienceSessionOrigin(next), "http://"), fmt.Sprintf(".localhost:%d", z.env.front.port))
	if state := z.env.front.labelState(label); state != "session" {
		t.Fatalf("the next frame's label is %q", state)
	}
	if z.shell.eval(t, `document.querySelectorAll("iframe[data-science-frame]").length`) != float64(1) {
		t.Fatal("the late removal took the next frame")
	}
}

// The window's end and crash waits leave time in the held navigation's
// wait for the frame's targets to go; ScienceFrame.cs has the same values
// (science-cdp.json).
func TestScienceTeardownFitsTheNavigationWait(t *testing.T) {
	if scienceShellEndWait+scienceCrashWait+time.Second > scienceNavigateWait {
		t.Fatalf("end %v + crash %v leave less than 1 s of the %v navigation wait", scienceShellEndWait, scienceCrashWait, scienceNavigateWait)
	}
}

// N2: a nested worker is alive when the frame goes. Its session ends
// without an event of its own; the window still ends auto-attach, clears
// the session's storage and lets a later navigation go.
func TestScienceBrowserNestedWorkerDoesNotHoldTheWindow(t *testing.T) {
	z := newScienceZombieEnv(t)
	z.shell.close(t)
	waitFor(t, 10*time.Second, "auto-attach to end", func() bool { return !z.shell.pageAttached() })
	waitFor(t, 10*time.Second, "the origin cleared", func() bool { return z.shell.cleared.Load() > 0 })
	z.shell.navigateTop(t, z.harness.origin+"/slow")
	if ended := z.shell.ended(); ended != 0 {
		t.Fatalf("the shell ended %d frames", ended)
	}
}
