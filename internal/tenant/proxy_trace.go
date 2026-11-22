package tenant

import (
	"context"
	"net/http/httptrace"
	"sync/atomic"
)

// reuseTrace attaches a client trace that records whether the transport sent
// the request on a connection that had been used before. The request reads the
// flag after the round trip: a transport that retries reports its last attempt.
func reuseTrace(ctx context.Context) (context.Context, *atomic.Bool) {
	reused := new(atomic.Bool)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
	}), reused
}
