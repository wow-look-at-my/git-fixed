// Package progress draws a phase's progress the way git's progress.c draws it.
//
// A run over a large repository spends minutes inside one phase, and until this
// existed it printed nothing at all for the whole time. git shows a meter on
// five phases of its fsck and this shows one on the same five, with the same
// titles, the same delays and the same wording, because --dry-run stands in for
// git fsck. The repair scan that follows gets one too, for the same reason and
// with nothing to copy.
//
// see docs/progress.md
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tick is how often a meter is allowed to redraw. git arms a one second
// SIGALRM and draws on the next call after it fires.
const tick = time.Second

// delay is how long a delayed meter stays quiet, as git's GIT_PROGRESS_DELAY
// does. A phase that finishes inside it prints nothing at all.
const delay = time.Second

// Meter draws one phase's progress: a title, then a count that is rewritten in
// place, then a "done." line when the phase ends.
//
// Every method works on a nil Meter and draws nothing, so a caller with
// progress turned off keeps one nil Meter rather than a condition at every
// place it counts.
type Meter struct {
	w     io.Writer
	title string
	total int64

	count atomic.Int64
	// pct is the percentage last drawn, so a step that does not move it
	// costs one atomic load and no lock.
	pct atomic.Int32
	// quiet holds a delayed meter back until its delay is up.
	quiet atomic.Bool
	// due is raised by the ticker, standing in for git's SIGALRM.
	due atomic.Bool

	// start is when the phase began, so every line says how long it has been
	// running. git prints no time at all, which leaves a person watching a
	// number climb with no idea whether it is minutes or hours from done.
	start time.Time

	mu      sync.Mutex
	shown   bool
	lastLen int

	stop chan struct{}
	wg   sync.WaitGroup
}

// Start begins a meter that draws immediately, as git's start_progress does.
// total is zero for a phase that counts without knowing where it ends.
func Start(w io.Writer, title string, total int64) *Meter {
	return start(w, title, total, false)
}

// StartDelayed begins a meter that stays quiet for a second first, as git's
// start_delayed_progress does. It is for a phase that is usually instant.
func StartDelayed(w io.Writer, title string, total int64) *Meter {
	return start(w, title, total, true)
}

func start(w io.Writer, title string, total int64, delayed bool) *Meter {
	m := &Meter{w: w, title: title, total: total, start: time.Now(), stop: make(chan struct{})}
	m.pct.Store(-1)
	m.quiet.Store(delayed)
	m.wg.Add(1)
	go m.run(delayed)
	return m
}

// run raises the redraw flag on a timer. The meter draws from whichever worker
// notices the flag, so nothing here writes.
func (m *Meter) run(delayed bool) {
	defer m.wg.Done()
	if delayed {
		select {
		case <-m.stop:
			return
		case <-time.After(delay):
			m.quiet.Store(false)
		}
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	m.due.Store(true)
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.due.Store(true)
		}
	}
}

// Step counts one unit of work. Workers call it once per object, so the path
// that draws nothing must stay to a couple of atomics.
func (m *Meter) Step() {
	if m == nil {
		return
	}
	m.report(m.count.Add(1))
}

// Advance moves the count to n, unless it is already past it. Work that
// finishes out of order still leaves a meter that only ever goes forwards.
func (m *Meter) Advance(n int64) {
	if m == nil {
		return
	}
	for {
		cur := m.count.Load()
		if cur >= n {
			m.report(cur)
			return
		}
		if m.count.CompareAndSwap(cur, n) {
			m.report(n)
			return
		}
	}
}

// report draws when the count has moved the percentage or the timer has come
// round, which is what git's display() decides.
func (m *Meter) report(n int64) {
	if m.quiet.Load() {
		return
	}
	if m.total > 0 {
		p := int32(n * 100 / m.total)
		if m.pct.Load() == p && !m.due.Load() {
			return
		}
	} else if !m.due.Load() {
		// Without a total there is no percentage to move, so the timer is
		// the only thing that draws.
		return
	}
	m.due.Store(false)
	m.draw(n, "")
}

// draw writes one line. end is empty for an update, which returns the cursor to
// the start of the line, and ", done." for the last one.
func (m *Meter) draw(n int64, end string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var counters string
	if m.total > 0 {
		p := n * 100 / m.total
		m.pct.Store(int32(p))
		counters = fmt.Sprintf("%3d%% (%d/%d) %s", p, n, m.total, elapsed(time.Since(m.start)))
	} else {
		counters = fmt.Sprintf("%d %s", n, elapsed(time.Since(m.start)))
	}
	// A shorter line than the last one leaves the tail of the last one on
	// screen, so it is painted over with spaces.
	pad := ""
	if len(counters) < m.lastLen {
		pad = strings.Repeat(" ", m.lastLen-len(counters)+1)
	}
	m.lastLen = len(counters)
	tail := "\r"
	if end != "" {
		tail = end + "\n"
	}
	fmt.Fprintf(m.w, "%s: %s%s%s", m.title, counters, pad, tail)
	m.shown = true
}

// Finish stops the meter and leaves the last count on screen. A meter that
// never drew anything, because its phase beat its delay, prints nothing at all.
func (m *Meter) Finish() {
	if m == nil {
		return
	}
	close(m.stop)
	m.wg.Wait()
	m.mu.Lock()
	shown := m.shown
	m.mu.Unlock()
	if !shown {
		return
	}
	m.draw(m.count.Load(), ", done.")
}

// elapsed renders how long a phase has been running, in the widest unit that
// has a whole number in it. A fixed unit is either three digits of seconds or
// a leading zero on everything short.
func elapsed(d time.Duration) string {
	switch s := int64(d.Seconds()); {
	case s < 60:
		return fmt.Sprintf("[%ds]", s)
	case s < 3600:
		return fmt.Sprintf("[%dm%02ds]", s/60, s%60)
	default:
		return fmt.Sprintf("[%dh%02dm]", s/3600, s%3600/60)
	}
}
