package quartz

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The public API cannot make a trapped call ready, or complete, before Trap.Wait or Call.Release
// observes the context: catch delivers each call from its own goroutine, and a call completes only
// after its last release. These tests build that state directly.

// TestTrapWait_CallReadyWithExpiredContext verifies that Wait returns the context error when the
// context has expired, even if a call is ready, and leaves the call for the next Wait.
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

func TestCallRelease_CompletedWithExpiredContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := newCall(clockFunctionNow, nil)
	a.releases.Add(1)
	// Closing complete first stands in for the trapped call returning after Release calls
	// releases.Done and before Release checks the context.
	close(a.complete)
	c := &Call{tb: t, apiCall: a, trap: &Trap{unreleasedCalls: 1}}

	if err := c.Release(ctx); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
}
