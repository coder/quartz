package quartz_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
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

// A mock context expires when Advance or AdvanceNext reaches its deadline. By the time MustWait
// returns, it and the contexts derived from it report DeadlineExceeded and the deadline's cause.
func TestWithTimeout_FiresOnAdvance(t *testing.T) {
	t.Parallel()

	deadlineCause := errors.New("deadline cause")
	withTimeout := func(mClock *quartz.Mock) (context.Context, context.CancelFunc) {
		return mClock.WithTimeout(context.Background(), time.Second)
	}
	advance := func(_ *testing.T, mClock *quartz.Mock) quartz.AdvanceWaiter {
		return mClock.Advance(time.Second)
	}
	tests := map[string]struct {
		newCtx    func(mClock *quartz.Mock) (context.Context, context.CancelFunc)
		move      func(t *testing.T, mClock *quartz.Mock) quartz.AdvanceWaiter
		wantCause error
	}{
		"WithTimeout": {
			newCtx:    withTimeout,
			move:      advance,
			wantCause: context.DeadlineExceeded,
		},
		"WithTimeoutCause": {
			newCtx: func(mClock *quartz.Mock) (context.Context, context.CancelFunc) {
				return mClock.WithTimeoutCause(context.Background(), time.Second, deadlineCause)
			},
			move:      advance,
			wantCause: deadlineCause,
		},
		"AdvanceNext": {
			newCtx: withTimeout,
			move: func(t *testing.T, mClock *quartz.Mock) quartz.AdvanceWaiter {
				d, w := mClock.AdvanceNext()
				if d != time.Second {
					t.Errorf("expected AdvanceNext to advance 1s, got %s", d)
				}
				return w
			},
			wantCause: context.DeadlineExceeded,
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

			tt.move(t, mClock).MustWait(testCtx)

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

// Canceling a chain of mock contexts reads each context's parent a bounded number of times. If the
// reads grow exponentially with the chain's depth, cancel hangs, and go test -timeout reports it by
// naming this test.
func TestWithTimeout_CancelDeepMockChain(t *testing.T) {
	t.Parallel()

	mClock := quartz.NewMock(t)
	root, cancel := mClock.WithTimeout(context.Background(), time.Hour)
	ctx := root
	for range 30 {
		nested, nestedCancel := mClock.WithTimeout(ctx, time.Hour)
		t.Cleanup(nestedCancel)
		ctx = nested
	}

	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("expected the deepest context Canceled, got %v", ctx.Err())
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
func descendants(
	t *testing.T, mClock *quartz.Mock, ctx context.Context,
) map[string]context.Context {
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
		"Direct":               withCancel(ctx),
		"WithValue":            withCancel(context.WithValue(ctx, ctxKey{}, "v")),
		"Embedding":            withCancel(embeddingCtx{ctx}),
		"Mock":                 throughMock(ctx),
		"MockWithValue":        throughMock(context.WithValue(ctx, ctxKey{}, "v")),
		"MockEmbedding":        throughMock(embeddingCtx{ctx}),
		"MockBehindWithCancel": throughMock(withCancel(ctx)),
	}
}

// stallingParent is a parent whose Done never closes and whose AfterFunc stop function blocks until
// release is closed. A mock context calls that stop function while it is being canceled, so a test
// can act while another goroutine is canceling it. Canceling the embedded context makes Err and
// Cause report the parent done, but p never calls the functions passed to its AfterFunc.
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
// parent of the context or of a mock context derived from it, cancel does not wait for that parent,
// and the context and its derived contexts report that goroutine's error and cause.
func TestWithTimeout_CancelWhileCanceling(t *testing.T) {
	t.Parallel()

	deadlineCause := errors.New("deadline cause")
	parentCause := errors.New("parent cause")
	expire := func(testCtx context.Context, mClock *quartz.Mock,
		_ context.CancelFunc, _ context.CancelCauseFunc,
	) func() {
		w := mClock.Advance(time.Second)
		return func() { w.MustWait(testCtx) }
	}
	tests := map[string]struct {
		// startCanceling starts canceling the context on another goroutine and returns a function
		// that waits for that goroutine.
		startCanceling func(testCtx context.Context, mClock *quartz.Mock,
			cancel context.CancelFunc, cancelParent context.CancelCauseFunc) (wait func())
		// stallDescendant puts the stalling parent between the context and a mock context derived
		// from it, so the cancellation blocks while it unregisters that mock context instead.
		stallDescendant bool
		wantErr         error
		wantCause       error
	}{
		"Expiry": {
			startCanceling: expire,
			wantErr:        context.DeadlineExceeded,
			wantCause:      deadlineCause,
		},
		"ExpiryUnregisteringDescendant": {
			startCanceling:  expire,
			stallDescendant: true,
			wantErr:         context.DeadlineExceeded,
			wantCause:       deadlineCause,
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
				stopping: make(chan struct{}),
				release:  make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(parent.release) })
			// Without the release, a failure before it would leave the test's cleanup blocked.
			defer release()
			var ctx, derivedParent context.Context
			var cancel context.CancelFunc
			if tt.stallDescendant {
				ctx, cancel = mClock.WithTimeoutCause(root, time.Second, deadlineCause)
				parent.Context = ctx
				nested, nestedCancel := mClock.WithTimeout(parent, time.Hour)
				t.Cleanup(nestedCancel)
				derivedParent = nested
			} else {
				parent.Context = root
				ctx, cancel = mClock.WithTimeoutCause(parent, time.Second, deadlineCause)
				derivedParent = ctx
			}
			child, childCancel := context.WithCancel(derivedParent)
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
			release()
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

// When cancel or the expiry's MustWait returns, the mock contexts derived from the context and
// their descendants are done, also those that another goroutine is still canceling.
func TestWithTimeout_WaitsForMockContextsBeingCanceled(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// layer returns the parent of one chain of mock contexts derived from ctx.
		layer func(t *testing.T, ctx context.Context) context.Context
		// expire advances the clock to ctx's deadline and returns the expiry's waiter once the call
		// under test has returned.
		expire func(t *testing.T, testCtx context.Context, mClock *quartz.Mock,
			ctx context.Context, cancel context.CancelFunc) quartz.AdvanceWaiter
	}{
		// ctx.Done closes before the expiry cancels the mock contexts, and with many of them the
		// expiry is still canceling when cancel is called.
		"Cancel": {
			layer: func(_ *testing.T, ctx context.Context) context.Context { return ctx },
			expire: func(t *testing.T, testCtx context.Context, mClock *quartz.Mock,
				ctx context.Context, cancel context.CancelFunc,
			) quartz.AdvanceWaiter {
				w := mClock.Advance(time.Second)
				select {
				case <-ctx.Done():
				case <-testCtx.Done():
					t.Fatal("timed out waiting for the expiry")
				}
				cancel()
				return w
			},
		},
		// The expiry cancels each layer, which starts a stdlib goroutine that cancels the chain
		// derived from the layer, so the expiry finds some chains already being canceled.
		"MustWait": {
			layer: func(t *testing.T, ctx context.Context) context.Context {
				layer, cancel := context.WithCancel(ctx)
				t.Cleanup(cancel)
				return layer
			},
			expire: func(_ *testing.T, testCtx context.Context, mClock *quartz.Mock,
				_ context.Context, _ context.CancelFunc,
			) quartz.AdvanceWaiter {
				w := mClock.Advance(time.Second)
				w.MustWait(testCtx)
				return w
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()

			mClock := quartz.NewMock(t)
			deadlineCause := errors.New("deadline cause")
			ctx, cancel := mClock.WithTimeoutCause(context.Background(), time.Second, deadlineCause)
			defer cancel()
			// The chains are long enough that the call under test usually returns while one is
			// still being canceled if it does not wait for it.
			var derived []context.Context
			for range 20 {
				parent := tt.layer(t, ctx)
				for range 20 {
					nested, nestedCancel := mClock.WithTimeout(parent, time.Hour)
					t.Cleanup(nestedCancel)
					parent = nested
				}
				child, childCancel := context.WithCancel(parent)
				t.Cleanup(childCancel)
				derived = append(derived, child)
			}

			w := tt.expire(t, testCtx, mClock, ctx, cancel)
			var live int
			for _, c := range derived {
				err, cause := c.Err(), context.Cause(c)
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(cause, deadlineCause) {
					live++
				}
			}
			w.MustWait(testCtx)
			if live > 0 {
				t.Fatalf("%d of %d derived contexts lacked DeadlineExceeded or the deadline cause",
					live, len(derived))
			}
		})
	}
}

// blockingErrCtx is a layer whose Err blocks after a token is sent on arm: the next call closes
// entered and returns only after release is closed, so a test can hold a cancellation in progress
// where it reads the layer.
type blockingErrCtx struct {
	context.Context
	arm     chan struct{}
	entered chan struct{}
	release chan struct{}
}

func (p *blockingErrCtx) Err() error {
	select {
	case <-p.arm:
		close(p.entered)
		<-p.release
	default:
	}
	return p.Context.Err()
}

// A mock context behind context.WithoutCancel is not part of its ancestor's cancellation, so a
// stalled cancellation of that detached subtree does not delay canceling the ancestor. The layer's
// Err blocks, which mock contexts do not support, only to hold the detached cancellation in
// progress: canceling the ancestor never reads that layer.
func TestWithTimeout_CancelDoesNotWaitForDetachedContexts(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	mClock := quartz.NewMock(t)
	root, cancelRoot := mClock.WithTimeout(context.Background(), time.Hour)
	detached, cancelDetached := mClock.WithTimeout(context.WithoutCancel(root), time.Hour)
	layer := &blockingErrCtx{
		Context: detached,
		arm:     make(chan struct{}, 1),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	_, cancelLast := mClock.WithTimeout(layer, time.Hour)
	defer cancelLast()
	kept, cancelKept := mClock.WithTimeout(context.WithoutCancel(root), time.Hour)
	defer cancelKept()

	// Hold the detached context's cancellation where it reads layer.
	layer.arm <- struct{}{}
	detachedDone := make(chan struct{})
	go func() {
		cancelDetached()
		close(detachedDone)
	}()
	select {
	case <-layer.entered:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for the detached cancellation to read the layer")
	}
	// If cancelRoot waits for the detached cancellation, go test -timeout reports it by naming this
	// test.
	cancelRoot()
	close(layer.release)
	<-detachedDone
	if err := kept.Err(); err != nil {
		t.Fatalf("expected the other context behind WithoutCancel to stay live, got %v", err)
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
	// also cancels the mock contexts and would hide a gap in the others.
	observe := map[string]func(t *testing.T, mClock *quartz.Mock, ctxs []context.Context){
		"Err": func(t *testing.T, _ *quartz.Mock, ctxs []context.Context) {
			for i, ctx := range ctxs {
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("context %d: expected Canceled, got %v", i, ctx.Err())
				}
			}
		},
		"Done": func(t *testing.T, _ *quartz.Mock, ctxs []context.Context) {
			for i, ctx := range ctxs {
				select {
				case <-ctx.Done():
				default:
					t.Fatalf("context %d: expected Done to be closed", i)
				}
			}
		},
		"Cause": func(t *testing.T, _ *quartz.Mock, ctxs []context.Context) {
			for i, ctx := range ctxs {
				if got := context.Cause(ctx); !errors.Is(got, context.Canceled) {
					t.Fatalf("context %d: expected Canceled cause, got %v", i, got)
				}
			}
		},
		"Peek": func(t *testing.T, mClock *quartz.Mock, _ []context.Context) {
			if d, ok := mClock.Peek(); ok {
				t.Fatalf("expected no pending events, got Peek()=%s", d)
			}
		},
		"Advance": func(t *testing.T, mClock *quartz.Mock, _ []context.Context) {
			// The canceled contexts must not block advancing past their old deadline.
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.Advance(2 * time.Hour).MustWait(testCtx)
		},
		"Set": func(t *testing.T, mClock *quartz.Mock, _ []context.Context) {
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.Set(mClock.Now().Add(2 * time.Hour)).MustWait(testCtx)
		},
		"AdvanceNext": func(t *testing.T, mClock *quartz.Mock, _ []context.Context) {
			testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer testCancel()
			mClock.NewTimer(2 * time.Hour) // AdvanceNext fails the test when nothing is scheduled.
			d, w := mClock.AdvanceNext()
			w.MustWait(testCtx)
			if d != 2*time.Hour {
				t.Fatalf("expected AdvanceNext to skip the canceled contexts, advanced %s", d)
			}
		},
	}
	for name, fn := range observe {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mClock := quartz.NewMock(t)
			parent, parentCancel := context.WithCancel(context.Background())
			// The parent's cancel starts a goroutine for each mock context, so with many of them
			// the observer reads some while their goroutine is still canceling them.
			var ctxs []context.Context
			for range 200 {
				ctx, cancel := mClock.WithTimeout(parent, time.Hour)
				t.Cleanup(cancel)
				ctxs = append(ctxs, ctx)
			}

			// As with stdlib contexts, the cancellation is visible as soon as parentCancel returns.
			parentCancel()
			fn(t, mClock, ctxs)
		})
	}
}

// When a parent that is not a mock context is canceled, a mock context learns of it on another
// goroutine, so a Done channel obtained before, and the Done of a derived context, close a moment
// later.
func TestWithTimeout_ParentCanceledClosesDone(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, *quartz.Mock) (context.Context, context.CancelFunc){
		"Stdlib": func(*testing.T, *quartz.Mock) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		// The stdlib layer can be canceled without the mock context behind it, so the child must
		// also register with the layer.
		"WithCancelOfMock": func(
			t *testing.T, mClock *quartz.Mock,
		) (context.Context, context.CancelFunc) {
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
			dones := map[string]<-chan struct{}{"ctx": ctxDone, "child": child.Done()}
			for name, done := range dones {
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

// Failures to move the clock name the events that block it, and the AdvanceNext failure lists the
// kinds of event it looks for, so a pending mock context is not mistaken for a timer or ticker.
func TestMock_FailureNamesBlockingEvents(t *testing.T) {
	t.Parallel()

	advanceHour := func(mClock *quartz.Mock) { mClock.Advance(time.Hour) }
	tests := map[string]struct {
		schedule func(t *testing.T, mClock *quartz.Mock, tags []string)
		move     func(mClock *quartz.Mock)
		want     string
	}{
		"AdvancePastWithTimeout": {
			schedule: func(t *testing.T, mClock *quartz.Mock, tags []string) {
				_, cancel := mClock.WithTimeout(context.Background(), 30*time.Second, tags...)
				t.Cleanup(cancel)
			},
			move: advanceHour,
			want: "cannot advance 1h0m0s which is beyond next event in 30s: " +
				"mock context from WithTimeout(..., [a])",
		},
		"AdvancePastNewTimer": {
			schedule: func(_ *testing.T, mClock *quartz.Mock, tags []string) {
				mClock.NewTimer(30*time.Second, tags...)
			},
			move: advanceHour,
			want: "cannot advance 1h0m0s which is beyond next event in 30s: " +
				"timer from NewTimer(..., [a])",
		},
		"AdvancePastAfterFunc": {
			schedule: func(_ *testing.T, mClock *quartz.Mock, tags []string) {
				mClock.AfterFunc(30*time.Second, func() {}, tags...)
			},
			move: advanceHour,
			want: "cannot advance 1h0m0s which is beyond next event in 30s: " +
				"timer from AfterFunc(..., [a])",
		},
		"AdvancePastNewTicker": {
			schedule: func(_ *testing.T, mClock *quartz.Mock, tags []string) {
				mClock.NewTicker(30*time.Second, tags...)
			},
			move: advanceHour,
			want: "cannot advance 1h0m0s which is beyond next event in 30s: " +
				"ticker from NewTicker(..., [a])",
		},
		"AdvancePastTickerFunc": {
			schedule: func(t *testing.T, mClock *quartz.Mock, tags []string) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				mClock.TickerFunc(ctx, 30*time.Second, func() error { return nil }, tags...)
			},
			move: advanceHour,
			want: "cannot advance 1h0m0s which is beyond next event in 30s: " +
				"ticker from TickerFunc(..., [a])",
		},
		"SetPastTwoEvents": {
			schedule: func(t *testing.T, mClock *quartz.Mock, tags []string) {
				mClock.NewTimer(30 * time.Second)
				deadline := mClock.Now().Add(30 * time.Second)
				_, cancel := mClock.WithDeadline(context.Background(), deadline, tags...)
				t.Cleanup(cancel)
			},
			move: func(mClock *quartz.Mock) { mClock.Set(mClock.Now().Add(time.Hour)) },
			want: "cannot Set time to 2024-01-01 01:00:00 +0000 UTC " +
				"which is beyond next event at 2024-01-01 00:00:30 +0000 UTC: " +
				"timer from NewTimer(..., []), mock context from WithDeadline(..., [a])",
		},
		"SetBackward": {
			schedule: func(t *testing.T, mClock *quartz.Mock, tags []string) {
				_, cancel := mClock.WithTimeout(context.Background(), 30*time.Second, tags...)
				t.Cleanup(cancel)
			},
			move: func(mClock *quartz.Mock) { mClock.Set(mClock.Now().Add(-time.Hour)) },
			want: "Set mock clock to the past while events are scheduled, next at " +
				"2024-01-01 00:00:30 +0000 UTC: mock context from WithTimeout(..., [a])",
		},
		"AdvanceNextWithNothingScheduled": {
			schedule: func(*testing.T, *quartz.Mock, []string) {},
			move:     func(mClock *quartz.Mock) { mClock.AdvanceNext() },
			want: "cannot AdvanceNext because no timers, tickers, or mock contexts are " +
				"scheduled",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tb := &captureFailTB{TB: t}
			mClock := quartz.NewMock(tb)
			tags := []string{"a"}
			tt.schedule(t, mClock, tags)
			// The caller may reuse its tags slice once the call returns. The failure still reports
			// the tags the event was created with.
			tags[0] = "reused"

			tt.move(mClock)
			if want := []string{tt.want}; !slices.Equal(tb.errs, want) {
				t.Fatalf("expected errors %q, got %q", want, tb.errs)
			}
		})
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
	// errs records Error and Errorf messages.
	errs []string
	testing.TB
}

func (t *captureFailTB) Errorf(format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	t.errs = append(t.errs, fmt.Sprintf(format, args...))
	t.failed = true
}

func (t *captureFailTB) Error(args ...any) {
	t.Helper()
	t.Log(args...)
	t.errs = append(t.errs, fmt.Sprint(args...))
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

	// The timer's old 1h schedule must not remain in the mock's event list. If it does, Advance
	// rejects with "cannot advance 2h0m0s which is beyond next event".
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
