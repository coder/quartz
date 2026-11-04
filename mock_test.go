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

// TestTickerFunc_MockContextCanceled tests that TickerFunc exits when its context, created by the
// same Mock, is canceled. If it deadlocks, go test -timeout reports it by naming this test.
func TestTickerFunc_MockContextCanceled(t *testing.T) {
	t.Parallel()
	mClock := quartz.NewMock(t)

	ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour)
	w := mClock.TickerFunc(ctx, time.Second, func() error { return nil })
	cancel()
	if err := w.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got %v", err)
	}
}

// A mock context expires when Advance reaches its deadline. By the time MustWait returns, it and the
// contexts derived from it report DeadlineExceeded and the deadline's cause.
func TestWithTimeout_FiresOnAdvance(t *testing.T) {
	t.Parallel()

	deadlineCause := errors.New("deadline cause")
	tests := map[string]struct {
		newCtx    func(mClock *quartz.Mock) (context.Context, context.CancelFunc)
		wantCause error
	}{
		"WithTimeout": {
			newCtx: func(mClock *quartz.Mock) (context.Context, context.CancelFunc) {
				return mClock.WithTimeout(context.Background(), time.Second)
			},
			wantCause: context.DeadlineExceeded,
		},
		"WithTimeoutCause": {
			newCtx: func(mClock *quartz.Mock) (context.Context, context.CancelFunc) {
				return mClock.WithTimeoutCause(context.Background(), time.Second, deadlineCause)
			},
			wantCause: deadlineCause,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()

			mClock := quartz.NewMock(t)
			ctx, cancel := tt.newCtx(mClock)
			defer cancel()
			ctxs := descendants(t, mClock, ctx)
			ctxs["Self"] = ctx

			mClock.Advance(time.Second).MustWait(testCtx)

			for name, c := range ctxs {
				if !errors.Is(c.Err(), context.DeadlineExceeded) {
					t.Errorf("%s: expected DeadlineExceeded, got %v", name, c.Err())
				}
				if !errors.Is(context.Cause(c), tt.wantCause) {
					t.Errorf("%s: expected cause %v, got %v", name, tt.wantCause, context.Cause(c))
				}
			}
		})
	}
}

func TestWithTimeout_CancelCancelsDerived(t *testing.T) {
	t.Parallel()

	mClock := quartz.NewMock(t)
	ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour)
	derived := descendants(t, mClock, ctx)

	cancel()
	for name, child := range derived {
		if !errors.Is(child.Err(), context.Canceled) {
			t.Errorf("%s: expected Canceled, got %v", name, child.Err())
		}
		if got := context.Cause(child); !errors.Is(got, context.Canceled) {
			t.Errorf("%s: expected Canceled cause, got %v", name, got)
		}
	}
}

type ctxKey struct{}

// embeddingCtx is a user-defined context that embeds its parent, as wrappers in logging and tracing
// libraries do.
type embeddingCtx struct{ context.Context }

// descendants returns contexts derived from ctx directly and through layers that do not cancel,
// each done before the call that cancels ctx returns. The Mock cases derive a mock context and
// return a stdlib context derived from it: reading the mock context itself would check its parent
// and cancel it, hiding a missed notification.
func descendants(t *testing.T, mClock *quartz.Mock, ctx context.Context) map[string]context.Context {
	withCancel := func(parent context.Context) context.Context {
		child, cancel := context.WithCancel(parent)
		t.Cleanup(cancel)
		return child
	}
	throughMock := func(parent context.Context) context.Context {
		nested, cancel := mClock.WithTimeout(parent, time.Hour)
		t.Cleanup(cancel)
		return withCancel(nested)
	}
	return map[string]context.Context{
		"Direct":        withCancel(ctx),
		"WithValue":     withCancel(context.WithValue(ctx, ctxKey{}, "v")),
		"Embedding":     withCancel(embeddingCtx{ctx}),
		"Mock":          throughMock(ctx),
		"MockWithValue": throughMock(context.WithValue(ctx, ctxKey{}, "v")),
		"MockEmbedding": throughMock(embeddingCtx{ctx}),
	}
}

