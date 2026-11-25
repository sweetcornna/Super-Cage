package cage

import (
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/agents"
)

// Guarantee G (the 其他客户端 design, 9.1, "Guest revision"): adding other clients to an
// environment must not change the guest revision, because a changed revision
// closes the network gate on every guest, Claude-only ones included, until
// 更新环境组件 (internal/workbench/environment_versions.go). The revision hashes
// guestDependencies, guestEnv, componentStartScript() and ToolInstallPrelude()
// and, through them, every script the start script embeds (guest.go).

// guestRevisionAtOtherClientsBase is guestRevision() at the last intentional
// guest component update (updated for Antigravity OAuth snapshot/login-host
// fixes in agent-runtime.py and browser-login.mjs, which ship via 更新环境组件).
// If this fails you changed a hashed input: either undo it, or, if the change
// is a product decision that is meant to ship with 更新环境组件, set the new
// value here in the same commit and say so in the message. Work on 其他客户端
// never does.
const guestRevisionAtOtherClientsBase = "7fb22a21070068b38c8fffedf0dad116cd42b99119e57ab681b607f059b1c60d"

func TestGuestRevisionIsPinnedForOtherClients(t *testing.T) {
	if got := guestRevision(); got != guestRevisionAtOtherClientsBase {
		t.Fatalf("the guest revision changed: %s (pinned %s). Some script or file hashed into it was edited (see guest.go guestRevision); every environment would need 更新环境组件 with its network closed", got, guestRevisionAtOtherClientsBase)
	}
	if got := RequiredGuestManifest().Revision; got != guestRevisionAtOtherClientsBase {
		t.Fatalf("RequiredGuestManifest().Revision = %s, pinned %s", got, guestRevisionAtOtherClientsBase)
	}
}

// Nothing of the feature may be in what the revision hashes: its paths, its
// installer, its progress lines, and the pinned downloads of the catalog's
// optional entries (a digest or an address in a hashed script would mean an
// entry travels with the start script instead of with its install job).
func TestGuestRevisionInputsCarryNoOtherClientsFeature(t *testing.T) {
	markers := []string{
		"agent-clients", "agent-install", "agent_install", "AGENT_ID", "CAGE_AGENT_", "super-cage-agent-launcher",
		"/usr/local/lib/agent-clients", "/var/lib/agent-clients", "agent-catalog", "agents_policy",
	}
	entries, err := agents.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Optional() {
			continue
		}
		for _, artifact := range entry.Artifacts {
			for _, pin := range []string{artifact.SHA256, artifact.SHA512, artifact.URL} {
				if pin != "" {
					markers = append(markers, pin)
				}
			}
		}
	}
	inputs := map[string]string{
		"guestDependencies":    guestDependencies,
		"guestEnv":             guestEnv,
		"componentStartScript": componentStartScript(),
		"ToolInstallPrelude":   ToolInstallPrelude(),
		"StartScript":          DefaultConfig().StartScript(),
		"GuestSetup":           GuestSetup,
	}
	for name, text := range inputs {
		if text == "" {
			t.Errorf("%s is empty: the check would pass for nothing", name)
		}
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				t.Errorf("%s (hashed into the guest revision) holds %q", name, marker)
			}
		}
	}
}
