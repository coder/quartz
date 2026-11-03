package quartz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// TestingT is the minimal interface required from a testing framework for the Mock.
type TestingT interface {
	Helper()

	Log(...any)
	Logf(string, ...any)
	Error(...any)
	Errorf(string, ...any)
	Fatal(...any)
	Fatalf(string, ...any)

	Cleanup(func())
}

// ErrMockClockTestEnded is the context.Cause of a mock context that was still live when the test
// ended; its Err is context.Canceled. Each context is canceled by a cleanup registered when the
// context was created, so it is still live in cleanups registered after that, which run first. A
// context created after the cleanup registered by NewMock has run, in a cleanup registered before
// NewMock or by a goroutine that outlived the test, is returned already canceled with this cause.
var ErrMockClockTestEnded = errors.New("quartz: mock clock test ended")

// Mock is the testing implementation of Clock.  It tracks a time that monotonically increases
// during a test, triggering scheduled events automatically. Timers, tickers, and mock contexts are
// events. A mock context is a context from WithDeadline, WithTimeout, or their Cause variants, and
// it is scheduled until it expires or is canceled.
type Mock struct {
	tb       TestingT
	logger   Logger
	mu       sync.Mutex
	testOver bool

	// cur is the current time
	cur time.Time

	all        []event
	nextTime   time.Time
	nextEvents []event
	traps      []*Trap
}

type event interface {
	next() time.Time
	fire(t time.Time)
	// describe names the event for failure messages.
	describe() string
}

// eventSource is the Clock call that created an event.
type eventSource struct {
	fn   clockFunction
	tags []string
}

// describe formats the call like Trap.String, so the user can find the call or trap it by function
// and tags.
func (s eventSource) describe(kind string) string {
	return fmt.Sprintf("%s from %s(..., %v)", kind, s.fn, s.tags)
}

// newEventSource copies the tags, which may alias the caller's variadic slice. The caller may reuse
// that slice once the call returns, while describe reads the tags later.
func newEventSource(c *apiCall) eventSource {
	return eventSource{fn: c.fn, tags: slices.Clone(c.Tags)}
}

func (m *Mock) TickerFunc(ctx context.Context, d time.Duration, f func() error, tags ...string) Waiter {
	if d <= 0 {
		panic("TickerFunc called with negative or zero duration")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionTickerFunc, tags, withDuration(d))
	m.matchCallLocked(c)
	defer close(c.complete)
	t := &mockTickerFunc{
		ctx:  ctx,
		d:    d,
		f:    f,
		nxt:  m.cur.Add(d),
		mock: m,
		cond: sync.NewCond(&m.mu),
		src:  newEventSource(c),
	}
	m.all = append(m.all, t)
	m.recomputeNextLocked()
	go t.waitForCtx()
	return t
}

// NewTicker creates a mocked ticker attached to this Mock. Note that it will cease sending ticks on its channel at the
// end of the test, to avoid leaking any goroutines. Ticks are suppressed even if the mock clock is advanced after the
// test completes. Best practice is to only manipulate the mock time in the main goroutine of the test.
func (m *Mock) NewTicker(d time.Duration, tags ...string) *Ticker {
	if d <= 0 {
		panic("NewTicker called with negative or zero duration")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionNewTicker, tags, withDuration(d))
	m.matchCallLocked(c)
	defer close(c.complete)
	return newMockTickerLocked(m, d, newEventSource(c))
}

func (m *Mock) NewTimer(d time.Duration, tags ...string) *Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionNewTimer, tags, withDuration(d))
	defer close(c.complete)
	m.matchCallLocked(c)
	ch := make(chan time.Time)
	t := &Timer{
		C:    ch,
		c:    ch,
		nxt:  m.cur.Add(d),
		mock: m,
		src:  newEventSource(c),
	}
	if d <= 0 {
		// zero or negative duration timer means we should immediately fire
		// it, rather than add it.
		go t.fire(t.mock.cur)
		return t
	}
	m.addEventLocked(t)
	return t
}

func (m *Mock) AfterFunc(d time.Duration, f func(), tags ...string) *Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionAfterFunc, tags, withDuration(d))
	defer close(c.complete)
	m.matchCallLocked(c)
	t := &Timer{
		nxt:  m.cur.Add(d),
		fn:   f,
		mock: m,
		src:  newEventSource(c),
	}
	if d <= 0 {
		// zero or negative duration timer means we should immediately fire
		// it, rather than add it.
		go t.fire(t.mock.cur)
		return t
	}
	m.addEventLocked(t)
	return t
}

func (m *Mock) Now(tags ...string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionNow, tags)
	defer close(c.complete)
	m.matchCallLocked(c)
	return m.cur
}

func (m *Mock) Since(t time.Time, tags ...string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionSince, tags, withTime(t))
	defer close(c.complete)
	m.matchCallLocked(c)
	return m.cur.Sub(t)
}

func (m *Mock) Until(t time.Time, tags ...string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newCall(clockFunctionUntil, tags, withTime(t))
	defer close(c.complete)
	m.matchCallLocked(c)
	return t.Sub(m.cur)
}

func (m *Mock) WithDeadline(parent context.Context, d time.Time, tags ...string) (context.Context, context.CancelFunc) {
	c := newCall(clockFunctionWithDeadline, tags, withTime(d))
	return m.newMockCtx(parent, c, nil, func() time.Time { return d })
}

