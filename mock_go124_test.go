//go:build go1.24

// The weak package was added in Go 1.24.

package quartz_test

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/coder/quartz"
)

// collectSentinel is the cause given to the context under test, so it is reachable only through
// that context. The pointer field keeps the allocator from batching it with other tiny objects,
// which would keep it reachable through a neighbor.
type collectSentinel struct{ _ *int }

func (*collectSentinel) Error() string { return "collect sentinel" }

func TestWithTimeout_DoneContextIsCollectable(t *testing.T) {
	t.Parallel()

	// Each case ends a context whose cause is the returned sentinel. The Mock stays reachable
	// through the cleanup that NewMock registers on t.
	tests := map[string]func(t *testing.T) weak.Pointer[collectSentinel]{
		"Canceled": func(t *testing.T) weak.Pointer[collectSentinel] {
			mClock := quartz.NewMock(t)
			cause := &collectSentinel{}
			_, cancel := mClock.WithTimeoutCause(context.Background(), time.Hour, cause)
			cancel()
			return weak.Make(cause)
		},
		"Expired": func(t *testing.T) weak.Pointer[collectSentinel] {
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock := quartz.NewMock(t)
			cause := &collectSentinel{}
			// The cancel function is dropped: an expired context must not need it to be released.
			_, _ = mClock.WithTimeoutCause(context.Background(), time.Second, cause)
			mClock.Advance(time.Second).MustWait(testCtx)
			return weak.Make(cause)
		},
		"CanceledWithLiveMockParent": func(t *testing.T) weak.Pointer[collectSentinel] {
			mClock := quartz.NewMock(t)
			parent, parentCancel := mClock.WithTimeout(context.Background(), time.Hour)
			t.Cleanup(parentCancel)
			cause := &collectSentinel{}
			_, cancel := mClock.WithTimeoutCause(parent, time.Hour, cause)
			cancel()
			return weak.Make(cause)
		},
		"CanceledWithLiveStdlibParent": func(t *testing.T) weak.Pointer[collectSentinel] {
			mClock := quartz.NewMock(t)
			parent, parentCancel := context.WithCancel(context.Background())
			t.Cleanup(parentCancel)
			cause := &collectSentinel{}
			_, cancel := mClock.WithTimeoutCause(parent, time.Hour, cause)
			cancel()
			return weak.Make(cause)
		},
	}
	for name, endContext := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cause := endContext(t)
			// runtime.GC returns after a full cycle, including sweeping, so an unreachable sentinel
			// is already reclaimed.
			runtime.GC()
			if cause.Value() != nil {
				t.Fatal("expected the done mock context to be unreachable")
			}
		})
	}
}