// stallingParent is a parent whose Done never closes and whose AfterFunc stop function blocks until
// release is closed. A mock context calls that stop function while it is being canceled, so a test
// can act while another goroutine is canceling it. Canceling the embedded context makes Err and
// Cause report the parent done without notifying the mock context.
type stallingParent struct {
	context.Context
	stopping chan struct{}
	release  chan struct{}
}

// Done is not nil, so contexts derived from p register through AfterFunc.
func (*stallingParent) Done() <-chan struct{} { return make(chan struct{}) }

func (p *stallingParent) AfterFunc(func()) func() bool {
	return func() bool {
		close(p.stopping)
		<-p.release
		return true
	}
}

// When cancel is called while another goroutine is canceling the context and is blocked in the
// parent, cancel does not wait for the parent, and the context and its derived contexts report that
// goroutine's error and cause.
func TestWithTimeout_CancelWhileCanceling(t *testing.T) {
	t.Parallel()

	deadlineCause := errors.New("deadline cause")
	parentCause := errors.New("parent cause")
	tests := map[string]struct {
		// startCanceling starts canceling the context on another goroutine and returns a function
		// that waits for that goroutine.
		startCanceling func(testCtx context.Context, mClock *quartz.Mock,
			cancel context.CancelFunc, cancelParent context.CancelCauseFunc) (wait func())
		wantErr   error
		wantCause error
	}{
		"Expiry": {
			startCanceling: func(testCtx context.Context, mClock *quartz.Mock,
				_ context.CancelFunc, _ context.CancelCauseFunc,
			) func() {
				w := mClock.Advance(time.Second)
				return func() { w.MustWait(testCtx) }
			},
			wantErr:   context.DeadlineExceeded,
			wantCause: deadlineCause,
		},
		// The first cancel finds the parent done and takes its error and cause.
		"ParentCanceled": {
			startCanceling: func(_ context.Context, _ *quartz.Mock,
				cancel context.CancelFunc, cancelParent context.CancelCauseFunc,
			) func() {
				cancelParent(parentCause)
				done := make(chan struct{})
				go func() {
					cancel()
					close(done)
				}()
				return func() { <-done }
			},
			wantErr:   context.Canceled,
			wantCause: parentCause,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()

			mClock := quartz.NewMock(t)
			root, cancelRoot := context.WithCancelCause(context.Background())
			defer cancelRoot(nil)
			parent := &stallingParent{
				Context:  root,
				stopping: make(chan struct{}),
				release:  make(chan struct{}),
			}
			ctx, cancel := mClock.WithTimeoutCause(parent, time.Second, deadlineCause)
			child, childCancel := context.WithCancel(ctx)
			defer childCancel()

			wait := tt.startCanceling(testCtx, mClock, cancel, cancelRoot)
			select {
			case <-parent.stopping:
			case <-testCtx.Done():
				t.Fatal("timed out waiting for the cancellation to stop the parent registration")
			}
			// If cancel waits for the stalled goroutine, go test -timeout reports it by naming this
			// test.
			cancel()
			// Read while the other goroutine is still in the parent's stop function.
			ctxErr, ctxCause := ctx.Err(), context.Cause(ctx)
			childErr, childCause := child.Err(), context.Cause(child)
			close(parent.release)
			wait()

			if !errors.Is(ctxErr, tt.wantErr) || !errors.Is(ctxCause, tt.wantCause) {
				t.Errorf("expected context %v with cause %v, got %v with cause %v",
					tt.wantErr, tt.wantCause, ctxErr, ctxCause)
			}
			if !errors.Is(childErr, tt.wantErr) || !errors.Is(childCause, tt.wantCause) {
				t.Errorf("expected derived context %v with cause %v, got %v with cause %v",
					tt.wantErr, tt.wantCause, childErr, childCause)
			}
		})
	}
}

