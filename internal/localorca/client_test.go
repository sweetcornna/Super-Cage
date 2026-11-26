package localorca

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestClientEnvironmentListAddRemoveAndPortScan(t *testing.T) {
	var seen [][]string
	c := NewClient(Installation{Installed: true, CLIPath: "/fake/orca", Version: "1.4.277"})
	c.Run = func(_ context.Context, bin string, args ...string) ([]byte, error) {
		if bin != "/fake/orca" {
			t.Fatalf("unexpected binary %q", bin)
		}
		seen = append(seen, slices.Clone(args))
		switch {
		case len(args) >= 2 && args[0] == "environment" && args[1] == "list":
			return []byte(`{"id":"local","ok":true,"result":{"environments":[{"id":"env-1","name":"lab1-cage","endpoints":[{"id":"ws-1","kind":"websocket","endpoint":"ws://127.0.0.1:16768"}]}]}}`), nil
		case len(args) >= 2 && args[0] == "environment" && args[1] == "add":
			return []byte(`{"id":"local","ok":true,"result":{"environment":{"id":"env-2","name":"Super Cage","endpoints":[{"id":"ws-2","kind":"websocket","endpoint":"ws://127.0.0.1:16769"}]}}}`), nil
		case len(args) >= 2 && args[0] == "environment" && args[1] == "rm":
			return []byte(`{"id":"local","ok":true,"result":{"removed":true}}`), nil
		case len(args) >= 1 && args[0] == "open":
			return []byte(`{"id":"local","ok":true,"result":{"opened":true}}`), nil
		default:
			return nil, errors.New("unexpected args")
		}
	}

	ports := c.UsedLoopbackPorts(context.Background())
	if !ports[16768] || ports[16769] {
		t.Fatalf("unexpected used loopback ports: %+v", ports)
	}

	found, err := c.FindEnvironmentByEndpoint(context.Background(), "ws://localhost:16768/")
	if err != nil || found == nil || found.ID != "env-1" {
		t.Fatalf("FindEnvironmentByEndpoint failed: %+v %v", found, err)
	}

	added, err := c.AddEnvironment(context.Background(), "Super Cage (work)", "orca://pair?code=abc123&endpoint=ws%3A%2F%2F127.0.0.1%3A16769")
	if err != nil || added.ID != "env-2" {
		t.Fatalf("AddEnvironment failed: %+v %v", added, err)
	}

	if _, err := c.AddEnvironment(context.Background(), "--flag", "orca://pair?code=abc"); err == nil {
		t.Fatal("expected flag-like environment name to be rejected")
	}
	if _, err := c.AddEnvironment(context.Background(), "Good", "http://evil.example/"); err == nil {
		t.Fatal("expected non-orca pairing code to be rejected")
	}
	if err := c.RemoveEnvironment(context.Background(), "env-2"); err != nil {
		t.Fatalf("RemoveEnvironment failed: %v", err)
	}
	if err := c.OpenDesktop(context.Background()); err != nil {
		t.Fatalf("OpenDesktop failed: %v", err)
	}
	if len(seen) < 5 {
		t.Fatalf("expected >=5 CLI invocations, got %d", len(seen))
	}
}
