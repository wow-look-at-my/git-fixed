package main

import (
	"os"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheHeapCeilingComesFromTheMachine covers the arithmetic and, above all,
// the refusals: a ceiling computed from a line this misread would be a limit
// nobody chose, on a run that is already in trouble.
func TestTheHeapCeilingComesFromTheMachine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		meminfo string
		want    int64
	}{
		{
			name:    "a machine with 64 GiB",
			meminfo: "MemTotal:       65759416 kB\nMemFree:         1234 kB\n",
			want:    65759416 / 4 * 3 * 1024,
		},
		{
			name:    "MemTotal is not the first line",
			meminfo: "SwapTotal:      100 kB\nMemTotal:       1024 kB\n",
			want:    1024 / 4 * 3 * 1024,
		},
		{name: "no MemTotal at all", meminfo: "MemFree: 100 kB\n"},
		{name: "a unit this does not know", meminfo: "MemTotal:       64 GB\n"},
		{name: "no unit", meminfo: "MemTotal:       65759416\n"},
		{name: "not a number", meminfo: "MemTotal:       lots kB\n"},
		{name: "zero", meminfo: "MemTotal:       0 kB\n"},
		{name: "empty", meminfo: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := heapLimit(tc.meminfo)
			assert.Equal(t, tc.want != 0, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheHeapCeilingReadsThisMachine is the half a fixture cannot prove: that
// the file being parsed is the shape /proc/meminfo actually has.
func TestTheHeapCeilingReadsThisMachine(t *testing.T) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Skip("no /proc/meminfo on this system, which is where the ceiling is skipped too")
	}
	limit, ok := heapLimit(string(data))
	require.True(t, ok, "this machine's /proc/meminfo names no MemTotal this understands")
	assert.Greater(t, limit, int64(0))
	t.Logf("ceiling %d bytes", limit)
}

// TestAnExplicitLimitWins proves the deference: capHeap leaves a limit somebody
// else set, which is what makes GOMEMLIMIT and go-toolchain's cgroup guard the
// answer rather than an argument with this one.
func TestAnExplicitLimitWins(t *testing.T) {
	const chosen = 123 << 20
	before := debug.SetMemoryLimit(chosen)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	capHeap()
	assert.Equal(t, int64(chosen), debug.SetMemoryLimit(-1))
}

// TestTheCollectorTargetDefersToGOGC is the same deference for the other knob.
// A person who names a target has said what this run may hold, and the default
// this sets is only for a run where nobody has.
func TestTheCollectorTargetDefersToGOGC(t *testing.T) {
	restore := debug.SetGCPercent(300)
	t.Cleanup(func() { debug.SetGCPercent(restore) })

	t.Setenv("GOGC", "300")
	setGCTarget()
	assert.Equal(t, 300, debug.SetGCPercent(300), "a target somebody chose is left alone")

	os.Unsetenv("GOGC")
	setGCTarget()
	assert.Equal(t, gcTarget, debug.SetGCPercent(gcTarget), "and one nobody chose is lowered")
}