func (m *Mock) WithDeadlineCause(parent context.Context, d time.Time, cause error, tags ...string) (context.Context, context.CancelFunc) {
	c := newCall(clockFunctionWithDeadlineCause, tags, withTime(d), withCause(cause))
	return m.newMockCtx(parent, c, cause, func() time.Time { return d })
}

func (m *Mock) WithTimeout(parent context.Context, d time.Duration, tags ...string) (context.Context, context.CancelFunc) {
	c := newCall(clockFunctionWithTimeout, tags, withDuration(d))
	return m.newMockCtx(parent, c, nil, func() time.Time { return m.cur.Add(d) })
}

func (m *Mock) WithTimeoutCause(parent context.Context, d time.Duration, cause error, tags ...string) (context.Context, context.CancelFunc) {
	c := newCall(clockFunctionWithTimeoutCause, tags, withDuration(d), withCause(cause))
	return m.newMockCtx(parent, c, cause, func() time.Time { return m.cur.Add(d) })
}

// newMockCtx creates the context for call. deadlineLocked is evaluated with m.mu held after any
// traps release the call.
func (m *Mock) newMockCtx(parent context.Context, call *apiCall, cause error, deadlineLocked func() time.Time) (context.Context, context.CancelFunc) {
	// inner holds values and the cancellation cause. It does not inherit parent's cancellation, so
	// only cancel() decides which error and cause the context reports.
	inner, innerCancel := context.WithCancelCause(context.WithoutCancel(parent))
	wallDeadline, ok := parent.Deadline()
	if !ok {
		wallDeadline = noWallDeadline
	}
	c := &mockCtx{
		parent:       parent,
		inner:        inner,
		innerCancel:  innerCancel,
		wallDeadline: wallDeadline,
		finished:     make(chan struct{}),
		cause:        cause,
		mock:         m,
		src:          newEventSource(call),
		done:         make(chan struct{}),
	}

	m.mu.Lock()
	m.matchCallLocked(call)
	c.deadline = deadlineLocked()
	pastDeadline := !c.deadline.After(m.cur)
	// After the cleanup registered by NewMock has run, the cleanup below runs only after the
	// caller's cleanup returns, or never if the test has finished, so cancel the context now.
	testOver := m.testOver
	if !pastDeadline {
		m.addEventLocked(c)
	}
	m.tb.Cleanup(func() { c.cancel(context.Canceled, ErrMockClockTestEnded) })
	close(call.complete)
	m.mu.Unlock()

	if testOver {
		c.cancel(context.Canceled, ErrMockClockTestEnded)
	} else if pastDeadline {
		c.cancel(context.DeadlineExceeded, cause)
	}
	var stop func() bool
	// Register with the nearest mock context, also behind layers that forward Value such as
	// context.WithValue, so c is canceled before that context's cancellation returns, as stdlib
	// children are. When Done differs, a layer between them, such as context.WithCancel, can
	// also be canceled on its own, as in stdlib's parentCancelCtx, so c registers with parent as
	// well.
	p, ok := parent.Value(mockCtxViewKey{}).(*mockCtxView)
	switch {
	case ok && parent.Done() == p.std.Done():
		stop = p.c.registerChild(c)
	case ok:
		stopChild, stopAfterFunc := p.c.registerChild(c), context.AfterFunc(parent, c.checkParent)
		stop = func() bool {
			stopped := stopChild()
			return stopAfterFunc() || stopped
		}
	default:
		stop = context.AfterFunc(parent, c.checkParent)
	}
	c.mock.mu.Lock()
	c.stopParent = stop
	done := c.err != nil
	c.mock.mu.Unlock()
	if done {
		stop()
	}
	std, stdCancel := context.WithCancel(c)
	return &mockCtxView{std: std, c: c}, func() {
		c.cancel(context.Canceled, nil)
		// If another goroutine canceled c first, it may still be canceling std and the mock
		// contexts registered with c. Wait for that, as a stdlib cancel that loses a race does.
		// finished does not wait for the functions that unregister those contexts from their
		// parents, which can block in user code. stdCancel is then a no-op, called because
		// WithCancel requires it.
		<-c.finished
		<-std.Done()
		stdCancel()
	}
}

// checkDoneContexts cancels contexts whose parents are done, so their pending events do not block
// Advance or show up in Peek.
func (m *Mock) checkDoneContexts() {
	m.mu.Lock()
	var ctxs []*mockCtx
	for _, e := range m.all {
		if c, ok := e.(*mockCtx); ok {
			ctxs = append(ctxs, c)
		}
	}
	m.mu.Unlock()
	for _, c := range ctxs {
		c.checkParent()
	}
}

func (m *Mock) addEventLocked(e event) {
	m.all = append(m.all, e)
	m.recomputeNextLocked()
}

func (m *Mock) recomputeNextLocked() {
	var best time.Time
	var events []event
	for _, e := range m.all {
		if best.IsZero() || e.next().Before(best) {
			best = e.next()
			events = []event{e}
			continue
		}
		if e.next().Equal(best) {
			events = append(events, e)
			continue
		}
	}
	m.nextTime = best
	m.nextEvents = events
}

