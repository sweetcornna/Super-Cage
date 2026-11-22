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

	"github.com/sylvinus/agent-vm/internal/entryproxy"
)

type entryRouteRecorder struct{ patterns []string }

func (r *entryRouteRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

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
	if view.Enabled || view.State != "off" || len(view.Devices) != 0 || view.HostAlias != "" || view.Include != nil {
		t.Fatalf("status while off: %+v", view)
	}
	// The Include preview reads ~/.ssh/config for the dialog that turns
	// the entry on; it writes nothing (checked by the snapshot below).
	var include entryInclude
	if err := f.call("GET", "/api/entry/include", nil, &include); err != nil || include.Present || include.Token == "" || include.Line == "" {
		t.Fatalf("include preview while off: %+v %v", include, err)
	}
	// Walk every route the entry registers, so a route added later is
	// covered too. Only POST /api/entry/on may change anything: it is the
	// action that turns the entry on.
	recorder := &entryRouteRecorder{}
	f.s.entryRoutes(recorder)
	if len(recorder.patterns) < 9 {
		t.Fatalf("recorded routes %v", recorder.patterns)
	}
	device := strings.Repeat("ab", 16)
	for _, pattern := range recorder.patterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{id}", device)
		var refusal *entryRefusal
		switch pattern {
		case "POST /api/entry/on":
			continue
		case "GET /api/entry", "GET /api/entry/include":
			if err := f.call(method, path, nil, nil); err != nil {
				t.Fatalf("%s while off: %v", pattern, err)
			}
		case "GET /api/entry/ssh":
			if _, _, err := entryproxy.Dial(context.Background(), f.s.configPath, device, agentSameBuild); !errors.As(err, &refusal) || refusal.Code != errEntryOff.Code || refusal.Status != http.StatusConflict {
				t.Fatalf("dial while off: %v", err)
			}
		case "POST /api/terminals/{id}/keep":
			// A terminal action, not the entry's: no ssh terminal exists.
			if err := f.call(method, path, map[string]any{}, nil); !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
				t.Fatalf("keep while off: %v", err)
			}
		default:
			input := map[string]string{}
			if strings.HasSuffix(path, "/rename") {
				input["name"] = "x"
			}
			if strings.HasSuffix(path, "/include") {
				input["token"] = "x"
			}
			if err := f.call(method, path, input, nil); !errors.As(err, &refusal) || refusal.Code != errEntryOff.Code {
				t.Fatalf("%s while off: %v", pattern, err)
			}
		}
	}
	var stderr bytes.Buffer
	if code := entryproxy.Run(context.Background(), entryproxy.Config{ConfigPath: f.s.configPath, Environment: defaultLocalVMID, Paths: f.proxyPaths(), Check: agentSameBuild}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != entryProxyFailure || !strings.HasPrefix(stderr.String(), "Super Cage：") {
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

// The entry has no listener, server or idle timer of its own: its routes sit
// on the workbench's existing handler. Its only timers are bounded and live
// within one connection: the SSH handshake deadline (entryServe) and the
// retry while an ssh terminal waits for the operation lock
// (entryLockOperation); and the idle check of kept ssh terminals (entryKeep,
// entryCheckKept), which runs only while a terminal is kept
// (TestEntryKeptTerminalsHaveTheirOwnCapAndExpire). This reads the entry's
// sources.
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
		for _, declaration := range parsed.Decls {
			function, _ := declaration.(*ast.FuncDecl)
			ast.Inspect(declaration, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := selector.Sel.Name
				timer := name == "NewTicker" || name == "Tick" || name == "AfterFunc" && selector.X.(*ast.Ident).Name == "time"
				bounded := map[string]string{"entryLockOperation": "NewTicker", "entryServe": "AfterFunc", "entryKeep": "AfterFunc", "entryCheckKept": "AfterFunc"}
				if timer && function != nil && bounded[function.Name.Name] == name {
					return true
				}
				if strings.HasPrefix(name, "Listen") || name == "Serve" || timer {
					t.Errorf("%s calls %s", source, name)
				}
				return true
			})
		}
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
