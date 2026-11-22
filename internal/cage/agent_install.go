package cage

import _ "embed"

// agentInstall is the guest side of 设置 → 其他客户端: install, update and
// remove of one optional agent client (docs/agent-install.md).
//
//go:embed agent-install.sh
var agentInstall string

// AgentInstallScript is the whole guest script of one 其他客户端 job: the tool
// prelude (cage_download_tool), the job's input from agents.ScriptInput, then
// the fixed installer. It travels with each job and is never part of a script
// the guest revision hashes (guestRevision), so adding a client never closes
// the network gate of every guest.
func AgentInstallScript(input string) string {
	return "set -euo pipefail\n" + ToolInstallPrelude() + "\n" + input + "\n" + agentInstall
}
