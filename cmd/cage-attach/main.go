// Command cage-attach is the program behind cage attach: it shows a Cage
// terminal in the terminal application it runs in, with the same flags and
// exit codes as cage attach, which runs it when it is installed beside cage.
// Its second mode is cage entry proxy (cage-attach entry proxy
// <environment>), the ProxyCommand of the ssh Host blocks of Super Cage's
// local entry: one runs for as long as each ssh connection lasts.
//
// People keep attached windows and ssh connections open for hours, several
// at a time, so it links only what a viewer needs: the controller checks
// and session (internal/controlclient), a minimal HTTP and WebSocket client
// (internal/controlwire), the escape filter, the tty, the image paste and
// the entry's proxy (internal/entryproxy). Never internal/workbench,
// net/http or gorilla/websocket (main_test.go): net/http's initialisation
// alone would add about 4 MB to every window.
// Packaging builds it with CGO_ENABLED=0.
package main

import (
	"context"
	"os"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/version"
)

func main() {
	// It is a different executable from the cage it ships with: it accepts
	// only a controller with its build stamp that verified this very file
	// beside itself (controlclient.SameCompanion).
	check := controlclient.SameCompanion(version.Stamp)
	if len(os.Args) > 1 && os.Args[1] == "entry" {
		config, err := entryproxy.DefaultConfig(os.Getenv(attach.ConfigEnv))
		if err != nil {
			os.Stderr.WriteString("Super Cage：" + err.Error() + "\n")
			os.Exit(entryproxy.Failure)
		}
		os.Exit(entryproxy.Main(context.Background(), os.Args[2:], config, check, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(attach.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, attach.Options{Check: check}))
}
