//go:build windows

package localterm

import (
	"context"
	"errors"
)

// ExecRunner arrives with the Windows table (design step 5).
type ExecRunner struct{}

func (ExecRunner) Run(context.Context, Step) error {
	return errors.New("本机终端应用还不支持 Windows")
}
