//go:build !darwin

package workbench

import (
	"context"
	"errors"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
)

// hostConditions is not read on this platform yet, which is "not now" for an
// unattended decision. Silent mode is unavailable here anyway.
func hostConditions(context.Context) (*policy.HostState, error) {
	return nil, errors.New("host power state is not read on this platform")
}
