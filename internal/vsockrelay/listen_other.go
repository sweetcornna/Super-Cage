//go:build !linux

package vsockrelay

import "errors"

// ListenVsock exists so the package builds everywhere; the relay runs in
// Linux workspace VMs only.
func ListenVsock(uint32) (Listener, error) {
	return nil, errors.New("vsock needs a Linux guest")
}