func TestWithDeadline_Deadline(t *testing.T) {
	t.Parallel()

	t.Run("NoParentDeadline", func(t *testing.T) {
		t.Parallel()
		mClock := quartz.NewMock(t)
		ctx, cancel := mClock.WithDeadline(context.Background(), mClock.Now().Add(time.Hour))
		defer cancel()

		// Code that requires a deadline still finds one, and it is in the wall-clock future.
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected deadline")
		}
		if got.Year() != 9999 {
			t.Fatalf("expected deadline in year 9999, got %s", got)
		}
	})

	t.Run("ParentDeadline", func(t *testing.T) {
		t.Parallel()
		mClock := quartz.NewMock(t)
		parent, parentCancel := context.WithTimeout(context.Background(), time.Hour)
		defer parentCancel()
		want, _ := parent.Deadline()
		ctx, cancel := mClock.WithDeadline(parent, mClock.Now().Add(time.Minute))
		defer cancel()

		got, ok := ctx.Deadline()
		if !ok || !got.Equal(want) {
			t.Fatalf("expected parent deadline %s, got %s, %v", want, got, ok)
		}
	})
}

func TestWithTimeout_CancelIsIdempotentAndRemovesEvent(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	mClock := quartz.NewMock(t)
	ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour)

	cancel()
	cancel()
	select {
	case <-ctx.Done():
		// OK
	case <-testCtx.Done():
		t.Fatal("timeout waiting for canceled context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("expected Canceled, got %v", ctx.Err())
	}
	if d, ok := mClock.Peek(); ok {
		t.Fatalf("expected no pending events, got Peek()=%s", d)
	}
	mClock.Advance(2 * time.Hour).MustWait(testCtx)
}

func TestWithTimeout_ParentCanceled(t *testing.T) {
	t.Parallel()

	// Each subtest observes the parent's cancellation through one path only, because every path
	// also cancels the mock context and would hide a gap in the others.
	observe := map[string]func(t *testing.T, mClock *quartz.Mock, ctx context.Context){
		"Err": func(t *testing.T, _ *quartz.Mock, ctx context.Context) {
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("expected Canceled, got %v", ctx.Err())
			}
		},
		"Done": func(t *testing.T, _ *quartz.Mock, ctx context.Context) {
			select {
			case <-ctx.Done():
			default:
				t.Fatal("expected Done to be closed")
			}
		},
		"Cause": func(t *testing.T, _ *quartz.Mock, ctx context.Context) {
			if got := context.Cause(ctx); !errors.Is(got, context.Canceled) {
				t.Fatalf("expected Canceled cause, got %v", got)
			}
		},
		"Peek": func(t *testing.T, mClock *quartz.Mock, _ context.Context) {
			if d, ok := mClock.Peek(); ok {
				t.Fatalf("expected no pending events, got Peek()=%s", d)
			}
		},
		"Advance": func(t *testing.T, mClock *quartz.Mock, _ context.Context) {
			// The canceled context must not block advancing past its old deadline.
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.Advance(2 * time.Hour).MustWait(testCtx)
		},
		"Set": func(t *testing.T, mClock *quartz.Mock, _ context.Context) {
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.Set(mClock.Now().Add(2 * time.Hour)).MustWait(testCtx)
		},
		"AdvanceNext": func(t *testing.T, mClock *quartz.Mock, _ context.Context) {
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.NewTimer(2 * time.Hour) // AdvanceNext fails the test when nothing is scheduled.
			d, w := mClock.AdvanceNext()
			w.MustWait(testCtx)
			if d != 2*time.Hour {
				t.Fatalf("expected AdvanceNext to skip the canceled context, advanced %s", d)
			}
		},
	}
	for name, fn := range observe {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mClock := quartz.NewMock(t)
			parent, parentCancel := context.WithCancel(context.Background())
			ctx, cancel := mClock.WithTimeout(parent, time.Hour)
			defer cancel()

			// As with stdlib contexts, the cancellation is visible as soon as parentCancel returns.
			parentCancel()
			fn(t, mClock, ctx)
		})
	}
}

