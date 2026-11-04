package quartz_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/quartz"
)

func TestTimer_NegativeDuration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)
	start := mClock.Now()
	trap := mClock.Trap().NewTimer()
	defer trap.Close()

	timers := make(chan *quartz.Timer, 1)
	go func() {
		timers <- mClock.NewTimer(-time.Second)
	}()
	c := trap.MustWait(ctx)
	c.MustRelease(ctx)
	// trap returns the actual passed value
	if c.Duration != -time.Second {
		t.Fatalf("expected -time.Second, got: %v", c.Duration)
	}

	tmr := <-timers
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for timer")
	case tme := <-tmr.C:
		// the tick is the current time, not the past
		if !tme.Equal(start) {
			t.Fatalf("expected time %v, got %v", start, tme)
		}
	}
	if tmr.Stop() {
		t.Fatal("timer still running")
	}
}

func TestAfterFunc_NegativeDuration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)
	trap := mClock.Trap().AfterFunc()
	defer trap.Close()

	timers := make(chan *quartz.Timer, 1)
	done := make(chan struct{})
	go func() {
		timers <- mClock.AfterFunc(-time.Second, func() {
			close(done)
		})
	}()
	c := trap.MustWait(ctx)
	c.MustRelease(ctx)
	// trap returns the actual passed value
	if c.Duration != -time.Second {
		t.Fatalf("expected -time.Second, got: %v", c.Duration)
	}

	tmr := <-timers
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for timer")
	case <-done:
		// OK!
	}
	if tmr.Stop() {
		t.Fatal("timer still running")
	}
}

func TestNewTicker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)
	start := mClock.Now()
	trapNT := mClock.Trap().NewTicker("new")
	defer trapNT.Close()
	trapStop := mClock.Trap().TickerStop("stop")
	defer trapStop.Close()
	trapReset := mClock.Trap().TickerReset("reset")
	defer trapReset.Close()

	tickers := make(chan *quartz.Ticker, 1)
	go func() {
		tickers <- mClock.NewTicker(time.Hour, "new")
	}()
	c := trapNT.MustWait(ctx)
	c.MustRelease(ctx)
	if c.Duration != time.Hour {
		t.Fatalf("expected time.Hour, got: %v", c.Duration)
	}
	tkr := <-tickers

	for i := 0; i < 3; i++ {
		mClock.Advance(time.Hour).MustWait(ctx)
	}

	// should get first tick, rest dropped
	tTime := start.Add(time.Hour)
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for ticker")
	case tick := <-tkr.C:
		if !tick.Equal(tTime) {
			t.Fatalf("expected time %v, got %v", tTime, tick)
		}
	}

	go tkr.Reset(time.Minute, "reset")
	c = trapReset.MustWait(ctx)
	mClock.Advance(time.Second).MustWait(ctx)
	c.MustRelease(ctx)
	if c.Duration != time.Minute {
		t.Fatalf("expected time.Minute, got: %v", c.Duration)
	}
	mClock.Advance(time.Minute).MustWait(ctx)

	// tick should show present time, ensuring the 2 hour ticks got dropped when
	// we didn't read from the channel.
	tTime = mClock.Now()
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for ticker")
	case tick := <-tkr.C:
		if !tick.Equal(tTime) {
			t.Fatalf("expected time %v, got %v", tTime, tick)
		}
	}

	go tkr.Stop("stop")
	trapStop.MustWait(ctx).MustRelease(ctx)
	mClock.Advance(time.Hour).MustWait(ctx)
	select {
	case <-tkr.C:
		t.Fatal("ticker still running")
	default:
		// OK
	}

	// Resetting after stop
	go tkr.Reset(time.Minute, "reset")
	trapReset.MustWait(ctx).MustRelease(ctx)
	mClock.Advance(time.Minute).MustWait(ctx)
	tTime = mClock.Now()
	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for ticker")
	case tick := <-tkr.C:
		if !tick.Equal(tTime) {
			t.Fatalf("expected time %v, got %v", tTime, tick)
		}
	}
}