// describeNextEventsLocked lists the events scheduled at nextTime for failure messages.
func (m *Mock) describeNextEventsLocked() string {
	descs := make([]string, 0, len(m.nextEvents))
	for _, e := range m.nextEvents {
		descs = append(descs, e.describe())
	}
	return strings.Join(descs, ", ")
}

func (m *Mock) removeTimer(t *Timer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeTimerLocked(t)
}

func (m *Mock) removeTimerLocked(t *Timer) {
	t.stopped = true
	m.removeEventLocked(t)
}

func (m *Mock) removeEventLocked(e event) {
	defer m.recomputeNextLocked()
	for i := range m.all {
		if m.all[i] == e {
			m.all = append(m.all[:i], m.all[i+1:]...)
			return
		}
	}
}

func (m *Mock) matchCallLocked(c *apiCall) {
	var traps []*Trap
	for _, t := range m.traps {
		if t.matches(c) {
			traps = append(traps, t)
		}
	}
	if !m.testOver {
		m.logger.Logf("Mock Clock - %s call, matched %d traps", c, len(traps))
	}
	if len(traps) == 0 {
		return
	}
	c.releases.Add(len(traps))
	m.mu.Unlock()
	for _, t := range traps {
		go t.catch(c)
	}
	c.releases.Wait()
	m.mu.Lock()
}

// AdvanceWaiter is returned from Advance and Set calls and allows you to wait for the triggered
// events to complete.  In the case of functions passed to AfterFunc or TickerFunc, it waits for the
// functions to return.  For other ticks & timers, it just waits for the tick to be delivered to
// the channel.  For a mock context, it waits until the context is done, along with contexts derived
// from it through context.WithCancel, WithTimeout, WithDeadline, or this Mock's With methods. A
// context derived through context.WithValue is canceled on another goroutine, so it may not be done
// yet when Wait returns.
//
// If multiple events trigger simultaneously, they are all run on separate go routines.
type AdvanceWaiter struct {
	tb TestingT
	ch chan struct{}
}

// Wait for all triggered events to complete, or until ctx expires.
func (w AdvanceWaiter) Wait(ctx context.Context) error {
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MustWait waits for all triggered events to complete, and fails the test immediately if the
// context completes first.  MustWait must be called from the goroutine running the test or
// benchmark, similar to `t.FailNow()`.
func (w AdvanceWaiter) MustWait(ctx context.Context) {
	w.tb.Helper()
	select {
	case <-w.ch:
		return
	case <-ctx.Done():
		w.tb.Fatalf("context expired while waiting for clock to advance: %s", ctx.Err())
	}
}

// Done returns a channel that is closed when all triggered events complete.
func (w AdvanceWaiter) Done() <-chan struct{} {
	return w.ch
}

// Advance moves the clock forward by d, triggering any events scheduled at that time.  The returned
// value can be used to wait for the triggered events to complete.  Advance sets the clock forward
// before returning, and can only advance up to the next scheduled event. It will fail the test if
// you attempt to advance beyond.
//
// If you need to advance exactly to the next event, and don't know or don't wish to calculate it,
// consider AdvanceNext().
func (m *Mock) Advance(d time.Duration) AdvanceWaiter {
	m.tb.Helper()
	w := AdvanceWaiter{tb: m.tb, ch: make(chan struct{})}
	m.checkDoneContexts()
	m.mu.Lock()
	if !m.testOver {
		m.logger.Logf("Mock Clock - Advance(%s)", d)
	}
	fin := m.cur.Add(d)
	// nextTime.IsZero implies no events scheduled.
	if m.nextTime.IsZero() || fin.Before(m.nextTime) {
		m.cur = fin
		m.mu.Unlock()
		close(w.ch)
		return w
	}
	if fin.After(m.nextTime) {
		m.tb.Errorf("cannot advance %s which is beyond next event in %s: %s",
			d.String(), m.nextTime.Sub(m.cur), m.describeNextEventsLocked())
		m.mu.Unlock()
		close(w.ch)
		return w
	}

	m.cur = m.nextTime
	go m.advanceLocked(w)
	return w
}

func (m *Mock) advanceLocked(w AdvanceWaiter) {
	defer close(w.ch)
	wg := sync.WaitGroup{}
	for i := range m.nextEvents {
		e := m.nextEvents[i]
		t := m.cur
		wg.Add(1)
		go func() {
			e.fire(t)
			wg.Done()
		}()
	}
	// release the lock and let the events resolve.  This allows them to call back into the
	// Mock to query the time or set new timers.  Each event should remove or reschedule
	// itself from nextEvents.
	m.mu.Unlock()
	wg.Wait()
}

// Set the time to t.  If the time is after the current mocked time, then this is equivalent to
// Advance() with the difference.  You may only Set the time earlier than the current time while
// no events are scheduled (e.g. at the start of your test case).
func (m *Mock) Set(t time.Time) AdvanceWaiter {
	m.tb.Helper()
	w := AdvanceWaiter{tb: m.tb, ch: make(chan struct{})}
	m.checkDoneContexts()
	m.mu.Lock()
	if !m.testOver {
		m.logger.Logf("Mock Clock - Set(%s)", t)
	}
	if t.Before(m.cur) {
		defer close(w.ch)
		defer m.mu.Unlock()
		// past
		if !m.nextTime.IsZero() {
			m.tb.Errorf("Set mock clock to the past while events are scheduled, next at %s: %s",
				m.nextTime, m.describeNextEventsLocked())
		}
		m.cur = t
		return w
	}
	// future
	// nextTime.IsZero implies no events scheduled.
	if m.nextTime.IsZero() || t.Before(m.nextTime) {
		defer close(w.ch)
		defer m.mu.Unlock()
		m.cur = t
		return w
	}
	if t.After(m.nextTime) {
		defer close(w.ch)
		defer m.mu.Unlock()
		m.tb.Errorf("cannot Set time to %s which is beyond next event at %s: %s",
			t.String(), m.nextTime, m.describeNextEventsLocked())
		return w
	}

	m.cur = m.nextTime
	go m.advanceLocked(w)
	return w
}

// AdvanceNext advances the clock to the next scheduled event.  It fails the test if there are none
// scheduled.  It returns the duration the clock was advanced and a waiter that can be used to wait
// for the event(s) to finish.
func (m *Mock) AdvanceNext() (time.Duration, AdvanceWaiter) {
	m.checkDoneContexts()
	m.mu.Lock()
	if !m.testOver {
		m.logger.Logf("Mock Clock - AdvanceNext()")
	}
	m.tb.Helper()
	w := AdvanceWaiter{tb: m.tb, ch: make(chan struct{})}
	if m.nextTime.IsZero() {
		defer close(w.ch)
		defer m.mu.Unlock()
		m.tb.Error("cannot AdvanceNext because no timers, tickers, or mock contexts are scheduled")
		return 0, w
	}
	d := m.nextTime.Sub(m.cur)
	m.cur = m.nextTime
	go m.advanceLocked(w)
	return d, w
}

// Peek returns the duration until the next scheduled event and the value true, or, if there are
// no scheduled events, it returns zero and false.
func (m *Mock) Peek() (d time.Duration, ok bool) {
	m.checkDoneContexts()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nextTime.IsZero() {
		return 0, false
	}
	return m.nextTime.Sub(m.cur), true
}

// Trapper allows the creation of Traps
type Trapper struct {
	// mock is the underlying Mock.  This is a thin wrapper around Mock so that
	// we can have our interface look like mClock.Trap().NewTimer("foo")
	mock *Mock
}

func (t Trapper) NewTimer(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionNewTimer, tags)
}

