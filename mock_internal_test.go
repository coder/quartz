package quartz

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTrapWait_CallReadyWithExpiredContext verifies that Wait returns the context error when the
// context has expired, even if a call is ready, and leaves the call for the next Wait.
//
// The public API cannot make a call ready before Wait checks the context, because catch delivers
// each call from its own goroutine, so this test builds that state directly.
func TestTrapWait_CallReadyWithExpiredContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()

	// A buffered channel holding the call stands in for a catch goroutine blocked sending it.
	trap := &Trap{
		fn:    clockFunctionNow,
		mock:  NewMock(t),
		calls: make(chan *apiCall, 1),
		done:  make(chan struct{}),
	}
	want := newCall(clockFunctionNow, nil)
	// A Wait that picks at random between the ready call and ctx.Done fails only half the
	// iterations, so repeat.
	for range 100 {
		trap.calls <- want
		if _, err := trap.Wait(expired); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
		}
		c, err := trap.Wait(ctx)
		if err != nil {
			t.Fatalf("second Wait() error = %v, want nil", err)
		}
		if c.apiCall != want {
			t.Fatal("second Wait() returned a different call than the ready one")
		}
	}
}