func TestPeek(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)
	d, ok := mClock.Peek()
	if d != 0 {
		t.Fatal("expected Peek() to return 0")
	}
	if ok {
		t.Fatal("expected Peek() to return false")
	}

	tmr := mClock.NewTimer(time.Second)
	d, ok = mClock.Peek()
	if d != time.Second {
		t.Fatal("expected Peek() to return 1s")
	}
	if !ok {
		t.Fatal("expected Peek() to return true")
	}

	mClock.Advance(999 * time.Millisecond).MustWait(ctx)
	d, ok = mClock.Peek()
	if d != time.Millisecond {
		t.Fatal("expected Peek() to return 1ms")
	}
	if !ok {
		t.Fatal("expected Peek() to return true")
	}

	stopped := tmr.Stop()
	if !stopped {
		t.Fatal("expected Stop() to return true")
	}

	d, ok = mClock.Peek()
	if d != 0 {
		t.Fatal("expected Peek() to return 0")
	}
	if ok {
		t.Fatal("expected Peek() to return false")
	}
}

// TestTickerFunc_ContextDoneDuringTick tests that TickerFunc.Wait() can't return while the tick
// function callback is in progress.
func TestTickerFunc_ContextDoneDuringTick(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer testCancel()
		mClock := quartz.NewMock(t)

		tickStart := make(chan struct{})
		tickDone := make(chan struct{})
		ctx, cancel := context.WithCancel(testCtx)
		defer cancel()
		tkr := mClock.TickerFunc(ctx, time.Second, func() error {
			close(tickStart)
			select {
			case <-tickDone:
			case <-testCtx.Done():
				t.Error("timeout waiting for tickDone")
			}
			return nil
		})
		w := mClock.Advance(time.Second)
		select {
		case <-tickStart:
			// OK
		case <-testCtx.Done():
			t.Fatal("timeout waiting for tickStart")
		}
		waitErr := make(chan error, 1)
		go func() {
			waitErr <- tkr.Wait()
		}()
		cancel()
		// Once every goroutine in the bubble is blocked, Wait has either returned or is waiting
		// for the callback.
		synctest.Wait()
		select {
		case <-waitErr:
			t.Fatal("wait should not return while tick callback in progress")
		default:
			// OK
		}
		close(tickDone)
		select {
		case err := <-waitErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("expected context.Canceled error")
			}
		case <-testCtx.Done():
			t.Fatal("timed out waiting for wait to finish")
		}
		w.MustWait(testCtx)
	})
}

// TestTickerFunc_LongCallback tests that we don't call the ticker func a second time while the
// first is still executing.
func TestTickerFunc_LongCallback(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	mClock := quartz.NewMock(t)

	expectedErr := errors.New("callback error")
	tickStart := make(chan struct{})
	tickDone := make(chan struct{})
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	tkr := mClock.TickerFunc(ctx, time.Second, func() error {
		close(tickStart)
		select {
		case <-tickDone:
		case <-testCtx.Done():
			t.Error("timeout waiting for tickDone")
		}
		return expectedErr
	})
	w := mClock.Advance(time.Second)
	select {
	case <-tickStart:
		// OK
	case <-testCtx.Done():
		t.Fatal("timeout waiting for tickStart")
	}
	// additional ticks complete immediately.
	elapsed := time.Duration(0)
	for elapsed < 5*time.Second {
		d, wt := mClock.AdvanceNext()
		elapsed += d
		wt.MustWait(testCtx)
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- tkr.Wait()
	}()
	cancel()
	close(tickDone)

	select {
	case err := <-waitErr:
		// we should get the function error, not the context error, since context was canceled while
		// we were calling the function, and it returned an error.
		if !errors.Is(err, expectedErr) {
			t.Fatalf("wrong error: %s", err)
		}
	case <-testCtx.Done():
		t.Fatal("timed out waiting for wait to finish")
	}
	w.MustWait(testCtx)
}

func Test_MultipleTraps(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	mClock := quartz.NewMock(t)

	trap0 := mClock.Trap().Now("0")
	defer trap0.Close()
	trap1 := mClock.Trap().Now("1")
	defer trap1.Close()

	timeCh := make(chan time.Time)
	go func() {
		timeCh <- mClock.Now("0", "1")
	}()

	c0 := trap0.MustWait(testCtx)
	mClock.Advance(time.Second)
	// the two trapped call instances need to be released on separate goroutines since they each wait for the Now() call
	// to return, which is blocked on both releases happening. If you release them on the same goroutine, in either
	// order, it will deadlock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c0.MustRelease(testCtx)
	}()
	c1 := trap1.MustWait(testCtx)
	mClock.Advance(time.Second)
	c1.MustRelease(testCtx)

	select {
	case <-done:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for c0.Release()")
	}

	select {
	case got := <-timeCh:
		end := mClock.Now("end")
		if !got.Equal(end) {
			t.Fatalf("expected %s got %s", end, got)
		}
	case <-testCtx.Done():
		t.Fatal("timed out waiting for Now()")
	}
}