func (t Trapper) AfterFunc(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionAfterFunc, tags)
}

func (t Trapper) TimerStop(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTimerStop, tags)
}

func (t Trapper) TimerReset(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTimerReset, tags)
}

func (t Trapper) TickerFunc(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTickerFunc, tags)
}

func (t Trapper) TickerFuncWait(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTickerFuncWait, tags)
}

func (t Trapper) NewTicker(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionNewTicker, tags)
}

func (t Trapper) TickerStop(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTickerStop, tags)
}

func (t Trapper) TickerReset(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionTickerReset, tags)
}

func (t Trapper) Now(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionNow, tags)
}

func (t Trapper) Since(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionSince, tags)
}

func (t Trapper) Until(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionUntil, tags)
}

func (t Trapper) WithDeadline(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionWithDeadline, tags)
}

func (t Trapper) WithDeadlineCause(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionWithDeadlineCause, tags)
}

func (t Trapper) WithTimeout(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionWithTimeout, tags)
}

func (t Trapper) WithTimeoutCause(tags ...string) *Trap {
	return t.mock.newTrap(clockFunctionWithTimeoutCause, tags)
}

func (m *Mock) Trap() Trapper {
	return Trapper{m}
}

func (m *Mock) newTrap(fn clockFunction, tags []string) *Trap {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.testOver {
		m.logger.Logf("Mock Clock - Trap %s(..., %v)", fn, tags)
	}
	tr := &Trap{
		fn:    fn,
		tags:  tags,
		mock:  m,
		calls: make(chan *apiCall),
		done:  make(chan struct{}),
	}
	m.traps = append(m.traps, tr)
	return tr
}

// WithLogger replaces the default testing logger with a custom one.
//
// This can be used to discard log messages with:
//
//	quartz.NewMock(t).WithLogger(quartz.NoOpLogger)
func (m *Mock) WithLogger(l Logger) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = l
	return m
}

// NewMock creates a new Mock with the time set to midnight UTC on Jan 1, 2024.
// You may re-set the time earlier than this, but only before any events are
// scheduled.
func NewMock(tb TestingT) *Mock {
	cur, err := time.Parse(time.RFC3339, "2024-01-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	m := &Mock{
		tb:     tb,
		logger: tb,
		cur:    cur,
	}
	tb.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.testOver = true
		m.logger.Logf("Mock Clock - test cleanup; will no longer log clock events")
	})
	return m
}

var _ Clock = &Mock{}

type mockTickerFunc struct {
	ctx  context.Context
	d    time.Duration
	f    func() error
	nxt  time.Time
	mock *Mock

	// cond is a condition Locked on the main Mock.mu
	cond *sync.Cond
	// inProgress is true when we are actively calling f
	inProgress bool
	// done is true when the ticker exits
	done bool
	// err holds the error when the ticker exits
	err error
	// src is the mock call that created the ticker
	src eventSource
}

func (m *mockTickerFunc) next() time.Time {
	return m.nxt
}

func (m *mockTickerFunc) describe() string {
	return m.src.describe("ticker")
}

