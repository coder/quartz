package quartz_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
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
	select {
	case <-waitErr:
		t.Fatal("wait should not return while tick callback in progress")
	case <-time.After(time.Millisecond * 100):
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

// TestCallRelease_ExpiredContext verifies that Release returns the context error when the context
// expires before the call completes, still releases the call so that it returns, and mentions other
// traps only when they kept the call from completing while Release waited. Close must not report
// the call, and a failed MustRelease must name MustRelease and the call.
func TestCallRelease_ExpiredContext(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tt := range []struct {
		name  string
		traps int
		// whileWaiting gives Release a context that expires once Release waits for the call to
		// complete, instead of one that has already expired.
		whileWaiting bool
		// want must be in the error.
		want string
	}{
		{name: "OneTrap", traps: 1, want: "already expired when Release was called; this trap released"},
		{name: "TwoTraps", traps: 2, want: "already expired when Release was called; this trap released"},
		{name: "OneTrapWhileWaiting", traps: 1, whileWaiting: true, want: "complete after release"},
		{name: "TwoTrapsWhileWaiting", traps: 2, whileWaiting: true, want: "2 traps caught it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			tb := &captureFailTB{TB: t}
			mClock := quartz.NewMock(tb)
			// The trapped call is the ticker's Wait, which does not return until the ticker stops,
			// so the call cannot complete while Release waits, even with one trap.
			tickerCtx, stopTicker := context.WithCancel(ctx)
			defer stopTicker()
			tkr := mClock.TickerFunc(tickerCtx, time.Hour, func() error { return nil })
			var tags []string
			var traps []*quartz.Trap
			for i := range tt.traps {
				tags = append(tags, strconv.Itoa(i))
				traps = append(traps, mClock.Trap().TickerFuncWait(tags[i]))
			}
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				_ = tkr.Wait(tags...)
			}()

			c, err := traps[0].Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var releaseCtx context.Context = expired
			if tt.whileWaiting {
				releaseCtx = newExpiresOnWaitCtx(t)
			}
			errs := make(chan error, 1)
			go func() { errs <- c.Release(releaseCtx) }()
			select {
			case err = <-errs:
			case <-ctx.Done():
				t.Fatal("Release() did not return after its context expired")
			}
			if releaseCtx.Err() == nil || !errors.Is(err, releaseCtx.Err()) {
				t.Fatalf("Release() = %v, want %v", err, releaseCtx.Err())
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Release() = %q, want it to contain %q", err, tt.want)
			}
			blamesTraps := tt.whileWaiting && tt.traps > 1
			if strings.Contains(err.Error(), "traps caught it") != blamesTraps {
				t.Fatalf("Release() = %q, want a mention of other traps: %v", err, blamesTraps)
			}

			stopTicker()
			for _, trap := range traps[1:] {
				c, err := trap.Wait(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Release(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-returned:
			case <-ctx.Done():
				t.Fatal("the call did not return after every trap released it")
			}
			for _, trap := range traps {
				trap.Close()
			}
			if tb.failed {
				t.Fatal("Close() reported a failure after every trap released the call")
			}
		})
	}

	t.Run("MustRelease", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tb := &captureFailTB{TB: t}
		mClock := quartz.NewMock(tb)
		trap := mClock.Trap().Now()
		defer trap.Close()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			mClock.Now()
		}()
		c, err := trap.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c.MustRelease(expired)
		select {
		case <-returned:
		case <-ctx.Done():
			t.Fatal("the call did not return after MustRelease")
		}
		wantMessages(t, tb.messages, "MustRelease of Now([]): context already expired")
	})
}

// Test_UnreleasedCalls verifies that Close fails the test only when a trapped call has not been
// released.
func Test_UnreleasedCalls(t *testing.T) {
	t.Parallel()

	t.Run("NotReleased", func(t *testing.T) {
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
	})

	// Release must count the call as released before the call returns, not after it completes: the
	// test may call Close as soon as the call returns.
	t.Run("ReleasedOnAnotherGoroutine", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tb := &captureFailTB{TB: t}
		mClock := quartz.NewMock(tb)
		trap := mClock.Trap().Now()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			mClock.Now()
		}()
		c, err := trap.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		released := make(chan error, 1)
		go func() { released <- c.Release(ctx) }()
		<-returned
		trap.Close()
		if err := <-released; err != nil {
			t.Fatal(err)
		}
		if tb.failed {
			t.Fatal("Close() reported a failure after the released call returned")
		}
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
	// messages holds the failure messages, in order.
	messages []string
	testing.TB
}

func (t *captureFailTB) Errorf(format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	t.fail(fmt.Sprintf(format, args...))
}

func (t *captureFailTB) Error(args ...any) {
	t.Helper()
	t.Log(args...)
	t.fail(fmt.Sprint(args...))
}

func (t *captureFailTB) Fatal(args ...any) {
	t.Helper()
	t.Log(args...)
	t.fail(fmt.Sprint(args...))
}

func (t *captureFailTB) Fatalf(format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	t.fail(fmt.Sprintf(format, args...))
}

