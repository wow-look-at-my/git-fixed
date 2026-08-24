package main

import (
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// capHeap makes a repository too large for the machine cost time instead of costing the run. see docs/architecture.md
func capHeap() {
	// Both of these mean somebody has already chosen, and GOMEMLIMIT=off is
	// how a person turns the whole thing off.
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		return
	}
	if debug.SetMemoryLimit(-1) != math.MaxInt64 {
		return
	}

	// Absent on every system that is not Linux, and the ceiling goes with it.
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	if limit, ok := heapLimit(string(data)); ok {
		debug.SetMemoryLimit(limit)
	}
}

// heapLimit is three quarters of what the machine has.
//
// The other quarter is not spare. A packfile is read through a mapping, which
// is not part of the Go heap and does not count against this number, and the
// machine has other work on it.
func heapLimit(meminfo string) (int64, bool) {
	for line := range strings.SplitSeq(meminfo, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "MemTotal" {
			continue
		}
		// "MemTotal: 65759416 kB".
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0, false
		}
		return kb / 4 * 3 * 1024, true
	}
	return 0, false
}