func (m *mockTickerFunc) fire(_ time.Time) {
	m.mock.mu.Lock()
	if m.done {
		m.mock.mu.Unlock()
		return
	}
	m.nxt = m.nxt.Add(m.d)
	m.mock.recomputeNextLocked()
	// we need this check to happen after we've computed the next tick,
	// otherwise it will be immediately rescheduled.
	if m.inProgress {
		m.mock.mu.Unlock()
		return
	}

	m.inProgress = true
	m.mock.mu.Unlock()
	err := m.f()
	m.mock.mu.Lock()
	defer m.mock.mu.Unlock()
	m.inProgress = false
	m.cond.Broadcast() // wake up anything waiting for f to finish
	if err != nil {
		m.exitLocked(err)
	}
}

func (m *mockTickerFunc) exitLocked(err error) {
	if m.done {
		return
	}
	m.done = true
	m.err = err
	m.mock.removeEventLocked(m)
	m.cond.Broadcast()
}

func (m *mockTickerFunc) waitForCtx() {
	<-m.ctx.Done()
	// Read before taking mock.mu: Err() takes mock.mu when ctx is a context from this Mock.
	err := m.ctx.Err()
	m.mock.mu.Lock()
	defer m.mock.mu.Unlock()
	for m.inProgress {
		m.cond.Wait()
	}
	m.exitLocked(err)
}

func (m *mockTickerFunc) Wait(tags ...string) error {
	m.mock.mu.Lock()
	defer m.mock.mu.Unlock()
	c := newCall(clockFunctionTickerFuncWait, tags)
	m.mock.matchCallLocked(c)
	defer close(c.complete)
	for !m.done {
		m.cond.Wait()
	}
	return m.err
}

var _ Waiter = &mockTickerFunc{}

type mockCtx struct {
	mock   *Mock
	parent context.Context
	src    eventSource

	// inner stores the cancellation cause so context.Cause() can find it via Value(&cancelCtxKey).
	// Done() must NOT return inner.Done(): stdlib's parentCancelCtx fast-path would then attach
	// descendants directly to inner, and they would read inner's context.Canceled Err() on deadline
	// fire instead of ours.
	inner       context.Context
	innerCancel context.CancelCauseFunc
	// deadline is in mock time and schedules the event.
	deadline time.Time
	// wallDeadline is what Deadline() reports; see noWallDeadline.
	wallDeadline time.Time
	// children holds the mock contexts registered through registerChild and is guarded by
	// mock.mu. finished is closed once c and every mock context canceled with it are canceled. It
	// does not wait for cancel to unregister them from their parents.
	children []*mockCtx
	finished chan struct{}

	// Cause recorded when the deadline fires.
	cause error

	// err, done, afterFuncs, and stopParent are guarded by mock.mu. err and done are written
	// together.
	err        error
	done       chan struct{}
	afterFuncs []*afterFunc
	stopParent func() bool
}

// afterFunc gives each registration a pointer identity for stop to find; funcs are not comparable.
type afterFunc struct{ f func() }

// noWallDeadline is what Deadline() reports for a mock context whose parent has no deadline.
//
// Code that reads ctx.Deadline() compares it with time.Now() or hands it to wall-clock APIs such as
// net.Dialer, net.Conn.SetDeadline, http.Client, gRPC, and x/time/rate. Mock time is unrelated to
// wall time, so reporting the mock deadline makes those consumers see an expired deadline, and
// converting it to wall time would make a mock timeout expire real I/O. Reporting the parent's
// deadline, or a deadline that never arrives, keeps ok == true for code that requires a deadline
// while leaving expiry to Done() and Err(), which follow mock time.
var noWallDeadline = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// AfterFunc lets the stdlib context behind c's mockCtxView register for cancellation instead of
// starting a goroutine that waits on Done (see context.afterFuncer). Registered functions run
// before the call that cancels c returns, so that context and its descendants are done by the time
// cancel() or Advance().MustWait() returns.
func (c *mockCtx) AfterFunc(f func()) (stop func() bool) {
	c.mock.mu.Lock()
	defer c.mock.mu.Unlock()
	if c.err != nil {
		// The caller may hold locks that f takes, so f must not run on this goroutine.
		go f()
		return func() bool { return false }
	}
	a := &afterFunc{f: f}
	c.afterFuncs = append(c.afterFuncs, a)
	return func() bool {
		c.mock.mu.Lock()
		defer c.mock.mu.Unlock()
		for i, x := range c.afterFuncs {
			if x == a {
				// slices.Delete zeroes the vacated slot, so the backing array stops referencing
				// the stopped function and the done child context it may hold.
				c.afterFuncs = slices.Delete(c.afterFuncs, i, i+1)
				return true
			}
		}
		return false
	}
}

// registerChild registers child, a mock context created from c's mockCtxView directly or behind
// other layers, so c's cancellation cancels it before the call that cancels c returns. Unlike an
// AfterFunc registration, cancel unregisters child from its parents only after it has canceled
// every context registered with c, directly or below.
func (c *mockCtx) registerChild(child *mockCtx) (stop func() bool) {
	c.mock.mu.Lock()
	defer c.mock.mu.Unlock()
	if c.err != nil {
		// checkParent takes mock.mu, which this goroutine holds.
		go child.checkParent()
		return func() bool { return false }
	}
	c.children = append(c.children, child)
	return func() bool {
		c.mock.mu.Lock()
		defer c.mock.mu.Unlock()
		i := slices.Index(c.children, child)
		if i < 0 {
			return false
		}
		// slices.Delete zeroes the vacated slot, so the backing array stops referencing child.
		c.children = slices.Delete(c.children, i, i+1)
		return true
	}
}

