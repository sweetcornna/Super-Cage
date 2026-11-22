package workbench

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/version"
)

// Follow-ups of the review of controller-dial-hardening (A1, A4).

// A1: AgentStartService and ServerEntry use the proven connect: a
// controller from the release before the proof gets neither the token nor a
// launch request from them.
func TestAgentStartServiceRefusesOlderController(t *testing.T) {
	s := testServer(t)
	s.proofKey = ""
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	recording := startTestController(t, s, listener, legacyRecord(s))
	startErr := AgentStartService(t.Context(), s.configPath)
	_, entryErr := ServerEntry(s.configPath)
	if seen := recording.log(); strings.Contains(seen, s.token) || strings.Contains(seen, "/api/launch ") {
		t.Fatalf("an unproven controller was sent the token:\n%s", seen)
	}
	var agentErr *AgentError
	if !errors.As(startErr, &agentErr) || agentErr.Code != "controller_mismatch" || !strings.Contains(agentErr.Message, "需要更新") {
		t.Fatalf("AgentStartService: %#v", startErr)
	}
	// A server has no window to open: its own sentence.
	if !errors.Is(entryErr, errServerControllerOutdated) || strings.Contains(entryErr.Error(), "打开 Super Cage") || !strings.Contains(entryErr.Error(), "服务器") {
		t.Fatalf("ServerEntry: %v", entryErr)
	}
}

// A1: the grace for a controller without the proof has one list of the
// functions that still use it, and ends in a named release.
func TestOlderControllerGraceCallers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "connectControllerOrOlder" {
				continue
			}
			caller := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				receiver := fn.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if ident, ok := receiver.(*ast.Ident); ok {
					caller = ident.Name + "." + caller
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && (ident.Name == "connectController" || ident.Name == "openController") && !slices.Contains(found, caller) {
					found = append(found, caller)
				}
				return true
			})
		}
	}
	slices.Sort(found)
	listed := slices.Clone(olderControllerGraceCallers)
	slices.Sort(listed)
	if !slices.Equal(found, listed) {
		t.Fatalf("callers of the grace:\n  found  %v\n  listed %v", found, listed)
	}
	if !versionBelow(version.Version, olderControllerGraceEndsIn) {
		t.Fatalf("release %s: the grace for older controllers ends in %s; remove it", version.Version, olderControllerGraceEndsIn)
	}
}

func versionBelow(a, b string) bool {
	parse := func(v string) []int {
		var out []int
		for _, part := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
			n, _ := strconv.Atoi(part)
			out = append(out, n)
		}
		return out
	}
	return slices.Compare(parse(a), parse(b)) < 0
}

// failingListener stands for an IPv6 loopback listener whose Accept keeps
// failing with an error other than closed.
type failingListener struct {
	calls  atomic.Int32
	closed chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, errors.New("accept: too many open files")
	}
}
func (l *failingListener) Close() error   { close(l.closed); return nil }
func (l *failingListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv6loopback} }

// A4: a failing IPv6 twin backs off instead of spinning, and its error never
// reaches the server, which would stop serving IPv4 too.
func TestLoopbackPairBacksOffOnAFailingTwin(t *testing.T) {
	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v6 := &failingListener{closed: make(chan struct{})}
	pair := newLoopbackPair(v4, v6)
	defer pair.Close()
	go func() {
		if conn, err := net.Dial("tcp4", v4.Addr().String()); err == nil {
			defer conn.Close()
			time.Sleep(time.Second)
		}
	}()
	conn, err := pair.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	conn.Close()
	before := v6.calls.Load()
	time.Sleep(200 * time.Millisecond)
	if calls := v6.calls.Load() - before; calls > 20 {
		t.Fatalf("the IPv6 accept loop ran %d times in 200ms", calls)
	}
}
