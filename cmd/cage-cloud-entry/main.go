// Command cage-cloud-entry is the ProxyCommand of the ssh Host blocks of
// Super Cage's cloud workspaces (cage-cloud-entry entry proxy cloud-<vm id>):
// one runs for as long as each ssh connection to a cloud workspace lasts,
// and it opens that connection to the portal's gateway over TLS
// (internal/cloudproxy).
//
// It is not part of cage-attach because TLS is: crypto/x509 makes macOS load
// the Security and CoreFoundation frameworks at launch, which would add about
// 4.5 MB to every attached window and local ssh connection. It links neither
// net/http nor gorilla/websocket nor x/crypto/ssh (main_test.go).
// Packaging builds it with CGO_ENABLED=0, beside cage.
package main

import (
	"context"
	"os"

	"github.com/sylvinus/agent-vm/internal/cloudproxy"
)

func main() {
	os.Exit(cloudproxy.Main(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
