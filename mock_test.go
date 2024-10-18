package quartz_test

import (
	"context"
	"errors"
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
	c.Release()
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
	c.Release()
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
	c.Release()
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
	c.Release()
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
	trapStop.MustWait(ctx).Release()
	mClock.Advance(time.Hour).MustWait(ctx)
	select {
	case <-tkr.C:
		t.Fatal("ticker still running")
	default:
		// OK
	}

	// Resetting after stop
	go tkr.Reset(time.Minute, "reset")
	trapReset.MustWait(ctx).Release()
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