// When a parent that is not a mock context is canceled, a mock context learns of it on another
// goroutine, so a Done channel obtained before, and the Done of a derived context, close a moment
// later.
func TestWithTimeout_ParentCanceledClosesDone(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T, mClock *quartz.Mock) (context.Context, context.CancelFunc){
		"Stdlib": func(*testing.T, *quartz.Mock) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		// The stdlib layer can be canceled without the mock context behind it, so the child must
		// not register with that mock context.
		"WithCancelOfMock": func(t *testing.T, mClock *quartz.Mock) (context.Context, context.CancelFunc) {
			mockParent, cancel := mClock.WithTimeout(context.Background(), time.Hour)
			t.Cleanup(cancel)
			return context.WithCancel(mockParent)
		},
	}
	for name, newParent := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()

			mClock := quartz.NewMock(t)
			parent, parentCancel := newParent(t, mClock)
			ctx, cancel := mClock.WithTimeout(parent, time.Hour)
			defer cancel()
			child, childCancel := context.WithCancel(ctx)
			defer childCancel()
			ctxDone := ctx.Done()

			parentCancel()
			for name, done := range map[string]<-chan struct{}{"ctx": ctxDone, "child": child.Done()} {
				select {
				case <-done:
				case <-testCtx.Done():
					t.Fatalf("%s: Done did not close after the parent was canceled", name)
				}
			}
		})
	}
}

// A context created from a done parent reports the parent's error and cause, as stdlib children do.
func TestWithTimeout_ParentAlreadyDone(t *testing.T) {
	t.Parallel()

	cause := errors.New("parent cause")
	tests := map[string]struct {
		newParent func(t *testing.T, mClock *quartz.Mock) context.Context
		wantErr   error
	}{
		"Stdlib": {
			newParent: func(_ *testing.T, _ *quartz.Mock) context.Context {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(cause)
				return ctx
			},
			wantErr: context.Canceled,
		},
		// If creating a context from a done context of the same Mock deadlocks, go test -timeout
		// reports it by naming this subtest.
		"Mock": {
			newParent: func(t *testing.T, mClock *quartz.Mock) context.Context {
				ctx, cancel := mClock.WithTimeoutCause(context.Background(), 0, cause)
				t.Cleanup(cancel)
				return ctx
			},
			wantErr: context.DeadlineExceeded,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mClock := quartz.NewMock(t)
			ctx, cancel := mClock.WithTimeout(tt.newParent(t, mClock), time.Hour)
			defer cancel()

			if !errors.Is(ctx.Err(), tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, ctx.Err())
			}
			if got := context.Cause(ctx); !errors.Is(got, cause) {
				t.Fatalf("expected parent cause, got %v", got)
			}
		})
	}
}

func TestWithTimeout_ParentDeadlineExceeded(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	mClock := quartz.NewMock(t)
	parent, parentCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer parentCancel()
	ctx, cancel := mClock.WithTimeout(parent, time.Hour)
	defer cancel()

	select {
	case <-ctx.Done():
		// A wall-clock parent deadline must still cancel mock-clock children. This keeps test
		// timeout guards effective when mock time is stalled.
	case <-testCtx.Done():
		t.Fatal("timeout waiting for parent deadline")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
	}
	if d, ok := mClock.Peek(); ok {
		t.Fatalf("expected no pending events, got Peek()=%s", d)
	}
}

func TestWithTimeout_PastDeadline(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			mClock := quartz.NewMock(t)
			ctx, cancel := mClock.WithTimeout(context.Background(), d)
			defer cancel()

			// As with stdlib contexts, the context is done when WithTimeout returns.
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
			}
			if d, ok := mClock.Peek(); ok {
				t.Fatalf("expected no pending events, got Peek()=%s", d)
			}
		})
	}
}

// A context created after the cleanup registered by NewMock has run might never be canceled by a
// cleanup, so it must be done when it is returned.
func TestWithTimeout_CreatedAfterMockCleanup(t *testing.T) {
	t.Parallel()

	// Each case creates a context after that cleanup has run and returns the error and cause the
	// context had when WithTimeout returned. Inner is not parallel, so it and its cleanups finish
	// before t.Run returns.
	tests := map[string]func(t *testing.T) (err, cause error){
		// A goroutine that outlives its test.
		"AfterTest": func(t *testing.T) (err, cause error) {
			var mClock *quartz.Mock
			t.Run("Inner", func(t *testing.T) {
				mClock = quartz.NewMock(t)
			})
			ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			return ctx.Err(), context.Cause(ctx)
		},
		// Cleanups run last added, first called, so this cleanup runs after the one NewMock
		// registered.
		"InCleanupRegisteredBeforeNewMock": func(t *testing.T) (err, cause error) {
			t.Run("Inner", func(t *testing.T) {
				var mClock *quartz.Mock
				t.Cleanup(func() {
					ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour)
					defer cancel()
					err, cause = ctx.Err(), context.Cause(ctx)
				})
				mClock = quartz.NewMock(t)
			})
			return err, cause
		},
	}
	for name, createAfterEnd := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err, cause := createAfterEnd(t)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected Canceled, got %v", err)
			}
			if !errors.Is(cause, quartz.ErrMockClockTestEnded) {
				t.Fatalf("expected cause ErrMockClockTestEnded, got %v", cause)
			}
		})
	}
}

