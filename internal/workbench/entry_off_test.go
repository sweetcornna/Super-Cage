//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entrySnapshot lists every file and directory under the roots.
func entrySnapshot(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, err := entry.Info()
			if err == nil {
				files[path] = info.Mode().String() + " " + info.ModTime().String()
			}
			return nil
		})
	}
	return files
}

// Design 5.6: the entry is off by default, and while it is off nothing is
// written, run or contacted: no key, no ssh file, no wrapper, no guest
// command, no connection.
func TestEntryOffByDefaultWritesAndRunsNothing(t *testing.T) {
	f := newEntryFixture(t, true)
	client := filepath.Join(f.dir, "client")
	before := entrySnapshot(t, client, entryDir(f.s.profile))
	var view entryStatusView
	if err := f.call("GET", "/api/entry", nil, &view); err != nil {
		t.Fatal(err)
	}
	if view.Enabled || view.State != "off" || len(view.Devices) != 0 || view.HostAlias != "" || view.Include != nil || view.Herdr {
		t.Fatalf("status while off: %+v", view)
	}
	device := strings.Repeat("ab", 16)
	for _, route := range []struct {
		method, path string
		input        any
	}{
		{"POST", "/api/entry/off", map[string]any{}},
		{"GET", "/api/entry/include", nil},
		{"POST", "/api/entry/include", map[string]string{"token": "x"}},
		{"POST", "/api/entry/devices/" + device + "/revoke", map[string]any{}},
		{"POST", "/api/entry/devices/" + device + "/rename", map[string]string{"name": "x"}},
	} {
		var refusal *entryRefusal
		if err := f.call(route.method, route.path, route.input, nil); !errors.As(err, &refusal) || refusal.Code != errEntryOff.Code {
			t.Fatalf("%s %s while off: %v", route.method, route.path, err)
		}
	}
	if _, _, err := entryDial(context.Background(), f.s.configPath, device); !errors.Is(err, error(nil)) {
		var refusal *entryRefusal
		if !errors.As(err, &refusal) || refusal.Code != errEntryOff.Code || refusal.Status != http.StatusConflict {
			t.Fatalf("dial while off: %v", err)
		}
	} else {
		t.Fatal("a dial while off was accepted")
	}
	var stderr bytes.Buffer
	if code := entryProxy(context.Background(), f.s.configPath, defaultLocalVMID, f.paths, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != entryProxyFailure || !strings.HasPrefix(stderr.String(), "Super Cage：") {
		t.Fatalf("proxy while off: %d %q", code, stderr.String())
	}
	if _, err := entrySSHCommand(f.paths, defaultLocalVMID, nil); err == nil {
		t.Fatal("cage ssh while off")
	}
	// The environment's own events with the entry off.
	f.s.entryGuestStarting()
	_ = f.s.pauseNetworkNow("测试暂停")
	f.s.entryEnvironmentStopped()
	after := entrySnapshot(t, client, entryDir(f.s.profile))
	if len(after) != len(before) {
		t.Fatalf("files while off: before %v after %v", before, after)
	}
	for path, stamp := range before {
		if after[path] != stamp {
			t.Fatalf("%s changed while off", path)
		}
	}
	if f.installs.Load() != 0 || f.guest.accepted.Load() != 0 || f.s.entry.registry.counts().Total != 0 {
		t.Fatalf("while off: %d wrapper installs, %d guest connections, %+v", f.installs.Load(), f.guest.accepted.Load(), f.s.entry.registry.counts())
	}
}

// The entry has no listener, process or timer of its own: its routes sit on
// the workbench's existing handler. This reads the entry's sources.
func TestEntryHasNoListenerOfItsOwn(t *testing.T) {
	sources, _ := filepath.Glob("entry_*.go")
	files := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		files++
		parsed, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if name := selector.Sel.Name; strings.HasPrefix(name, "Listen") || name == "Serve" || name == "NewTicker" || name == "Tick" {
					t.Errorf("%s calls %s", source, name)
				}
			}
			return true
		})
	}
	if files < 5 {
		t.Fatalf("read %d entry sources", files)
	}
}

// Design 5.6: the routes answer only the trusted local view; a phone or a
// remote desktop with a valid session gets local_only, a desktop entry or
// client-only server has no such routes.
func TestEntryRoutesAnswerOnlyTheTrustedLocalView(t *testing.T) {
	s := testServer(t)
	routes := [][2]string{{"GET", "/api/entry"}, {"POST", "/api/entry/on"}, {"POST", "/api/entry/off"}, {"GET", "/api/entry/include"}, {"POST", "/api/entry/include"}, {"POST", "/api/entry/devices/x/revoke"}, {"POST", "/api/entry/devices/x/rename"}, {"GET", "/api/entry/ssh"}}
	call := func(method, path string, trusted bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, s.origin+path, strings.NewReader("{}"))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		if trusted {
			r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	for _, route := range routes {
		if w := call(route[0], route[1], false); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "local_only") {
			t.Fatalf("%v without the trusted view: %d %s", route, w.Code, w.Body)
		}
		if w := request(s, route[0], route[1], "", "{}", false); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("%v without a session: %d", route, w.Code)
		}
		if w := request(s, route[0], route[1], "https://attacker.example", "{}", true); w.Code != http.StatusForbidden {
			t.Fatalf("%v cross-origin: %d", route, w.Code)
		}
	}
	if w := call("GET", "/api/entry", true); w.Code != http.StatusOK {
		t.Fatalf("trusted view: %d %s", w.Code, w.Body)
	}
	s.clientOnly = true
	for _, route := range routes[:7] {
		if w := call(route[0], route[1], true); w.Code != http.StatusNotFound {
			t.Fatalf("%v on a client-only server: %d", route, w.Code)
		}
	}
	s.clientOnly = false
	s.account.lock()
	if w := call("GET", "/api/entry", true); w.Code != http.StatusLocked {
		t.Fatalf("locked: %d", w.Code)
	}
}

var _ = os.Getpid
