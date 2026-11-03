// Package quartz is a library for testing time related code.  It exports an interface Clock that
// mimics the standard library time package functions.  In production, an implementation that calls
// thru to the standard library is used.  In testing, a Mock clock is used to precisely control and
// intercept time functions.
package quartz

import (
	"context"
	"time"
)

type Clock interface {
	// NewTicker returns a new Ticker containing a channel that will send the current time on the
	// channel after each tick. The period of the ticks is specified by the duration argument. The
	// ticker will adjust the time interval or drop ticks to make up for slow receivers. The
	// duration d must be greater than zero; if not, NewTicker will panic. Stop the ticker to
	// release associated resources.
	NewTicker(d time.Duration, tags ...string) *Ticker
	// TickerFunc is a convenience function that calls f on the interval d until either the given
	// context expires or f returns an error.  Callers may call Wait() on the returned Waiter to
	// wait until this happens and obtain the error. The duration d must be greater than zero; if
	// not, TickerFunc will panic.
	TickerFunc(ctx context.Context, d time.Duration, f func() error, tags ...string) Waiter
	// NewTimer creates a new Timer that will send the current time on its channel after at least
	// duration d.
	NewTimer(d time.Duration, tags ...string) *Timer
	// AfterFunc waits for the duration to elapse and then calls f in its own goroutine. It returns
	// a Timer that can be used to cancel the call using its Stop method. The returned Timer's C
	// field is not used and will be nil.
	AfterFunc(d time.Duration, f func(), tags ...string) *Timer

	// Now returns the current local time.
	Now(tags ...string) time.Time
	// Since returns the time elapsed since t. It is shorthand for Clock.Now().Sub(t).
	Since(t time.Time, tags ...string) time.Duration
	// Until returns the duration until t. It is shorthand for t.Sub(Clock.Now()).
	Until(t time.Time, tags ...string) time.Duration

	// WithDeadline returns a copy of parent that expires when this Clock reaches d. When it
	// expires, Err satisfies errors.Is(err, context.DeadlineExceeded). Parent cancellation
	// propagates with stdlib semantics. Parent deadlines are not converted to mock time, so a
	// parent created with context.WithDeadline or context.WithTimeout can cancel the child
	// independently of this Clock. The returned cancel function is idempotent and must be called.
	//
	// With a Mock clock, Done and Err follow mock time, but ctx.Deadline() reports the parent's
	// deadline, or a time in year 9999 if the parent has none. Code outside the test compares
	// Deadline() with wall time, for example in net.Dialer, http.Client, and gRPC, and would treat
	// a mock deadline as already expired. Code that needs the remaining mock time must compute it
	// from the deadline or timeout it passed in, not from ctx.Deadline().
	WithDeadline(parent context.Context, d time.Time, tags ...string) (context.Context, context.CancelFunc)

	// WithDeadlineCause is like WithDeadline and records cause when this Clock reaches d.
	WithDeadlineCause(parent context.Context, d time.Time, cause error, tags ...string) (context.Context, context.CancelFunc)

	// WithTimeout returns a copy of parent that expires after d on this Clock. It is shorthand for
	// WithDeadline(parent, Clock.Now().Add(d)).
	WithTimeout(parent context.Context, d time.Duration, tags ...string) (context.Context, context.CancelFunc)

	// WithTimeoutCause is like WithTimeout and records cause when this Clock reaches the timeout.
	WithTimeoutCause(parent context.Context, d time.Duration, cause error, tags ...string) (context.Context, context.CancelFunc)
}

// Waiter can be waited on for an error.
type Waiter interface {
	Wait(tags ...string) error
}