func Test_MultipleTrapsDeadlock(t *testing.T) {
	t.Parallel()
	tRunFail(t, func(t testing.TB) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer testCancel()
		mClock := quartz.NewMock(t)

		trap0 := mClock.Trap().Now("0")
		defer trap0.Close()
		trap1 := mClock.Trap().Now("1")
		defer trap1.Close()

		go func() {
			mClock.Now("0", "1")
		}()

		c0 := trap0.MustWait(testCtx)
		c0.MustRelease(testCtx) // deadlocks, test failure
	})
}

func Test_UnreleasedCalls(t *testing.T) {
	t.Parallel()
	tRunFail(t, func(t testing.TB) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer testCancel()
		mClock := quartz.NewMock(t)

		trap := mClock.Trap().Now()
		defer trap.Close()

		go func() {
			_ = mClock.Now()
		}()

		c := trap.MustWait(testCtx) // missing release
		trap.Close()                // detect unreleased call and fail

		c.Release(testCtx) // clean up goroutine
	})
}

func Test_WithLogger(t *testing.T) {
	t.Parallel()

	tl := &testLogger{}
	mClock := quartz.NewMock(t).WithLogger(tl)
	mClock.Now("test", "Test_WithLogger")
	if len(tl.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(tl.calls))
	}
	expectLogLine := "Mock Clock - Now([test Test_WithLogger]) call, matched 0 traps"
	if tl.calls[0] != expectLogLine {
		t.Fatalf("expected log line %q, got %q", expectLogLine, tl.calls[0])
	}

	mClock.NewTimer(time.Second, "timer")
	if len(tl.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(tl.calls))
	}
	expectLogLine = "Mock Clock - NewTimer(1s, [timer]) call, matched 0 traps"
	if tl.calls[1] != expectLogLine {
		t.Fatalf("expected log line %q, got %q", expectLogLine, tl.calls[1])
	}

	mClock.Advance(500 * time.Millisecond)
	if len(tl.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(tl.calls))
	}
	expectLogLine = "Mock Clock - Advance(500ms)"
	if tl.calls[2] != expectLogLine {
		t.Fatalf("expected log line %q, got %q", expectLogLine, tl.calls[2])
	}
}

type captureFailTB struct {
	failed bool
	testing.TB
}

func (t *captureFailTB) Errorf(format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	t.failed = true
}

func (t *captureFailTB) Error(args ...any) {
	t.Helper()
	t.Log(args...)
	t.failed = true
}

func (t *captureFailTB) Fatal(args ...any) {
	t.Helper()
	t.Log(args...)
	t.failed = true
}

func (t *captureFailTB) Fatalf(format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	t.failed = true
}

func (t *captureFailTB) Fail() {
	t.failed = true
}

func (t *captureFailTB) FailNow() {
	t.failed = true
}

func (t *captureFailTB) Failed() bool {
	return t.failed
}

func tRunFail(t testing.TB, f func(t testing.TB)) {
	tb := &captureFailTB{TB: t}
	f(tb)
	if !tb.Failed() {
		t.Fatal("want test to fail")
	}
}

type testLogger struct {
	calls []string
}

func (l *testLogger) Log(args ...any) {
	l.calls = append(l.calls, fmt.Sprint(args...))
}

