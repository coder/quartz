//go:build go1.25

package quartz_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/quartz"
)

// TestTimer_ReaderWaitingAtMockCleanup checks that a reader waiting on C when NewMock's cleanup
// runs still receives a timer value written after that cleanup, as happens when the write's
// goroutine is scheduled late. The timers fire from a later cleanup to get that order on every
// run. synctest.Wait makes sure the readers are waiting before NewMock's cleanup runs and that
// every write has ended before the check.
func TestTimer_ReaderWaitingAtMockCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// select picks at random among ready cases, so a write that does not try the waiting reader
		// first still reaches it half the time. With 10 timers, the test then fails in all but 1 of
		// 1024 runs.
		const n = 10
		var mClock *quartz.Mock
		received := make(chan struct{}, n)
		quit := make(chan struct{})
		// Registered before NewMock, so it runs after NewMock's cleanup.
		t.Cleanup(func() {
			mClock.Advance(time.Second)
			synctest.Wait()
			close(quit)
			if got := len(received); got != n {
				t.Errorf("%d of %d waiting readers received the timer value", got, n)
			}
		})
		mClock = quartz.NewMock(t)
		for range n {
			tmr := mClock.NewTimer(time.Second)
			go func() {
				select {
				case <-tmr.C:
					received <- struct{}{}
				case <-quit:
				}
			}()
		}
		synctest.Wait()
	})
}
