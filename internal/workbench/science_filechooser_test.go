package workbench

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// R5: while the Science frame is shown, a file input in it never opens the
// host's file chooser; the frame sees a cancel. The workbench's own file
// inputs are not touched (the interception is on the frame's target only).
func TestScienceBrowserFileChooserIntercepted(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><body style=margin:0><input type=file id=f style=width:100vw;height:100vh><script>"+
			"for (const name of ['change', 'cancel']) f.addEventListener(name, () => parent.postMessage(name, '*'));</script>")
	}))
	front.Listener = listener
	front.Start()
	t.Cleanup(front.Close)
	frameURL := fmt.Sprintf("http://%s.localhost:%d/", "0123456789abcdef0123456789abcdef", listener.Addr().(*net.TCPAddr).Port)
	workbench := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><body style=margin:0><div id=pane></div><input type=file id=own><script>window.events=[];addEventListener('message', (e) => events.push('frame:' + e.data));"+
			"for (const name of ['change', 'cancel']) own.addEventListener(name, () => events.push('page:' + name));</script>")
	}))
	t.Cleanup(workbench.Close)
	browser := launchTestBrowser(t, scienceHostEnv)
	session := browser.page(t)
	var pageOpened, frameOpened atomic.Int32
	browser.on(func(m cdpMessage) {
		if m.Method != "Page.fileChooserOpened" {
			return
		}
		if m.SessionID == session {
			pageOpened.Add(1)
		} else {
			frameOpened.Add(1)
		}
	})
	shell := newScienceShell(t, browser, session)
	if err := browser.call(session, "Page.navigate", map[string]any{"url": workbench.URL}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the page", func() bool { return shell.eval(t, "typeof events") == "object" })
	shell.open(t, frameURL)
	waitFor(t, 10*time.Second, "the frame", func() bool {
		return frameOpened.Load() == 0 && shell.eval(t, "!!document.getElementById('science')") == true
	})
	time.Sleep(500 * time.Millisecond)
	clickFrame := func() {
		for _, kind := range []string{"mousePressed", "mouseReleased"} {
			if err := browser.call(session, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": 100, "y": 100, "button": "left", "clickCount": 1}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	events := func() string { return fmt.Sprint(shell.eval(t, "events.join(' ')")) }
	clickFrame()
	waitFor(t, 5*time.Second, "the frame's chooser intercepted", func() bool { return frameOpened.Load() == 1 && events() == "frame:cancel" })
	if err := browser.call(session, "Runtime.evaluate", map[string]any{"expression": "own.click()", "userGesture": true}, nil); err != nil {
		t.Fatal(err)
	}
	// Headless Chromium cancels a chooser it would show; the protocol does
	// not hear of the workbench's own.
	waitFor(t, 5*time.Second, "the workbench's chooser untouched", func() bool { return events() == "frame:cancel page:cancel" })
	if pageOpened.Load() != 0 || frameOpened.Load() != 1 {
		t.Fatalf("intercepted: page %d, frame %d", pageOpened.Load(), frameOpened.Load())
	}
}
