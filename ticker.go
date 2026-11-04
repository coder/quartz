package quartz

import "time"

// A Ticker holds a channel that delivers “ticks” of a clock at intervals.
type Ticker struct {
	C <-chan time.Time
	//nolint: revive
	c             chan time.Time
	ticker        *time.Ticker   // realtime impl, if set
	d             time.Duration  // period, if set
	nxt           time.Time      // next tick time
	mock          *Mock          // mock clock, if set
	stopped       bool           // true if the ticker is not running
	internalTicks chan time.Time // used to deliver ticks to the runLoop goroutine
	// resets is how Reset tells runLoop to drop a tick from the old schedule.
	resets chan struct{}

	// As of Go 1.23, ticker channels are unbuffered and guaranteed to block forever after a call to
	// stop.
	//
	// When a mocked ticker fires, we don't want to block on a channel write, because it's fine for
	// the code under test not to be reading. That means we need to start a new goroutine to do the
	// channel write (runLoop) if we are a channel-based ticker.
	//
	// They also are not supposed to leak even if they are never read or stopped (Go runtime can
	// garbage collect them). We can't garbage-collect because we can't check if any other code
	// besides the mock references, but we can ensure that we don't leak goroutines so that the
	// garbage collector can do its job when the mock is no longer referenced. interrupt lets Stop
	// and the cleanup registered by NewMock end the runLoop goroutine.
	interrupt chan struct{}
}

func (t *Ticker) fire(tt time.Time) {
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	// A fire that gets the lock after Stop or Reset belongs to the schedule they replaced: Stop
	// stops the ticker, and Reset moves nxt. This happens when Stop or Reset gets the lock before
	// a fire that Advance started.
	if t.stopped || !t.nxt.Equal(tt) {
		return
	}
	for !t.nxt.After(t.mock.cur) {
		t.nxt = t.nxt.Add(t.d)
	}
	t.mock.recomputeNextLocked()
	if t.interrupt != nil { // implies runLoop is still going.
		t.internalTicks <- tt
		return
	}
	// NewMock's cleanup has stopped runLoop, so only a reader that is already waiting gets the
	// tick.
	select {
	case t.c <- tt:
	default:
	}
}

func (t *Ticker) next() time.Time {
	return t.nxt
}

// Stop turns off a ticker. After Stop, no more ticks will be sent. Stop does
// not close the channel, to prevent a concurrent goroutine reading from the
// channel from seeing an erroneous "tick".
func (t *Ticker) Stop(tags ...string) {
	if t.ticker != nil {
		t.ticker.Stop()
		return
	}
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	c := newCall(clockFunctionTickerStop, tags)
	t.mock.matchCallLocked(c)
	defer close(c.complete)
	t.mock.removeEventLocked(t)
	t.stopped = true
	t.stopRunLoopLocked()
}

// Reset stops a ticker and resets its period to the specified duration. The
// next tick will arrive after the new period elapses. The duration d must be
// greater than zero; if not, Reset will panic.
func (t *Ticker) Reset(d time.Duration, tags ...string) {
	if t.ticker != nil {
		t.ticker.Reset(d)
		return
	}
	if d <= 0 {
		panic("Ticker.Reset called with negative or zero duration")
	}
	t.mock.mu.Lock()
	defer t.mock.mu.Unlock()
	c := newCall(clockFunctionTickerReset, tags, withDuration(d))
	t.mock.matchCallLocked(c)
	defer close(c.complete)
	t.nxt = t.mock.cur.Add(d)
	t.d = d
	if t.stopped {
		t.stopped = false
		t.mock.addEventLocked(t)
	} else {
		t.mock.recomputeNextLocked()
	}
	if t.interrupt == nil {
		t.startRunLoopLocked()
		return
	}
	// Tell runLoop to drop a tick it holds for a reader, since that tick belongs to the old
	// schedule. Starting a new runLoop instead would put a goroutine in the caller's synctest
	// bubble that waits on channels created outside it, and synctest.Test would not return. The
	// branch above still starts one for a stopped ticker, so a Reset inside a bubble of a stopped
	// ticker created outside it hangs.
	t.resets <- struct{}{}
}

func (t *Ticker) runLoop(interrupt chan struct{}) {
	defer close(interrupt)
outer:
	for {
		select {
		case tt := <-t.internalTicks:
			// select picks at random among ready cases. Send first to a reader that is already
			// waiting, so it gets the tick even when Stop or NewMock's cleanup is waiting on
			// interrupt.
			select {
			case t.c <- tt:
				continue outer
			default:
			}
			for {
				select {
				case t.c <- tt:
					continue outer
				case <-t.internalTicks:
					// Discard future ticks until we can send this one.
				case <-t.resets:
					continue outer
				case interrupt <- struct{}{}:
					return
				}
			}
		case <-t.resets:
		case interrupt <- struct{}{}:
			return
		}
	}
}

func (t *Ticker) startRunLoopLocked() {
	// assert some assumptions. If these fire, it is a bug in Quartz itself.
	if t.interrupt != nil {
		t.mock.tb.Error("called startRunLoopLocked when interrupt suggests we are already running")
	}
	// NewMock's cleanup has run and would not stop the runLoop, so fire sends to C itself.
	if t.mock.testOver {
		return
	}
	interrupt := make(chan struct{})
	t.interrupt = interrupt
	t.mock.tickers[t] = struct{}{}
	go t.runLoop(interrupt)
}

// stopRunLoopLocked ends the runLoop goroutine, if it is running.
func (t *Ticker) stopRunLoopLocked() {
	if t.interrupt == nil {
		return
	}
	<-t.interrupt
	t.interrupt = nil
	delete(t.mock.tickers, t)
}

func newMockTickerLocked(m *Mock, d time.Duration) *Ticker {
	// no buffer follows Go 1.23+ behavior
	ticks := make(chan time.Time)
	t := &Ticker{
		C:             ticks,
		c:             ticks,
		d:             d,
		nxt:           m.cur.Add(d),
		mock:          m,
		internalTicks: make(chan time.Time),
		resets:        make(chan struct{}),
	}
	m.addEventLocked(t)
	t.startRunLoopLocked()
	return t
}
