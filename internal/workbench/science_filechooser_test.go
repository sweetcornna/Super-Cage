package workbench

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// R5: while armed, a file input in the page or in a frame of its process
// never opens the host's file chooser; the page sees a cancel. Disarming
// gives the chooser back to the workbench.
func TestScienceBrowserFileChooserIntercepted(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><body style=margin:0><input type=file id=f style=width:100vw;height:100vh><script>"+
			"for (const name of ['change', 'cancel']) f.addEventListener(name, () => parent.postMessage(name, '*'));</script>")
	}))
	t.Cleanup(front.Close)
	workbench := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><body style=margin:0><iframe src=%q style=border:0;width:400px;height:300px></iframe>"+
			"<input type=file id=own><script>window.events=[];addEventListener('message', (e) => events.push('frame:' + e.data));"+
			"for (const name of ['change', 'cancel']) own.addEventListener(name, () => events.push('page:' + name));</script>", front.URL)
	}))
	t.Cleanup(workbench.Close)
	browser := launchTestBrowser(t, scienceHostEnv)
	session := browser.page(t)
	var opened atomic.Int32
	browser.on(func(m cdpMessage) {
		if m.Method == "Page.fileChooserOpened" && m.SessionID == session {
			opened.Add(1)
		}
	})
	if err := browser.call(session, "Page.navigate", map[string]any{"url": workbench.URL}, nil); err != nil {
		t.Fatal(err)
	}
	eval := func(expression string) any {
		var result struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
		}
		if err := browser.call(session, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "userGesture": true}, &result); err != nil {
			t.Fatal(err)
		}
		return result.Result.Value
	}
	waitFor(t, 10*time.Second, "the page", func() bool { return eval("typeof events") == "object" })
	clickFrame := func() {
		for _, kind := range []string{"mousePressed", "mouseReleased"} {
			if err := browser.call(session, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": 100, "y": 100, "button": "left", "clickCount": 1}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	events := func() string { return fmt.Sprint(eval("events.join(' ')")) }

	browser.armIdentity(t, session, testScienceIdentity, true)
	eval("own.click()")
	waitFor(t, 5*time.Second, "the page's chooser intercepted", func() bool { return opened.Load() == 1 && events() == "page:cancel" })
	clickFrame()
	waitFor(t, 5*time.Second, "the frame's chooser intercepted", func() bool { return opened.Load() == 2 && events() == "page:cancel frame:cancel" })

	for _, call := range scienceDisarmCalls() {
		if err := browser.call(session, call.Method, call.Params, nil); err != nil {
			t.Fatal(err)
		}
	}
	eval("events.length = 0")
	eval("own.click()")
	clickFrame()
	// Headless Chromium cancels a chooser it would show; the protocol no
	// longer hears of it.
	waitFor(t, 5*time.Second, "both choosers back to the browser", func() bool { return events() == "page:cancel frame:cancel" })
	if opened.Load() != 2 {
		t.Fatalf("intercepted after disarm: %d", opened.Load())
	}
}
