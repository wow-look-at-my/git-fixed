package progress

// The meter's three promises: it only ever goes forwards, it never passes a hundred percent.

import (
	"bytes"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drawn is the count out of every line a meter wrote.
var drawn = regexp.MustCompile(`\((\d+)/\d+\)`)

// safeBuffer collects what several workers draw.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAMeterOnlyEverGoesForwards is the promise every phase here relies on: the
// work is done on every core and finishes out of order, and a count that walks
// backwards over work already done reads as a run losing ground.
//
// Two workers that step the counter can reach the lock in the other order. What
// keeps the line honest is that neither draws the number it was holding.
func TestAMeterOnlyEverGoesForwards(t *testing.T) {
	const (
		workers = 8
		each    = 40
		total   = workers * each
	)
	var out safeBuffer
	m := Start(&out, "Checking objects", total)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				m.Step()
			}
		}()
	}
	wg.Wait()
	m.Finish()

	var last int64
	var lines int
	for _, match := range drawn.FindAllStringSubmatch(out.String(), -1) {
		n, err := strconv.ParseInt(match[1], 10, 64)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, last, "the meter went backwards, from %d to %d", last, n)
		last = n
		lines++
	}
	assert.Positive(t, lines, "a meter over %d steps draws something", total)
	assert.Equal(t, int64(total), last, "the last line drawn is the finished count")
}

// TestAdvanceNeverPullsTheCountBack covers the other half, where the work
// arrives with its own number: the loose-object phase advances one meter per
// fanout directory from every core at once.
func TestAdvanceNeverPullsTheCountBack(t *testing.T) {
	var out safeBuffer
	m := Start(&out, "Checking object directories", 256)
	m.Advance(200)
	m.Advance(100)
	m.Finish()
	assert.Contains(t, out.String(), "(200/256)", "a lower count is not a step backwards, it is nothing")
}

// TestEveryLineSaysHowLongThePhaseHasRun keeps the clock in the widest unit
// that has a whole number in it. Three digits of seconds, or a leading zero on
// everything short, is what a fixed unit gives instead.
func TestEveryLineSaysHowLongThePhaseHasRun(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m00s"},
		{2*time.Minute + 47*time.Second, "2m47s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h00m"},
		{25*time.Hour + 30*time.Minute, "25h30m"},
	} {
		assert.Equal(t, tc.want, elapsed(tc.d))
	}
}

// TestTheStatusFieldCarriesTheClockAndTheMark is the shape of what follows the
// counters, which is this tool's own and not git's.
func TestTheStatusFieldCarriesTheClockAndTheMark(t *testing.T) {
	var out safeBuffer
	m := Start(&out, "Checking objects", 10)
	m.Advance(10)
	m.Finish()
	assert.Regexp(t, `Checking objects: 100% \(10/10\) \[\d+s(, peak [\d.]+ (bytes|KiB|MiB|GiB|TiB))?\], done\.`,
		out.String())
}

// TestANilMeterDrawsNothing is what lets a phase count without asking whether
// anybody wanted progress.
func TestANilMeterDrawsNothing(t *testing.T) {
	var m *Meter
	assert.NotPanics(t, func() {
		m.Step()
		m.Advance(5)
		m.Finish()
	})
}

// TestAMeterThatBeatItsDelayPrintsNothing is git's own last_value guard: a
// phase that finished inside the delay leaves no line behind at all.
func TestAMeterThatBeatItsDelayPrintsNothing(t *testing.T) {
	var out safeBuffer
	m := StartDelayed(&out, "Checking connectivity", 0)
	m.Step()
	m.Finish()
	assert.Empty(t, out.String())
}

// percent is the percentage out of every line a meter wrote.
var percent = regexp.MustCompile(`(\d+)% \((\d+)/(\d+)\)`)

// TestAMeterNeverPassesAHundredPercent is the third promise, and it is about a
// total that was wrong.
//
// The repair walk counts against the objects the repository holds and then
// reaches one it does not hold, which is the entire reason that walk runs. The
// arithmetic then says 150%, which reports on the meter and not on the run.
func TestAMeterNeverPassesAHundredPercent(t *testing.T) {
	var buf safeBuffer
	m := Start(&buf, "Checking", 2)
	for range 5 {
		m.Step()
	}
	m.Finish()

	lines := percent.FindAllStringSubmatch(buf.String(), -1)
	require.NotEmpty(t, lines, "the meter drew nothing")
	for _, l := range lines {
		p, err := strconv.Atoi(l[1])
		require.NoError(t, err)
		count, err := strconv.Atoi(l[2])
		require.NoError(t, err)
		total, err := strconv.Atoi(l[3])
		require.NoError(t, err)
		assert.LessOrEqual(t, p, 100, "%q", l[0])
		assert.LessOrEqual(t, count, total, "the count passed the total it was drawn against: %q", l[0])
	}
	last := lines[len(lines)-1]
	assert.Equal(t, "5", last[2], "the count is what it counted")
	assert.Equal(t, "5", last[3], "the total moved up to meet it")
}

// TestATotalThatWasRightIsLeftAlone keeps the correction from touching the
// ordinary case, where the caller knew what it was counting.
func TestATotalThatWasRightIsLeftAlone(t *testing.T) {
	var buf safeBuffer
	m := Start(&buf, "Checking", 10)
	for range 4 {
		m.Step()
	}
	m.Finish()

	lines := percent.FindAllStringSubmatch(buf.String(), -1)
	require.NotEmpty(t, lines)
	last := lines[len(lines)-1]
	assert.Equal(t, "40", last[1])
	assert.Equal(t, "10", last[3], "a total the count never reached must not move")
}
