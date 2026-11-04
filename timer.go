package quartz

import (
	"time"
)

// The Timer type represents a single event. When the Timer expires, the current time will be sent
// on C, unless the Timer was created by AfterFunc. A Timer must be created with NewTimer or
// AfterFunc.
type Timer struct {
	C <-chan time.Time
	//nolint: revive
	c       chan time.Time
	timer   *time.Timer // realtime impl, if set
	nxt     time.Time   // next tick time
	mock    *Mock       // mock clock, if set
	fn      func()      // AfterFunc function, if set
	stopped bool        // True if stopped, false if running

	// As of Go 1.23, timer channels are unbuffered and guaranteed to block forever after a call to
	// stop.
	//
	// When a mocked timer fires, we don't want to block on a channel write, because it's fine for
	// the code under test not to be reading. That means we need to start a new goroutine to do the
	// channel write if we are a channel-based timer.
	//
	// They also are not supposed to leak even if they are never read or stopped (Go runtime can
	// garbage collect them). We can't garbage-collect because we can't check if any other code
	// besides the mock references, but we can ensure that we don't leak goroutines so that the
	// garbage collector can do its job when the mock is no longer referenced. interrupt lets Stop,
	// Reset and the cleanup registered by NewMock end the write goroutine.
	interrupt chan struct{}
}

func (t *Timer) fire(tt time.Time) {
	t.mock.mu.Lock()
	t.mock.removeTimerLocked(t)
	if t.fn != nil {
		t.mock.mu.Unlock()
		t.fn()
		return
	} else if t.mock.testOver {
		t.mock.mu.Unlock()
		// NewMock's cleanup has run and would not end a write goroutine, so only a reader that is
		// already waiting gets the value.
		select {
		case t.c <- tt:
		default:
		}
	} else {
		// Reset and NewTimer fire a timer with a non-positive duration from a new goroutine, so the
		// timer can fire again before the write from an earlier fire has ended. writes tracks one
		// write per timer, so end the earlier one, as Reset would.
		t.endWriteLocked()
		interrupt := make(chan struct{})
		t.interrupt = interrupt
		t.mock.writes[t] = struct{}{}
		t.mock.mu.Unlock()
		go func() {
			// select picks at random among ready cases. Send first to a reader that is already
			// waiting, so it gets the value even when Stop, Reset or NewMock's cleanup is waiting
			// on interrupt.
			select {
			case t.c <- tt:
			default:
				select {
				case t.c <- tt:
				case interrupt <- struct{}{}:
					// endWriteLocked received this and removed the write.
					return
				}
			}
			// Close before locking: endWriteLocked waits on interrupt with the lock held.
			close(interrupt)
			t.mock.mu.Lock()
			defer t.mock.mu.Unlock()
			// If interrupt has changed, endWriteLocked has removed this write, and Reset may have
			// started another.
			if t.interrupt == interrupt {
				t.interrupt = nil
				delete(t.mock.writes, t)
			}
		}()
	}
}

// endWriteLocked ends the goroutine that fire started to send on C, if it has not ended yet. It
// reports whether that goroutine ended without sending the value.
func (t *Timer) endWriteLocked() bool {
	if t.interrupt == nil {
		return false
	}
	// The goroutine sends on interrupt when it ends without sending the value, and closes
	// interrupt once it has sent the value.
	_, unsent := <-t.interrupt
	t.interrupt = nil
	delete(t.mock.writes, t)
	return unsent
}

func (t *Timer) next() time.Time {
	return t.nxt
}

// Stop prevents the Timer from firing. It returns true if the call stops the timer, false if the
// timer has already expired or been stopped. As with a time.Timer, a timer whose value has not been
// received has not expired: Stop discards the value and returns true. Stop does not close the
// channel, to prevent a read from the channel succeeding incorrectly.
//
// See https://pkg.go.dev/time#Timer.Stop for more information.
func (t *Timer) Stop(tags ...string) bool {
	if t.timer != nil {
		return t.timer.Stop()
	}
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	c := newCall(clockFunctionTimerStop, tags)
	t.mock.matchCallLocked(c)
	defer close(c.complete)
	result := !t.stopped
	t.mock.removeTimerLocked(t)
	if t.endWriteLocked() {
		result = true
	}
	return result
}

// Reset changes the timer to expire after duration d. It returns true if the timer had been active,
// false if the timer had expired or been stopped. As with a time.Timer, a timer whose value has not
// been received is active: Reset discards the value and returns true.
//
// See https://pkg.go.dev/time#Timer.Reset for more information.
func (t *Timer) Reset(d time.Duration, tags ...string) bool {
	if t.timer != nil {
		return t.timer.Reset(d)
	}
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	c := newCall(clockFunctionTimerReset, tags, withDuration(d))
	t.mock.matchCallLocked(c)
	defer close(c.complete)
	result := !t.stopped
	if t.endWriteLocked() {
		result = true
	}
	// Remove the timer from the mock's event list synchronously; otherwise the
	// stale old schedule can cause e.g. Advance() to reject advancing past the
	// old nxt, even though the timer has been reset.
	t.mock.removeTimerLocked(t)
	if d <= 0 {
		// zero or negative duration timer means we should immediately re-fire
		// it, rather than re-add it.
		t.stopped = false
		go t.fire(t.mock.cur)
		return result
	}
	t.stopped = false
	t.nxt = t.mock.cur.Add(d)
	t.mock.addEventLocked(t)
	return result
}