func TestWithTimeout_CancelRace(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	mClock := quartz.NewMock(t)
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	ctx, cancel := mClock.WithTimeout(parent, time.Second)
	defer cancel()
	child, childCancel := context.WithCancel(ctx)
	defer childCancel()
	start := make(chan struct{})
	done := make(chan struct{}, 4)

	go func() {
		<-start
		mClock.Advance(time.Second).MustWait(testCtx)
		done <- struct{}{}
	}()
	go func() {
		<-start
		cancel()
		done <- struct{}{}
	}()
	go func() {
		<-start
		parentCancel()
		done <- struct{}{}
	}()
	go func() {
		<-start
		for {
			select {
			case <-ctx.Done():
				_ = ctx.Err()
				_ = child.Err()
				done <- struct{}{}
				return
			default:
				_ = ctx.Err()
				_ = child.Err()
			}
		}
	}()

	close(start)
	for range 4 {
		select {
		case <-done:
		case <-testCtx.Done():
			t.Fatal("timeout waiting for cancel race")
		}
	}
}

func TestWithTimeout_Trap(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	mClock := quartz.NewMock(t)
	trap := mClock.Trap().WithTimeout("ctx")
	defer trap.Close()
	ctxs := make(chan context.Context, 1)
	cancels := make(chan context.CancelFunc, 1)
	go func() {
		ctx, cancel := mClock.WithTimeout(context.Background(), time.Hour, "ctx")
		ctxs <- ctx
		cancels <- cancel
	}()

	call := trap.MustWait(testCtx)
	if call.Duration != time.Hour {
		t.Fatalf("expected time.Hour, got %v", call.Duration)
	}
	call.MustRelease(testCtx)
	var ctx context.Context
	select {
	case ctx = <-ctxs:
	case <-testCtx.Done():
		t.Fatal("timeout waiting for trapped WithTimeout context")
	}
	var cancel context.CancelFunc
	select {
	case cancel = <-cancels:
	case <-testCtx.Done():
		t.Fatal("timeout waiting for trapped WithTimeout cancel")
	}
	defer cancel()
	mClock.Advance(time.Hour).MustWait(testCtx)
	select {
	case <-ctx.Done():
		// OK
	case <-testCtx.Done():
		t.Fatal("timeout waiting for trapped context")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
	}

	causeErr := errors.New("trap cause")
	trapCause := mClock.Trap().WithTimeoutCause("cause")
	defer trapCause.Close()
	go func() {
		ctx, cancel := mClock.WithTimeoutCause(context.Background(), time.Minute, causeErr, "cause")
		ctxs <- ctx
		cancels <- cancel
	}()

	call = trapCause.MustWait(testCtx)
	if call.Duration != time.Minute {
		t.Fatalf("expected time.Minute, got %v", call.Duration)
	}
	if !errors.Is(call.Cause, causeErr) {
		t.Fatalf("expected cause %v, got %v", causeErr, call.Cause)
	}
	call.MustRelease(testCtx)
	select {
	case ctx = <-ctxs:
	case <-testCtx.Done():
		t.Fatal("timeout waiting for trapped WithTimeoutCause context")
	}
	select {
	case cancel = <-cancels:
	case <-testCtx.Done():
		t.Fatal("timeout waiting for trapped WithTimeoutCause cancel")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-testCtx.Done():
		t.Fatal("timeout waiting for canceled WithTimeoutCause ctx")
	}
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