func (l *testLogger) Logf(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

// TestTimer_StopAndReset checks what Stop and Reset return for a timer that has fired, and that
// no value is received after they return and Reset's schedule stays, as with a time.Timer.
func TestTimer_StopAndReset(t *testing.T) {
	t.Parallel()
	// Each start function returns the timer and the channel its value arrives on: C, or for an
	// AfterFunc timer a channel that f sends on.
	fired := func(ctx context.Context, m *quartz.Mock) (*quartz.Timer, <-chan time.Time) {
		tmr := m.NewTimer(time.Second)
		m.Advance(time.Second).MustWait(ctx)
		return tmr, tmr.C
	}
	received := func(ctx context.Context, m *quartz.Mock) (*quartz.Timer, <-chan time.Time) {
		tmr, c := fired(ctx, m)
		<-c
		return tmr, c
	}
	// NewTimer(0) fires from a new goroutine, which can run before or after the call.
	zeroDuration := func(_ context.Context, m *quartz.Mock) (*quartz.Timer, <-chan time.Time) {
		tmr := m.NewTimer(0)
		return tmr, tmr.C
	}
	// Advance fires the timer from a new goroutine, which can run before or after the call.
	advancing := func(_ context.Context, m *quartz.Mock) (*quartz.Timer, <-chan time.Time) {
		tmr := m.NewTimer(time.Second)
		_ = m.Advance(time.Second)
		return tmr, tmr.C
	}
	// AfterFunc(0) runs f from a new goroutine. Stop usually gets the Mock's lock before it: the
	// goroutine got it first in 19 of 32000 runs with -race and in none without.
	zeroAfterFunc := func(_ context.Context, m *quartz.Mock) (*quartz.Timer, <-chan time.Time) {
		ran := make(chan time.Time, 1)
		return m.AfterFunc(0, func() { ran <- time.Time{} }), ran
	}
	stop := func(tmr *quartz.Timer, _ <-chan time.Time) bool { return tmr.Stop() }
	reset := func(tmr *quartz.Timer, _ <-chan time.Time) bool { return tmr.Reset(time.Hour) }
	// stopAndDrain is the way to stop a timer before Go 1.23. It returns false if the receive
	// blocks, which bubble time reports at once.
	stopAndDrain := func(tmr *quartz.Timer, c <-chan time.Time) bool {
		if !tmr.Stop() {
			select {
			case <-c:
			case <-time.After(time.Second):
				return false
			}
		}
		return true
	}
	// stopAfterFunc reports whether Stop's result agrees with whether f ran, which is what a
	// time.AfterFunc timer promises: true if f never runs, false if f has started. Expecting true
	// would fail in the runs where f's goroutine gets the lock first, and no Mock call can hold
	// that goroutine back.
	stopAfterFunc := func(tmr *quartz.Timer, ran <-chan time.Time) bool {
		stopped := tmr.Stop()
		synctest.Wait()
		select {
		case <-ran:
			return !stopped
		default:
			return stopped
		}
	}
	for _, tc := range []struct {
		name  string
		start func(context.Context, *quartz.Mock) (*quartz.Timer, <-chan time.Time)
		call  func(*quartz.Timer, <-chan time.Time) bool
		want  bool
		// wantNext is how long after the call the timer fires, or 0 if it does not.
		wantNext time.Duration
	}{
		{name: "StopFiredUnread", start: fired, call: stop, want: true},
		{name: "StopFiredReceived", start: received, call: stop, want: false},
		{name: "ResetFiredUnread", start: fired, call: reset, want: true, wantNext: time.Hour},
		{name: "StopAndDrainFiredUnread", start: fired, call: stopAndDrain, want: true},
		{name: "StopZeroDuration", start: zeroDuration, call: stop, want: true},
		{
			name: "ResetZeroDuration", start: zeroDuration, call: reset,
			want: true, wantNext: time.Hour,
		},
		{name: "StopDuringAdvance", start: advancing, call: stop, want: true},
		{
			name: "ResetDuringAdvance", start: advancing, call: reset,
			want: true, wantNext: time.Hour,
		},
		{
			name: "StopAfterFuncZeroDuration", start: zeroAfterFunc, call: stopAfterFunc,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				mClock := quartz.NewMock(t)
				tmr, c := tc.start(ctx, mClock)
				if got := tc.call(tmr, c); got != tc.want {
					t.Errorf("got %v, want %v", got, tc.want)
				}
				// Lets any goroutine that could still send on c run before the checks.
				synctest.Wait()
				select {
				case tme := <-c:
					t.Errorf("received %s after the call returned", tme)
				default:
				}
				if next, _ := mClock.Peek(); next != tc.wantNext {
					t.Errorf("timer fires in %s, want %s", next, tc.wantNext)
				}
			})
		})
	}
}

// TestTimerReset_NegativeDuration verifies that resetting a still-active timer
// to a non-positive duration clears its old schedule from the mock
// synchronously. Otherwise, subsequent Advance/Peek calls race with the
// fire goroutine and may observe the stale schedule (e.g. Advance beyond the
// old nxt is incorrectly rejected).
func TestTimerReset_NegativeDuration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)
	trapReset := mClock.Trap().TimerReset()
	defer trapReset.Close()

	tmr := mClock.NewTimer(time.Hour)

	results := make(chan bool, 1)
	go func() {
		results <- tmr.Reset(-time.Second)
	}()
	trapReset.MustWait(ctx).MustRelease(ctx)
	if !<-results {
		t.Fatal("Reset should report the timer was active")
	}

	// The timer's old 1h schedule must not remain in the mock's event list.
	// If it does, Advance rejects with "cannot advance beyond next
	// timer/ticker event".
	if d, ok := mClock.Peek(); ok {
		t.Fatalf("expected no pending events, got Peek()=%s", d)
	}
	mClock.Advance(2 * time.Hour).MustWait(ctx)

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for timer")
	case <-tmr.C:
		// OK
	}
}

