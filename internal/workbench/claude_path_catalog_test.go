package workbench

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/agents"
	"github.com/sylvinus/agent-vm/internal/cage"
)

// Guarantee G (design 9.1): the catalog of other clients is not parsed on the
// Claude path. The parse counter is process-wide and other tests of this
// package read the catalog on purpose, so the check runs in a fresh process: the
// test starts itself with the probe variable set and reads what that run says.
const catalogProbeEnv = "CAGE_TEST_CATALOG_PROBE"

func TestClaudeOnlyPathNeverParsesTheCatalog(t *testing.T) {
	if os.Getenv(catalogProbeEnv) != "" {
		claudeOnlyPathProbe(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestClaudeOnlyPathNeverParsesTheCatalog$", "-test.count=1", "-test.v")
	command.Env = append(os.Environ(), catalogProbeEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the probe run failed: %v\n%s", err, output)
	}
	// A run that matched no test also exits 0: require the probe's own words.
	for _, want := range []string{"parses on the Claude path: 0", "after one read of the list: 1", "--- PASS: TestClaudeOnlyPathNeverParsesTheCatalog"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("the probe did not say %q:\n%s", want, output)
		}
	}
}

// claudeOnlyPathProbe is what a Claude-only environment does on the host without
// a guest: the server exists, /status is read, the required guest manifest is
// derived, the attach read's presence part is built, and the answer of a Claude-only
// guest goes through the code that consumes and records it (parseGuestConnection,
// recordClientPresence), then through the loader the next start reads
// (loadClientPresence). Booting the guest and reading it over ssh are NOT
// exercised here; the only importers of internal/agents are pinned by
// TestOnlyTheOptionalClientCodeImportsTheCatalog in that package.
func claudeOnlyPathProbe(t *testing.T) {
	if n := agents.Decodes(); n != 0 {
		t.Fatalf("the catalog was parsed (%d) before the probe began: this process is not clean", n)
	}
	s := testServer(t)
	for range 5 {
		response := httptest.NewRecorder()
		s.status(response, httptest.NewRequest("GET", "/api/status", nil))
		if response.Code != 200 {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
	}
	if script := clientPresenceProbe(clientReceiptDir); !strings.Contains(script, clientPresenceMarker) {
		t.Fatal("the presence probe lost its marker")
	}
	manifest, err := json.Marshal(cage.RequiredGuestManifest())
	if err != nil {
		t.Fatal(err)
	}
	read := "{\"token\":\"guest-test\"}\npassword\n" + clientPresenceMarker + " present= receipts=\n" + string(manifest) + "\n"
	credentials, current, presence, err := parseGuestConnection([]byte(read))
	if err != nil || current == nil || credentials.Token != "guest-test" || !presence.Known || len(presence.Clients) != 0 {
		t.Fatalf("the attach read of a Claude-only guest was not accepted: %v %+v", err, presence)
	}
	s.recordClientPresence(presence)
	if stored := loadClientPresence(s.profile); !stored.Known || len(stored.Clients) != 0 {
		t.Fatalf("the stored reading of a Claude-only guest was not read back: %+v", stored)
	}
	response := httptest.NewRecorder()
	s.status(response, httptest.NewRequest("GET", "/api/status", nil))
	if response.Code != 200 {
		t.Fatalf("status %d after the presence was known: %s", response.Code, response.Body.String())
	}
	got := agents.Decodes()
	t.Logf("catalog parses on the Claude path: %d", got)
	if got != 0 {
		t.Fatalf("the Claude path parsed the catalog %d times (design 9.1)", got)
	}
	// Control: the counter does count, so the zero above is a reading and not a dead wire.
	if _, err := agents.All(); err != nil {
		t.Fatal(err)
	}
	if after := agents.Decodes(); after != 1 {
		t.Fatalf("one read of the list gave %d parses, want 1", after)
	}
	t.Logf("catalog parses after one read of the list: %d", agents.Decodes())
}
