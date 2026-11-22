package workbench

import "github.com/sylvinus/agent-vm/internal/entryproxy"

// cage entry proxy, the ProxyCommand of the generated Host blocks, is
// internal/entryproxy: the slim cage-attach runs it (cage-attach entry proxy
// <environment>), so an ssh connection keeps only that small program
// resident, and cage runs the same code when no cage-attach is beside it.
// The refusals and their words are shared with it.

const entryProxyFailure = entryproxy.Failure

// entryRefusal is why the entry refuses a dial or cut a connection, in the
// owner's words; the proxy prints Message on stderr and exits 255.
type entryRefusal = entryproxy.Refusal

// entryClientError turns what the controller connection or the dial said
// into the owner's words.
func entryClientError(err error) string { return entryproxy.ClientError(err) }