func (c *mockCtx) Deadline() (time.Time, bool) {
	return c.wallDeadline, true
}

// mockCtxView is the context that WithDeadline, WithTimeout, and their Cause variants return. Its
// Done, Err, and Value come from std, the context.WithCancel(c) that newMockCtx creates. std
// registers with c through c.AfterFunc, so it is canceled with c's error and cause before the
// call that cancels c returns. Stdlib finds std through Done and Value and attaches derived
// contexts to it directly, also through layers that do not cancel, such as context.WithValue, so
// they are done when std is.
type mockCtxView struct {
	std context.Context
	c   *mockCtx
}

// mockCtxViewKey is the Value key under which a mockCtxView returns itself, so newMockCtx can find
// the nearest mock context behind layers such as context.WithValue.
type mockCtxViewKey struct{}

func (v *mockCtxView) Deadline() (time.Time, bool) {
	return v.c.Deadline()
}

// Done, Err, and Value check the parent first because stdlib contexts are done as soon as their
// parent is, but c may learn of its parent's cancellation from a goroutine. Only the cancellation
// of the mock context that c registered with reaches c on the canceling goroutine. A cancellation
// that starts anywhere else, such as at a parent from the context package or at a layer between c
// and that mock context, reaches c from a goroutine, as does one that happened before c
// registered. Value is included because context.Cause reads the cause through it. A Done channel
// obtained before the parent was canceled closes when that goroutine runs.
func (v *mockCtxView) Done() <-chan struct{} {
	v.checkParent()
	return v.std.Done()
}

func (v *mockCtxView) Value(key any) any {
	if key == (mockCtxViewKey{}) {
		return v
	}
	v.checkParent()
	return v.std.Value(key)
}

func (v *mockCtxView) Err() error {
	v.checkParent()
	return v.std.Err()
}

// checkParent cancels c if its parent is done, then waits for std if c is done. std is canceled
// after c: by the goroutine that canceled c, or by a goroutine that c.AfterFunc starts if c was
// canceled after context.WithCancel found c's Done open but before it called c.AfterFunc. Without
// the wait, Err could return nil after the parent's cancel returned, because c.checkParent returns
// at once while the parent's AfterFunc goroutine is canceling c.
func (v *mockCtxView) checkParent() {
	v.c.checkParent()
	select {
	case <-v.c.done:
		<-v.std.Done()
	default:
	}
}

// Done, Err, and Value report c's own state without checking the parent; mockCtxView checks it.
func (c *mockCtx) Done() <-chan struct{} {
	return c.done
}

func (c *mockCtx) Value(key any) any {
	return c.inner.Value(key)
}

func (c *mockCtx) Err() error {
	c.mock.mu.Lock()
	defer c.mock.mu.Unlock()
	return c.err
}

func (c *mockCtx) next() time.Time {
	return c.deadline
}

func (c *mockCtx) describe() string {
	return c.src.describe("mock context")
}

func (c *mockCtx) fire(_ time.Time) {
	c.cancel(context.DeadlineExceeded, c.cause)
	// Advance().MustWait() waits for fire, so wait for mock descendants that other goroutines are
	// still canceling.
	<-c.finished
}

// checkParent cancels c if its parent is done. A canceled c returns without reading the parent:
// cancel reads the parent again, and each read checks the grandparent the same way, so reading a
// canceled chain of mock contexts would cost parent reads exponential in its depth.
func (c *mockCtx) checkParent() {
	select {
	case <-c.done:
		return
	default:
	}
	if c.parent.Err() != nil {
		c.cancel(context.Canceled, nil)
	}
}

// cancel cancels c and the contexts registered with it, then unregisters them from their parents.
// It matches stdlib cancelCtx.cancel(): a nil cause defaults to err. If the parent is already done,
// its error and cause win, as they do when stdlib propagates cancellation.
func (c *mockCtx) cancel(err, cause error) {
	var stops []func() bool
	c.cancelTree(err, cause, &stops)
	for _, stop := range stops {
		stop()
	}
}

