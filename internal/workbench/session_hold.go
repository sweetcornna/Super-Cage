package workbench

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Held session requests (the list and read protocols in mobile_cli_http.go)
// reach the workbench through the device link, whose response-header limit is
// 35 s, and the remote-host connector, whose limit is 30 s. So a request
// answers by its time whatever the guest does: every look at the guest ends
// then, even one waiting for the conversation lock behind a slow discovery,
// and the answer is built from what the server already knows.
//
//   - A held list (wait > 0) answers at arrival + wait (at most 25 s).
//   - Any other list answers within sessionLookLimit (25 s); before, a list
//     that arrived during a slow discovery waited for it and then for one of
//     its own, up to 2 x 20 s.
//   - A held read's first look gets until arrival + max(wait, heldReadFirstLook)
//     and later looks end at arrival + wait. Its resident wait asks the guest
//     to answer viewWaitMargin before the deadline and gives the reply
//     viewWaitSlack after it, so a read answers within 28 s.
//
// A list look cut short answers with the newest finished observation (share
// keeps going and stores it); a read look cut short answers 204. Neither is a
// guest or helper failure: errHoldDeadline marks it.

// errHoldDeadline is the cause of a look ending with its request's time.
var errHoldDeadline = errors.New("the held request's time is up")

// sessionLookLimit bounds the looks of a list request without wait.
var sessionLookLimit = sessionListWaitLimit

// heldReadFirstLook is the least time a held read's first look gets: without
// one finished read, a short wait on a slow guest would never see a change.
var heldReadFirstLook = 15 * time.Second

const (
	viewWaitMargin = time.Second
	viewWaitSlack  = 3 * time.Second
)

func holdContext(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadlineCause(parent, deadline, errHoldDeadline)
}

// holdExpired reports whether ctx, or a context it derives from, ended with
// its request's time.
func holdExpired(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errHoldDeadline)
}

// readSessionBy is authorizedMobileSession for a held read that must answer
// by limit. reached is false when the read did not finish by then; nothing is
// written to w in that case, and the read, left to finish in the background
// with an ended context, starts no guest command.
func (s *Server) readSessionBy(w http.ResponseWriter, r *http.Request, limit time.Time) (view mobileSession, ok, reached bool) {
	ctx, cancel := holdContext(r.Context(), limit)
	defer cancel()
	type result struct {
		view  mobileSession
		ok    bool
		reply *phoneBuffer
	}
	done := make(chan result, 1)
	bounded := r.WithContext(ctx)
	go func() {
		reply := newPhoneBuffer()
		view, ok := s.authorizedMobileSession(reply, bounded)
		done <- result{view, ok, reply}
	}()
	select {
	case got := <-done:
		if !got.ok && ctx.Err() != nil {
			return mobileSession{}, false, false
		}
		if !got.ok {
			got.reply.copyTo(w)
		}
		return got.view, got.ok, true
	case <-ctx.Done():
		return mobileSession{}, false, false
	}
}
