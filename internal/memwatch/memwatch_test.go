package memwatch

// The marks a run is judged by. Each test here is about a wrong number being
// worse than no number: a figure printed on a progress line is read as a
// measurement, and a run that is killed for memory is diagnosed from the last
// one drawn.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// status writes a fixture and gives back its path.
func status(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// TestTheMarksComeOutOfProcStatus covers the arithmetic and, above all, the
// refusals. A line this misread would put a fabricated size on every meter.
func TestTheMarksComeOutOfProcStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   Marks
		ok     bool
	}{
		{
			name:   "a run holding 78 GiB, 12 GiB of it its own",
			status: "VmHWM:\t81788928 kB\nRssAnon:\t12897484 kB\nVmSwap:\t0 kB\n",
			want:   Marks{RSS: 81788928 * 1024, Anon: 12897484 * 1024},
			ok:     true,
		},
		{
			name:   "swapping",
			status: "VmHWM:\t100 kB\nRssAnon:\t80 kB\nVmSwap:\t40 kB\n",
			want:   Marks{RSS: 100 * 1024, Anon: 80 * 1024, Swap: 40 * 1024},
			ok:     true,
		},
		{
			name:   "the lines this does not want are passed over",
			status: "Name:\tgit-fixed\nThreads:\t8\nVmPeak:\t99 kB\nVmHWM:\t64 kB\n",
			want:   Marks{RSS: 64 * 1024},
			ok:     true,
		},
		{name: "no VmHWM at all", status: "VmRSS:\t64 kB\nVmSwap:\t0 kB\n"},
		{name: "a unit this does not know", status: "VmHWM:\t64 MB\n"},
		{name: "no unit", status: "VmHWM:\t65759416\n"},
		{name: "not a number", status: "VmHWM:\tlots kB\n"},
		{name: "a swap line this cannot read refuses the whole read",
			status: "VmHWM:\t64 kB\nVmSwap:\tsome kB\n"},
		{name: "empty", status: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseStatus(tc.status)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheMarksReadThisMachine is the half a fixture cannot prove: that the file
// being parsed is the shape /proc/self/status actually has, and that the
// process this runs in has a resident set to report.
func TestTheMarksReadThisMachine(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc/self/status on this system, which is where the marks are skipped too")
	}
	marks, ok := Peak()
	require.True(t, ok, "this machine's /proc/self/status names no VmHWM this understands")
	assert.Positive(t, marks.RSS, "a running process holds some memory")
	assert.LessOrEqual(t, marks.Anon, marks.RSS, "the anonymous set is part of the resident set")
	t.Logf("this test process: %s", marks)
}

// TestAMarkOnlyEverGoesUp is the whole meaning of the word. The resident mark
// is the kernel's own and rises by itself; the other two are sampled, so a
// sample taken after the worst moment has passed must not lower them.
func TestAMarkOnlyEverGoesUp(t *testing.T) {
	now := time.Now()
	w := &watcher{
		path: status(t, "VmHWM:\t100 kB\nRssAnon:\t90 kB\nVmSwap:\t50 kB\n"),
		now:  func() time.Time { return now },
	}
	first, ok := w.peak()
	require.True(t, ok)
	require.Equal(t, uint64(50*1024), first.Swap)

	// The same process, later, with the pressure over.
	w.path = status(t, "VmHWM:\t100 kB\nRssAnon:\t8 kB\nVmSwap:\t0 kB\n")
	now = now.Add(refresh)
	got, ok := w.peak()
	require.True(t, ok)
	assert.Equal(t, first, got, "a mark is the worst moment, not the latest one")
}

// TestAReadThatFailsLeavesTheMarksAlone keeps the last honest figure on screen.
// A run whose /proc entry has gone is a run in trouble, and zeroes there would
// read as a run that used nothing.
func TestAReadThatFailsLeavesTheMarksAlone(t *testing.T) {
	now := time.Now()
	w := &watcher{
		path: status(t, "VmHWM:\t2048 kB\nRssAnon:\t1024 kB\nVmSwap:\t0 kB\n"),
		now:  func() time.Time { return now },
	}
	want, ok := w.peak()
	require.True(t, ok)

	w.path = filepath.Join(t.TempDir(), "gone")
	now = now.Add(refresh)
	got, ok := w.peak()
	assert.True(t, ok, "a mark already taken is still a mark")
	assert.Equal(t, want, got)
}

// TestNothingToReportBeforeAnythingIsRead is the answer on a system that
// publishes no marks: no figure, rather than a made-up one.
func TestNothingToReportBeforeAnythingIsRead(t *testing.T) {
	w := &watcher{path: filepath.Join(t.TempDir(), "absent"), now: time.Now}
	got, ok := w.peak()
	assert.False(t, ok)
	assert.Equal(t, Marks{}, got)
}

// TestTheFileIsNotReadOnEveryDraw holds the cost of a mark down. Step is called
// once per object and draws several times a second, and a file read on each one
// would be paid for by the phase it is reporting on.
func TestTheFileIsNotReadOnEveryDraw(t *testing.T) {
	now := time.Now()
	w := &watcher{
		path: status(t, "VmHWM:\t64 kB\nRssAnon:\t32 kB\nVmSwap:\t0 kB\n"),
		now:  func() time.Time { return now },
	}
	first, ok := w.peak()
	require.True(t, ok)

	// A file that says something else, read again inside the interval.
	w.path = status(t, "VmHWM:\t4096 kB\nRssAnon:\t2048 kB\nVmSwap:\t0 kB\n")
	now = now.Add(refresh - time.Millisecond)
	again, _ := w.peak()
	assert.Equal(t, first, again, "the mark inside the interval is the cached one")

	now = now.Add(time.Millisecond)
	moved, _ := w.peak()
	assert.Equal(t, uint64(4096*1024), moved.RSS, "and the interval is over")
}

// TestASizeIsRenderedTheWayGitRendersOne keeps the figures reading like the
// ones git prints beside them.
func TestASizeIsRenderedTheWayGitRendersOne(t *testing.T) {
	for _, tc := range []struct {
		n    uint64
		want string
	}{
		{0, "0 bytes"},
		{1023, "1023 bytes"},
		{1024, "1.00 KiB"},
		{12 * 1024, "12.00 KiB"},
		{1 << 20, "1.00 MiB"},
		{1<<20 + 1<<19, "1.50 MiB"},
		{1 << 30, "1.00 GiB"},
		{81788928 * 1024, "78.00 GiB"},
		{3 << 40, "3.00 TiB"},
	} {
		assert.Equal(t, tc.want, Bytes(tc.n))
	}
}

// TestTheMarksReadAsASentence covers both renderings, and the one thing they
// must never do: claim a run swapped when it did not.
func TestTheMarksReadAsASentence(t *testing.T) {
	quiet := Marks{RSS: 81788928 * 1024, Anon: 12897484 * 1024}
	assert.Equal(t, "78.00 GiB", quiet.Short())
	assert.Equal(t, "78.00 GiB resident (12.30 GiB of it this process's own), nothing swapped",
		quiet.String())

	swapping := Marks{RSS: 2 << 30, Anon: 1 << 30, Swap: 512 << 20}
	assert.Equal(t, "2.00 GiB +512.00 MiB swap", swapping.Short())
	assert.Contains(t, swapping.String(), "512.00 MiB swapped")
}