// cancelTree cancels c, then the contexts registered with it, and closes finished once they are
// done. It appends the functions that unregister c and its mock descendants from their parents to
// stops instead of calling them: they call into the parents, which can block in user code, and
// callers of cancel() and fire wait for finished.
//
// cancelTree takes mock.mu. It reads the parent's error and cause before that, because parent.Err()
// also takes mock.mu when parent is a context from this Mock.
func (c *mockCtx) cancelTree(err, cause error, stops *[]func() bool) {
	if perr := c.parent.Err(); perr != nil {
		err, cause = perr, context.Cause(c.parent)
	}
	if cause == nil {
		cause = err
	}
	c.mock.mu.Lock()
	if c.err != nil {
		c.mock.mu.Unlock()
		return
	}
	c.err = err
	c.mock.removeEventLocked(c)
	c.innerCancel(cause)
	close(c.done)
	stop := c.stopParent
	afterFuncs := c.afterFuncs
	c.afterFuncs = nil
	children := c.children
	c.children = nil
	c.mock.mu.Unlock()
	// Without mock.mu: derived contexts call c.Err() while canceling. std goes first: children
	// read c through its view, which waits for std, and a stdlib layer between c and a child is
	// canceled through std.
	for _, a := range afterFuncs {
		a.f()
	}
	// A child whose parent is canceled is part of this cancellation. If another goroutine is
	// already canceling it, for example stdlib's AfterFunc goroutine for a layer that std just
	// canceled, it is pending: finished waits for it. Waiting here instead could deadlock, because
	// reading a child's parent can cancel an ancestor whose tree this goroutine is already running.
	// A child behind a layer that does not cancel with c, such as context.WithoutCancel, is not
	// part of it even when it is being canceled.
	var pending []*mockCtx
	for _, child := range children {
		// This goroutine calls Err and Value of the layers between c and child, here and through
		// Cause in child.cancelTree, so they must not block. Stdlib calls a parent's Err and Value
		// on the canceling goroutine only for a parent with its own AfterFunc method; it attaches a
		// child of a layer over a cancelCtx to that cancelCtx instead.
		if child.parent.Err() == nil {
			continue
		}
		child.cancelTree(context.Canceled, nil, stops)
		if child.canceling() {
			pending = append(pending, child)
		}
	}
	if len(pending) == 0 {
		close(c.finished)
	} else {
		go func() {
			for _, child := range pending {
				<-child.finished
			}
			close(c.finished)
		}()
	}
	if stop != nil {
		*stops = append(*stops, stop)
	}
}