func (t *captureFailTB) fail(msg string) {
	t.failed = true
	t.messages = append(t.messages, msg)
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

// expiresOnWaitCtx is a context that is live until its Done channel is first requested, and then
// expires. Release, Trap.Wait and MustWait check ctx.Err() on entry and call Done only to wait, so
// it stands in for a context that expires while they wait, without depending on timing.
type expiresOnWaitCtx struct {
	context.Context
	cancel context.CancelFunc
}

func newExpiresOnWaitCtx(t testing.TB) expiresOnWaitCtx {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return expiresOnWaitCtx{Context: ctx, cancel: cancel}
}

func (c expiresOnWaitCtx) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

// wantMessages fails the test unless got has one message per want, each containing its want.
func wantMessages(t testing.TB, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("failures = %q, want %d containing %q", got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Fatalf("failure %d = %q, want it to contain %q", i, got[i], want[i])
		}
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

func TestTimerStop_Go123(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)

	tmr := mClock.NewTimer(1 * time.Second)
	mClock.Advance(1 * time.Second).MustWait(ctx)
	if tmr.Stop() {
		t.Fatal("timer hadn't already been active")
	}

	select {
	case tme := <-tmr.C:
		t.Fatalf("got channel read after stop: %s", tme)
	default:
		// OK!
	}
}

func TestTimerReset_Go123(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)

	tmr := mClock.NewTimer(1 * time.Second)
	mClock.Advance(1 * time.Second).MustWait(ctx)
	if tmr.Reset(1 * time.Second) {
		t.Fatal("timer hadn't already been active")
	}

	select {
	case tme := <-tmr.C:
		t.Fatalf("got channel read after stop: %s", tme)
	default:
		// OK!
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

// TestAdvanceWaiter_ExpiredContext verifies that Wait returns the context error, without blocking,
// when the context has expired, whether or not the advance has completed. MustWait must say the
// context had already expired exactly when it had expired before MustWait was called, and
// otherwise that it expired while waiting.
func TestAdvanceWaiter_ExpiredContext(t *testing.T) {
	t.Parallel()
	// The deadline passed before Wait, as when the test's timeout ran out first.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)

	t.Run("Completed", func(t *testing.T) {
		t.Parallel()
		tb := &captureFailTB{TB: t}
		mClock := quartz.NewMock(tb)
		// A Wait that picks at random between completion and ctx.Done fails only half the
		// iterations, so repeat.
		for range 100 {
			// With no timers or tickers, the advance completes before Advance returns.
			err := mClock.Advance(time.Second).Wait(expired)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Wait() = %v, want %v", err, context.DeadlineExceeded)
			}
		}

		mClock.Advance(time.Second).MustWait(expired)
		mClock.Advance(time.Second).MustWait(newExpiresOnWaitCtx(t))
		wantMessages(t, tb.messages, "already expired when MustWait was called",
			"while waiting for clock to advance")
	})

	t.Run("Pending", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tb := &captureFailTB{TB: t}
		mClock := quartz.NewMock(tb)
		release := make(chan struct{})
		// The advance cannot complete until the AfterFunc returns.
		mClock.AfterFunc(time.Second, func() { <-release })
		w := mClock.Advance(time.Second)
		errs := make(chan error, 1)
		expiresOnWait := newExpiresOnWaitCtx(t)
		go func() {
			err := w.Wait(expired)
			w.MustWait(expired)
			w.MustWait(expiresOnWait)
			errs <- err
		}()
		select {
		case err := <-errs:
			close(release)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Wait() = %v, want %v", err, context.DeadlineExceeded)
			}
		case <-ctx.Done():
			close(release)
			t.Fatal("Wait() or MustWait() did not return after the context expired")
		}
		wantMessages(t, tb.messages, "already expired when MustWait was called",
			"while waiting for clock to advance")
		if err := w.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestTrapWait_ExpiredContext verifies that Wait returns the context error, without blocking, when
// the context expires, even if the trap is closed.
func TestTrapWait_ExpiredContext(t *testing.T) {
	t.Parallel()

	t.Run("TrapClosed", func(t *testing.T) {
		t.Parallel()
		expired, cancel := context.WithCancel(context.Background())
		cancel()

		mClock := quartz.NewMock(t)
		trap := mClock.Trap().Now()
		trap.Close()
		// A Wait that picks at random between the closed trap and ctx.Done fails only half the
		// iterations, so repeat.
		for range 100 {
			if _, err := trap.Wait(expired); !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait() = %v, want %v", err, context.Canceled)
			}
		}
	})

	t.Run("ExpiresWhileWaiting", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		mClock := quartz.NewMock(t)
		trap := mClock.Trap().Now()
		defer trap.Close()
		// The context is live when Wait checks it on entry and expires once Wait waits.
		waitCtx := newExpiresOnWaitCtx(t)
		errs := make(chan error, 1)
		go func() {
			_, err := trap.Wait(waitCtx)
			errs <- err
		}()
		select {
		case err := <-errs:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait() = %v, want %v", err, context.Canceled)
			}
		case <-ctx.Done():
			t.Fatal("Wait() did not return after the context expired")
		}
	})
}

func TestNoLeak_Go123(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mClock := quartz.NewMock(t)

	_ = mClock.NewTimer(1 * time.Second)
	_ = mClock.NewTicker(1 * time.Second)
	mClock.Advance(1 * time.Second).MustWait(ctx)
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
