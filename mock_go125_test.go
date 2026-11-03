//go:build go1.25

package quartz_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/quartz"
)

// TestTimer_ReaderWaitingAtMockCleanup checks that a reader waiting on C when NewMock's cleanup
// runs receives the value of a timer that fires just before or after that cleanup. synctest.Wait
// makes sure the reader is waiting before the timer fires and that the send has ended before the
// check.
func TestTimer_ReaderWaitingAtMockCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		fireAfterCleanup bool
	}{
		// The test fires the timer as its last step. In a bubble, the goroutine that sends the
		// value often starts only after NewMock's cleanup is waiting to end it.
		{name: "FiredBeforeCleanup"},
		{name: "FiredAfterCleanup", fireAfterCleanup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// When the sending goroutine starts late, select picks at random between the reader
			// and the cleanup, so a send that does not try the reader first loses the value about
			// half the time. Each round is a new bubble, so without the race detector, which
			// changes scheduling, the test then fails in nearly every run.
			for range 10 {
				synctest.Test(t, func(t *testing.T) {
					var mClock *quartz.Mock
					received := make(chan struct{}, 1)
					quit := make(chan struct{})
					// Registered before NewMock, so it runs after NewMock's cleanup.
					t.Cleanup(func() {
						if tc.fireAfterCleanup {
							mClock.Advance(time.Second)
						}
						synctest.Wait()
						close(quit)
						if len(received) == 0 {
							t.Error("the waiting reader did not receive the timer value")
						}
					})
					mClock = quartz.NewMock(t)
					tmr := mClock.NewTimer(time.Second)
					go func() {
						select {
						case <-tmr.C:
							received <- struct{}{}
						case <-quit:
						}
					}()
					synctest.Wait()
					if !tc.fireAfterCleanup {
						mClock.Advance(time.Second).MustWait(context.Background())
					}
				})
			}
		})
	}
}