// TestTimer_ReadableDuringCleanup checks that a cleanup registered after NewMock, which runs before
// NewMock's cleanup, can still receive from a timer that fired during the test. Tests that stop a
// server in such a cleanup rely on this when a worker reads the timer on its way out.
func TestTimer_ReadableDuringCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		mClock := quartz.NewMock(t)
		busy := make(chan struct{})
		quit := make(chan struct{})
		exited := make(chan struct{})
		// Stands in for a server's Close: it unblocks the worker and waits for it to exit. Bubble
		// time moves once every goroutine is blocked, so a worker that never receives fails the
		// test at once. quit then lets the worker exit, so the bubble can end.
		t.Cleanup(func() {
			close(busy)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Error("worker did not receive the timer value during cleanup")
			}
			close(quit)
		})
		tmr := mClock.NewTimer(time.Second)
		go func() {
			defer close(exited)
			<-busy
			select {
			case <-tmr.C:
			case <-quit:
			}
		}()
		mClock.Advance(time.Second).MustWait(ctx)
	})
}

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

// TestNoLeak_Go123 relies on verifyNoLeakTestMain to report goroutines that outlive the tests.
func TestNoLeak_Go123(t *testing.T) {
	t.Parallel()

	t.Run("FiredDuringTest", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		mClock := quartz.NewMock(t)

		_ = mClock.NewTimer(1 * time.Second)
		_ = mClock.NewTicker(1 * time.Second)
		mClock.Advance(1 * time.Second).MustWait(ctx)
	})

	// NewTimer(0) and Reset(0) each fire the timer from a new goroutine, and in most of 100 rounds
	// both run after Reset. Only one of them may start a write: the Mock tracks one write per
	// timer, so its cleanup would not end a second one.
	t.Run("TimerFiredTwice", func(t *testing.T) {
		t.Parallel()
		for range 100 {
			tmr := quartz.NewMock(t).WithLogger(quartz.NoOpLogger).NewTimer(0)
			tmr.Reset(0)
		}
	})

	// A test that stops waiting on an Advance can end before its timers fire.
	t.Run("TimerFiredAfterTestEnds", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var mClock *quartz.Mock
		t.Run("Inner", func(t *testing.T) {
			mClock = quartz.NewMock(t)
			_ = mClock.NewTimer(time.Second)
		})
		if err := mClock.Advance(time.Second).Wait(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTickerStop_Go123(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)

	tkr := mClock.NewTicker(1 * time.Second)
	mClock.Advance(1 * time.Second).MustWait(ctx)
	tkr.Stop()

	select {
	case tme := <-tkr.C:
		t.Fatalf("got channel read after stop: %s", tme)
	default:
		// OK!
	}
}

func TestMain(m *testing.M) {
	verifyNoLeakTestMain(m)
}

func verifyNoLeakTestMain(m *testing.M) {
	before := snapshot()
	code := m.Run()
	now := time.Now()
	for {
		after := snapshot()
		if len(after) > len(before) {
			// Allow test cleanup to settle.
			if time.Since(now) < 200*time.Millisecond {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			fmt.Fprintln(os.Stderr, "Possible goroutine leak(s):")
			fmt.Fprintln(os.Stderr, diff(before, after))
			os.Exit(1)
		}
		os.Exit(code)
	}
}

func snapshot() []string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var clean []string
	for _, s := range strings.Split(buf.String(), "\n\n") {
		if !strings.Contains(s, "runtime/pprof") {
			clean = append(clean, s)
		}
	}
	return clean
}

func diff(a, b []string) string {
	m := make(map[string]int)
	for _, s := range a {
		m[s]++
	}
	var leaks []string
	for _, s := range b {
		if m[s] > 0 {
			m[s]--
			continue
		}
		leaks = append(leaks, s)
	}
	return strings.Join(leaks, "\n\n")
}
