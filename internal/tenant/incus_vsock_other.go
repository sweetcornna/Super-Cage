//go:build !linux

package tenant

import (
	"context"
	"errors"
	"net"
)

// dialVsockHost exists so the package builds everywhere; the Incus runtime
// itself only runs on Linux.
func dialVsockHost(context.Context, uint32, uint32) (net.Conn, error) {
	return nil, errors.New("vsock needs a Linux host")
}
