package fsckcmd

// The progress meter, drawn the way git's progress.c draws it.
//
// A run over a large repository spends minutes inside one phase, and until this
// existed it printed nothing at all for the whole time. git shows a meter on
// five phases and this shows it on the same five, with the same titles, the
// same delays and the same wording, because --dry-run stands in for git fsck.
//
// see docs/progress.md

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// progressTick is how often the meter is allowed to redraw. git arms a one
// second SIGALRM and draws on the next call after it fires.
const progressTick = time.Second

// progressDelay is how long a delayed meter stays quiet, as git's
// GIT_PROGRESS_DELAY does. A phase that finishes inside it prints nothing.
const progressDelay = time.Second

// meter draws one phase's progress: a title, then a count that is rewritten in
// place, then a "done." line when the phase ends.
//
// Every method works on a nil meter and draws nothing, so a caller never asks
// whether progress is on.
type meter struct {
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

	mu      sync.Mutex
	shown   bool
	lastLen int

	stop chan struct{}
	wg   sync.WaitGroup
}

// meterOn builds a meter that draws immediately, as git's start_progress does.
// total is zero for a phase that counts without knowing where it ends.
func (r *run) meterOn(title string, total int64) *meter {
	return r.newMeter(title, total, false)
}

// meterDelayed builds a meter that stays quiet for a second first, as git's
// start_delayed_progress does. It is for a phase that is usually instant.
func (r *run) meterDelayed(title string, total int64) *meter {
	return r.newMeter(title, total, true)
}

func (r *run) newMeter(title string, total int64, delayed bool) *meter {
	if !r.o.ShowProgress {
		return nil
	}
	m := &meter{w: r.o.Stderr, title: title, total: total, stop: make(chan struct{})}
	m.pct.Store(-1)
	m.quiet.Store(delayed)
	m.wg.Add(1)
	go m.run(delayed)
	return m
}

// run raises the redraw flag on a timer. The meter draws from whichever worker
// notices the flag, so nothing here writes.
func (m *meter) run(delayed bool) {
	defer m.wg.Done()
	if delayed {
		select {
		case <-m.stop:
			return
		case <-time.After(progressDelay):
			m.quiet.Store(false)
		}
	}
	t := time.NewTicker(progressTick)
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

// step counts one unit of work. Workers call it once per object, so the path
// that draws nothing must stay to a couple of atomics.
func (m *meter) step() {
	if m == nil {
		return
	}
	m.report(m.count.Add(1))
}

// advance moves the count to n, unless it is already past it. Work that
// finishes out of order still leaves a meter that only ever goes forwards.
func (m *meter) advance(n int64) {
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
func (m *meter) report(n int64) {
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
func (m *meter) draw(n int64, end string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var counters string
	if m.total > 0 {
		p := n * 100 / m.total
		m.pct.Store(int32(p))
		counters = fmt.Sprintf("%3d%% (%d/%d)", p, n, m.total)
	} else {
		counters = fmt.Sprintf("%d", n)
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

// finish stops the meter and leaves the last count on screen. A meter that
// never drew anything, because its phase beat its delay, prints nothing at all.
func (m *meter) finish() {
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
