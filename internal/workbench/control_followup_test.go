package workbench

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// A1: the pre-0.1.16 grace for controllers without the identity proof ended in
// 0.1.19. Every workbench caller now requires proof, and openController refuses
// any control.json record without auth.
func TestOlderControllerGraceCallers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if node.Name == "connectController" || node.Name == "connectControllerOrOlder" {
					t.Fatalf("%s: retired unproven controller helper %s must not return", name, node.Name)
				}
			case *ast.SelectorExpr:
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "controlclient" && node.Sel.Name == "Connect" {
					t.Fatalf("%s: use controlclient.ConnectProven instead of controlclient.Connect", name)
				}
			}
			return true
		})
	}
	profile := t.TempDir()
	if err := writePrivate(filepath.Join(profile, "control.json"), controlFile{Port: 17777, Token: strings.Repeat("a", 64), PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := openController(profile); !errors.Is(err, errControllerOutdated) {
		t.Fatalf("openController accepted a record without auth: %v", err)
	}
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
