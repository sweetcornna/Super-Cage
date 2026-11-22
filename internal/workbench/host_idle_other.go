//go:build !darwin && !windows

package workbench

import (
	"context"
	"errors"
	"time"
)

// hostInputIdle has no portable source here; silent updates are not offered
// on these platforms, and unknown always reads as "someone may be here".
func hostInputIdle(context.Context) (time.Duration, error) {
	return 0, errors.New("host input idle time is not available on this platform")
}
