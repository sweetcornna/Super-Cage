package workbench

import "errors"

// A monitoring read of the guest fails in one of two ways, and the network
// gate treats them differently:
//
//   - no answer (noAnswer): the read could not be made or returned nothing
//     usable. It timed out, could not reach the guest, lost its connection,
//     was killed, or replied with something unreadable. A monitoring round
//     retries it within the current lease; nothing is renewed, so the lease
//     still closes the gate unless a later round confirms the guest.
//   - wrong answer (wrongAnswer): the guest, or the host tool reading its
//     policy, answered and the answer is wrong: an empty or invalid setting,
//     a stage the guest reported as failed, an identity or a network policy
//     that is not the expected one. It closes the gate at once.
//
// Any other error counts as a wrong answer, so an error site nobody
// classified fails closed. TestGuestReadErrorSites lists every site.
type guestReadError struct {
	err   error
	wrong bool
}

func (e *guestReadError) Error() string { return e.err.Error() }
func (e *guestReadError) Unwrap() error { return e.err }

// noAnswer marks err as a read that returned no answer.
func noAnswer(err error) error { return &guestReadError{err: err} }

// wrongAnswer marks err as a read whose answer is wrong.
func wrongAnswer(err error) error { return &guestReadError{err: err, wrong: true} }

// unanswered reports whether err is a read that returned no answer. Wrong
// answers and unclassified errors are not.
func unanswered(err error) bool {
	var read *guestReadError
	return errors.As(err, &read) && !read.wrong
}
