package quartz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The public API cannot control when a trapped call becomes ready, or when a released call
// completes: catch delivers each call from its own goroutine, and a released call completes on the
// goroutine of the code under test. These tests build those states directly.

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

// TestCallRelease_ExpiredContext verifies that Release returns nil when the context has expired but
// the call has completed, and otherwise returns the context error. With one trap, the error does
// not suggest that other traps hold the call.
func TestCallRelease_ExpiredContext(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tt := range []struct {
		name      string
		completed bool
	}{
		// Closing complete first stands in for the trapped call returning after Release calls
		// releases.Done and before Release checks the context.
		{name: "Completed", completed: true},
		{name: "NotCompleted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A Release that picks at random between completion and ctx.Done fails only half the
			// iterations, so repeat.
			for range 100 {
				a := newCall(clockFunctionNow, nil)
				a.traps = 1
				a.releases.Add(1)
				if tt.completed {
					close(a.complete)
				}
				c := &Call{tb: t, apiCall: a, trap: &Trap{unreleasedCalls: 1}}

				err := c.Release(expired)
				if tt.completed {
					if err != nil {
						t.Fatalf("Release() = %v, want nil", err)
					}
					continue
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Release() = %v, want %v", err, context.Canceled)
				}
				if strings.Contains(err.Error(), "trap") {
					t.Fatalf("Release() = %q, want no mention of other traps", err)
				}
			}
		})
	}
}
