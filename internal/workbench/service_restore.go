package workbench

import (
	"context"
	"errors"
	"fmt"
)

// restoreWorkbenchToolServices is called after readiness and account unlock,
// with no operation lock held by its caller. An enabled integration without
// a saved run intent is lazy: opening it from the workbench starts it.
func (s *Server) restoreWorkbenchToolServices(ctx context.Context) error {
	if s.windowsGuest != nil {
		return nil
	}
	ctx, cancel, err := s.workbenchToolContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	var failures error
	for _, tool := range []struct {
		name    string
		restore func(context.Context) error
	}{
		{"Magpie", s.restoreMagpie},
		{"T3 Code", s.restoreT3Code},
		{"Herdr", s.restoreHerdr},
	} {
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		if err := tool.restore(ctx); err != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", tool.name, err))
		}
	}
	return failures
}

// Services starting in the guest share the account unlock epoch and the
// environment lifecycle. Call this again after taking s.op if waiting for it.
func (s *Server) workbenchToolContext(parent context.Context) (context.Context, func(), error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	ready := s.ready && !s.closing && !s.appUpdateSealed
	s.mu.Unlock()
	if !ready {
		return nil, nil, errTerminalNotReady
	}
	epoch, unlocked := s.account.access()
	if requested, _ := parent.Value(accountEpochKey{}).(context.Context); !unlocked || requested != nil && requested != epoch {
		return nil, nil, errClaudeAccessLost
	}
	ctx, cancel := context.WithCancel(context.WithValue(parent, accountEpochKey{}, epoch))
	stop := context.AfterFunc(epoch, cancel)
	return ctx, func() { stop(); cancel() }, nil
}
