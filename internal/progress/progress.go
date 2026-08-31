// Package progress draws a phase's progress the way git's progress.c draws it.
//
// A run over a large repository spends minutes inside a single phase, and
// until this existed it printed nothing at all for the whole time. git draws
// a meter on several phases of its fsck, and this draws a meter on the same
// phases, with the same titles, the same delays and the same wording,
// because --dry-run stands in for git fsck. The repair scan that follows
// draws a meter too, for the same reason and with nothing to copy.
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

	"github.com/wow-look-at-my/git-fixed/internal/memwatch"
)

// tick is how often a meter is allowed to redraw.
const tick = time.Second

// delay is how long a delayed meter stays quiet, as git's GIT_PROGRESS_DELAY does.
const delay = time.Second

// Meter draws a phase's progress: a title, then a count that is rewritten in
// place, then a "done." line when the phase ends.
//
// Every method works on a nil Meter and draws nothing, so a caller with
// progress turned off keeps a nil Meter rather than a condition at every
// place it counts.
type Meter struct {
	w     io.Writer
	title string
	// total is what the caller believed the count would reach. see raise.
	total atomic.Int64

	count atomic.Int64
	// pct is the percentage last drawn, so a step that does not move it costs a single atomic load and no lock.
	pct atomic.Int32
	// quiet holds a delayed meter back until its delay is up.
	quiet atomic.Bool
	// due is raised by the ticker, standing in for git's SIGALRM.
	due atomic.Bool

	// start is when the phase began, which is what every line reports from.
	start time.Time

	mu      sync.Mutex
	shown   bool
	lastLen int

	stop chan struct{}
	wg   sync.WaitGroup
}

// Start begins a meter that draws immediately, as git's start_progress does.
func Start(w io.Writer, title string, total int64) *Meter {
	return start(w, title, total, false)
}

// StartDelayed begins a meter that stays quiet until the delay interval passes, as git's start_delayed_progress does.
func StartDelayed(w io.Writer, title string, total int64) *Meter {
	return start(w, title, total, true)
}

func start(w io.Writer, title string, total int64, delayed bool) *Meter {
	m := &Meter{w: w, title: title, start: time.Now(), stop: make(chan struct{})}
	m.total.Store(total)
	m.pct.Store(-1)
	m.quiet.Store(delayed)
	// Raised here and not in run: a phase that ends before that goroutine is scheduled drew nothing at all.
	m.due.Store(!delayed)
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

// Step counts a unit of work. Workers call it for each object, so the path
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

// raise moves the total up to meet a count that has passed it, and returns the total to measure against.
func (m *Meter) raise(n int64) int64 {
	for {
		total := m.total.Load()
		if n <= total || total == 0 {
			return total
		}
		if m.total.CompareAndSwap(total, n) {
			return n
		}
	}
}

// report draws when the count has moved the percentage or the timer has come
// round, which is what git's display() decides.
func (m *Meter) report(n int64) {
	if m.quiet.Load() {
		return
	}
	if total := m.raise(n); total > 0 {
		p := int32(n * 100 / total)
		if m.pct.Load() == p && !m.due.Load() {
			return
		}
	} else if !m.due.Load() {
		// Without a total there is no percentage to move, so the timer is the only thing that draws.
		return
	}
	m.due.Store(false)
	m.draw("")
}

// draw writes a line. end is empty for an update, which returns the cursor to
// the start of the line, and ", done." for the last update.
//
// The count it draws is whatever the counter holds now, not whatever the
// caller was holding when it decided to draw. These differ under workers:
// several that step the counter can reach the lock in a different order, and
// each drawing its own number sends the meter backwards over work that was
// already done.
func (m *Meter) draw(end string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.count.Load()
	var counters string
	if total := m.raise(n); total > 0 {
		p := n * 100 / total
		m.pct.Store(int32(p))
		counters = fmt.Sprintf("%3d%% (%d/%d) %s", p, n, total, m.status())
	} else {
		counters = fmt.Sprintf("%d %s", n, m.status())
	}
	// A line shorter than the previous line leaves the tail of that line on screen.
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
	m.draw(", done.")
}

// status is the bracketed field after the count: how long this phase has been running.
func (m *Meter) status() string {
	s := elapsed(time.Since(m.start))
	if marks, ok := memwatch.Peak(); ok {
		s += ", peak " + marks.Short()
	}
	return "[" + s + "]"
}

// elapsed renders how long a phase has been running, in the widest unit that
// carries a nonzero span, and pads the smaller field on the left so a short
// value still shows a full width.
func elapsed(d time.Duration) string {
	switch s := int64(d.Seconds()); {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
}
