package cage

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed claude-science-install.sh
var claudeScienceInstall string

//go:embed claude-science-start.sh
var claudeScienceStart string

//go:embed browser-science.mjs
var browserScience string

// Claude Science listens on guest loopback only. Both ports are inside the
// ephemeral range that Lima never forwards (internal/vm ephemeralPorts) and
// are reserved below, so no program asking for any free port takes them.
const (
	ClaudeSciencePort        = 47800
	ClaudeScienceSandboxPort = 47801
	ClaudeScienceUnit        = "cage-claude-science.service"
)

// ClaudeSciencePinned is the reviewed build from tool-versions.env.
type ClaudeSciencePinned struct {
	Version, Build, SHA256 string
}

func ClaudeSciencePin() ClaudeSciencePinned {
	var pin ClaudeSciencePinned
	for _, line := range strings.Split(toolVersions, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "CAGE_CLAUDE_SCIENCE_VERSION":
			pin.Version = value
		case "CAGE_CLAUDE_SCIENCE_BUILD":
			pin.Build = value
		case "CAGE_CLAUDE_SCIENCE_AMD64_SHA256":
			pin.SHA256 = value
		}
	}
	return pin
}

// The browser agent imports this module at start, so it is written wherever
// the agent is (guestOpenScript), whether or not Claude Science is installed.
func browserScienceScript() string {
	return "sudo tee /usr/local/lib/browser-science.mjs >/dev/null <<'CAGE_BROWSER_SCIENCE'\n" + browserScience + "\nCAGE_BROWSER_SCIENCE\n"
}

func ClaudeScienceInstallScript() string {
	return ToolInstallPrelude() + claudeScienceInstall + "\n" + ClaudeScienceProvisionScript()
}

// The daemon unit runs as the guest user with the guest's own timezone and
// locale (the environment file) and the documented telemetry opt-out. It
// serves guest loopback only and is not enabled at boot: the user opens it.
var claudeScienceUnit = fmt.Sprintf(`[Unit]
Description=Claude Science in Cage
StartLimitIntervalSec=120
StartLimitBurst=3
[Service]
Type=simple
User=cage
WorkingDirectory=/home/cage
Environment=HOME=/home/cage
EnvironmentFile=/etc/claude-cage/environment
Environment=DO_NOT_TRACK=1
ExecStart=/usr/local/bin/cage-log-run /home/cage/.local/bin/claude-science serve --no-browser --no-auto-update --host 127.0.0.1 --port %d --sandbox-port %d
ExecStop=/home/cage/.local/bin/claude-science stop
LogNamespace=cage
KillMode=control-group
Restart=on-failure
RestartSec=5
TimeoutStopSec=20
`, ClaudeSciencePort, ClaudeScienceSandboxPort)

// Provisioning only writes files; it never starts or restarts the daemon.
func ClaudeScienceProvisionScript() string {
	return "set -euo pipefail\nsudo install -d -m 0755 /etc/claude-cage /etc/sysctl.d\n" +
		"sudo tee /usr/local/bin/cage-claude-science-start >/dev/null <<'CAGE_SCIENCE_START'\n" + claudeScienceStart + "\nCAGE_SCIENCE_START\n" +
		"sudo chmod 0755 /usr/local/bin/cage-claude-science-start\n" +
		// Read by the browser agent: it accepts sign-in links for this port only.
		fmt.Sprintf("printf '{\"port\":%d,\"sandbox_port\":%d}\\n' | sudo tee /etc/claude-cage/science.json >/dev/null\n", ClaudeSciencePort, ClaudeScienceSandboxPort) +
		"sudo chmod 0644 /etc/claude-cage/science.json\n" +
		fmt.Sprintf("printf 'net.ipv4.ip_local_reserved_ports = %d-%d\\n' | sudo tee /etc/sysctl.d/60-cage-claude-science.conf >/dev/null\n", ClaudeSciencePort, ClaudeScienceSandboxPort) +
		// A container may refuse the setting; a taken port is then reported at start.
		"sudo sysctl -q -p /etc/sysctl.d/60-cage-claude-science.conf >/dev/null 2>&1 || echo 'Claude Science 端口未能保留；若端口被占用，启动时会提示。' >&2\n" +
		"sudo tee /etc/systemd/system/" + ClaudeScienceUnit + " >/dev/null <<'CAGE_SCIENCE_UNIT'\n" + claudeScienceUnit + "CAGE_SCIENCE_UNIT\n" +
		"sudo systemctl daemon-reload\n"
}