// canceling reports whether c is canceled and its finished channel is still open.
func (c *mockCtx) canceling() bool {
	select {
	case <-c.finished:
		return false
	default:
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

type clockFunction int

const (
	clockFunctionNewTimer clockFunction = iota
	clockFunctionAfterFunc
	clockFunctionTimerStop
	clockFunctionTimerReset
	clockFunctionTickerFunc
	clockFunctionTickerFuncWait
	clockFunctionNewTicker
	clockFunctionTickerReset
	clockFunctionTickerStop
	clockFunctionNow
	clockFunctionSince
	clockFunctionUntil
	clockFunctionWithDeadline
	clockFunctionWithDeadlineCause
	clockFunctionWithTimeout
	clockFunctionWithTimeoutCause
)

func (c clockFunction) String() string {
	switch c {
	case clockFunctionNewTimer:
		return "NewTimer"
	case clockFunctionAfterFunc:
		return "AfterFunc"
	case clockFunctionTimerStop:
		return "Timer.Stop"
	case clockFunctionTimerReset:
		return "Timer.Reset"
	case clockFunctionTickerFunc:
		return "TickerFunc"
	case clockFunctionTickerFuncWait:
		return "TickerFunc.Wait"
	case clockFunctionNewTicker:
		return "NewTicker"
	case clockFunctionTickerReset:
		return "Ticker.Reset"
	case clockFunctionTickerStop:
		return "Ticker.Stop"
	case clockFunctionNow:
		return "Now"
	case clockFunctionSince:
		return "Since"
	case clockFunctionUntil:
		return "Until"
	case clockFunctionWithDeadline:
		return "WithDeadline"
	case clockFunctionWithDeadlineCause:
		return "WithDeadlineCause"
	case clockFunctionWithTimeout:
		return "WithTimeout"
	case clockFunctionWithTimeoutCause:
		return "WithTimeoutCause"
	default:
		return fmt.Sprintf("Unknown clockFunction(%d)", c)
	}
}

type callArg func(c *apiCall)

// apiCall represents a single call to one of the Clock APIs.
type apiCall struct {
	Time     time.Time
	Duration time.Duration
	Cause    error
	Tags     []string

	fn       clockFunction
	releases sync.WaitGroup
	complete chan struct{}
}

func (a *apiCall) String() string {
	switch a.fn {
	case clockFunctionNewTimer:
		return fmt.Sprintf("NewTimer(%s, %v)", a.Duration, a.Tags)
	case clockFunctionAfterFunc:
		return fmt.Sprintf("AfterFunc(%s, <fn>, %v)", a.Duration, a.Tags)
	case clockFunctionTimerStop:
		return fmt.Sprintf("Timer.Stop(%v)", a.Tags)
	case clockFunctionTimerReset:
		return fmt.Sprintf("Timer.Reset(%s, %v)", a.Duration, a.Tags)
	case clockFunctionTickerFunc:
		return fmt.Sprintf("TickerFunc(<ctx>, %s, <fn>, %s)", a.Duration, a.Tags)
	case clockFunctionTickerFuncWait:
		return fmt.Sprintf("TickerFunc.Wait(%v)", a.Tags)
	case clockFunctionNewTicker:
		return fmt.Sprintf("NewTicker(%s, %v)", a.Duration, a.Tags)
	case clockFunctionTickerReset:
		return fmt.Sprintf("Ticker.Reset(%s, %v)", a.Duration, a.Tags)
	case clockFunctionTickerStop:
		return fmt.Sprintf("Ticker.Stop(%v)", a.Tags)
	case clockFunctionNow:
		return fmt.Sprintf("Now(%v)", a.Tags)
	case clockFunctionSince:
		return fmt.Sprintf("Since(%s, %v)", a.Time, a.Tags)
	case clockFunctionUntil:
		return fmt.Sprintf("Until(%s, %v)", a.Time, a.Tags)
	case clockFunctionWithDeadline:
		return fmt.Sprintf("WithDeadline(<ctx>, %s, %v)", a.Time, a.Tags)
	case clockFunctionWithDeadlineCause:
		return fmt.Sprintf("WithDeadlineCause(<ctx>, %s, cause=%v, %v)", a.Time, a.Cause, a.Tags)
	case clockFunctionWithTimeout:
		return fmt.Sprintf("WithTimeout(<ctx>, %s, %v)", a.Duration, a.Tags)
	case clockFunctionWithTimeoutCause:
		return fmt.Sprintf("WithTimeoutCause(<ctx>, %s, cause=%v, %v)", a.Duration, a.Cause, a.Tags)
	default:
		return fmt.Sprintf("Unknown clockFunction(%d)", a.fn)
	}
}

// Call represents an apiCall that has been trapped.
type Call struct {
	Time     time.Time
	Duration time.Duration
	Cause    error // Cause passed to a cause variant, if applicable.
	Tags     []string

	tb      TestingT
	apiCall *apiCall
	trap    *Trap
}

// Release the call and wait for it to complete. If the provided context expires before the call completes, it returns
// an error.
//
// IMPORTANT: If a call is trapped by more than one trap, they all must release the call before it can complete, and
// they must do so from different goroutines.
func (c *Call) Release(ctx context.Context) error {
	c.apiCall.releases.Done()
	select {
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for release; did more than one trap capture the call?: %w", ctx.Err())
	case <-c.apiCall.complete:
		// OK
	}
	c.trap.callReleased()
	return nil
}

// MustRelease releases the call and waits for it to complete. If the provided context expires before the call
// completes, it fails the test.
//
// IMPORTANT: If a call is trapped by more than one trap, they all must release the call before it can complete, and
// they must do so from different goroutines.
func (c *Call) MustRelease(ctx context.Context) {
	if err := c.Release(ctx); err != nil {
		c.tb.Helper()
		c.tb.Fatal(err.Error())
	}
}

func withTime(t time.Time) callArg {
	return func(c *apiCall) {
		c.Time = t
	}
}

func withDuration(d time.Duration) callArg {
	return func(c *apiCall) {
		c.Duration = d
	}
}

func withCause(err error) callArg {
	return func(c *apiCall) {
		c.Cause = err
	}
}

func newCall(fn clockFunction, tags []string, args ...callArg) *apiCall {
	c := &apiCall{
		fn:       fn,
		Tags:     tags,
		complete: make(chan struct{}),
	}
	for _, a := range args {
		a(c)
	}
	return c
}

type Trap struct {
	fn    clockFunction
	tags  []string
	mock  *Mock
	calls chan *apiCall
	done  chan struct{}

	// mu protects the unreleasedCalls count
	mu              sync.Mutex
	unreleasedCalls int
}

func (t *Trap) String() string {
	return fmt.Sprintf("Trap %s(..., %v)", t.fn.String(), t.tags)
}

func (t *Trap) catch(c *apiCall) {
	select {
	case t.calls <- c:
	case <-t.done:
		c.releases.Done()
	}
}

func (t *Trap) matches(c *apiCall) bool {
	if t.fn != c.fn {
		return false
	}
	for _, tag := range t.tags {
		if !slices.Contains(c.Tags, tag) {
			return false
		}
	}
	return true
}

func (t *Trap) Close() {
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	select {
	case <-t.done:
		t.mock.tb.Logf("%s already Closed()", t)
		return // already closed
	default:
	}
	if t.unreleasedCalls != 0 {
		t.mock.tb.Helper()
		t.mock.tb.Errorf("%s Closed() with %d unreleased calls", t, t.unreleasedCalls)
	}
	for i, tr := range t.mock.traps {
		if t == tr {
			t.mock.traps = append(t.mock.traps[:i], t.mock.traps[i+1:]...)
		}
	}
	close(t.done)
}

func (t *Trap) callReleased() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unreleasedCalls--
}

var ErrTrapClosed = errors.New("trap closed")

func (t *Trap) Wait(ctx context.Context) (*Call, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, ErrTrapClosed
	case a := <-t.calls:
		c := &Call{
			Time:     a.Time,
			Duration: a.Duration,
			Cause:    a.Cause,
			Tags:     a.Tags,
			apiCall:  a,
			trap:     t,
			tb:       t.mock.tb,
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.unreleasedCalls++
		return c, nil
	}
}

// MustWait calls Wait() and then if there is an error, immediately fails the
// test via tb.Fatalf()
func (t *Trap) MustWait(ctx context.Context) *Call {
	t.mock.tb.Helper()
	c, err := t.Wait(ctx)
	if err != nil {
		t.mock.tb.Fatalf("context expired while waiting for %s: %s", t, err.Error())
	}
	return c
}

type Logger interface {
	Log(args ...any)
	Logf(format string, args ...any)
}

// NoOpLogger is a Logger that discards all log messages.
var NoOpLogger Logger = noOpLogger{}

type noOpLogger struct{}

func (noOpLogger) Log(args ...any)                 {}
func (noOpLogger) Logf(format string, args ...any) {}
