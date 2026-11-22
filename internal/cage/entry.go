package cage

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strings"
)

// The local SSH entry ("本机终端") runs its exec, shell and SFTP channels in
// the guest through one small root-owned script. It is not part of the
// component start script: adding it there would change the guest revision
// (guestRevision) and ask every existing guest for a component update, also
// on computers that never turn the entry on. The controller installs it on
// demand instead, the first time a channel needs it after a guest start, so a
// guest whose entry stays off never receives it.

//go:embed entry-run.sh
var entryRun string

// EntryRunPath is where the entry's channel script lives in a Linux guest.
const EntryRunPath = "/usr/local/lib/cage-entry/run"

// EntryRunScript is the script installed at EntryRunPath.
func EntryRunScript() string { return entryRun }

// EntryRunDigest identifies the script, so an installed copy that matches is
// left alone.
func EntryRunDigest() string { return fmt.Sprintf("%x", sha256.Sum256([]byte(entryRun))) }

// EntryInstallScript installs the channel script at path when the copy there
// differs. It runs as the guest user through the controller's control hop and
// uses sudo like the component start script, so the script ends up owned by
// root. It prints nothing on success.
func EntryInstallScript(path string) string {
	directory := path[:strings.LastIndex(path, "/")]
	return "set -eu\n" +
		"want=" + EntryRunDigest() + "\n" +
		"have=$(sha256sum " + path + " 2>/dev/null | cut -d' ' -f1 || true)\n" +
		"if [ \"$have\" = \"$want\" ]; then exit 0; fi\n" +
		"tmp=$(mktemp)\n" +
		"trap 'rm -f \"$tmp\"' EXIT\n" +
		"cat >\"$tmp\" <<'CAGE_ENTRY_RUN'\n" + entryRun + "CAGE_ENTRY_RUN\n" +
		"sudo install -d -o root -g root -m 0755 " + directory + "\n" +
		"sudo install -o root -g root -m 0755 \"$tmp\" " + path + "\n"
}
