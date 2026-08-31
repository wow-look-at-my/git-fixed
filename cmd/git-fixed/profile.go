package main

// Profiling, for the questions this tool is ever asked about itself: where
// the time goes, and where the memory goes.

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// startProfile begins a CPU profile when GIT_FIXED_CPUPROFILE names a file, and
// returns the function that finishes it.
//
// A failure here stops the run. Someone who asked for a profile and got a
// silently unprofiled run would be measuring nothing and would not know.
func startProfile() (func(), error) {
	path := os.Getenv("GIT_FIXED_CPUPROFILE")
	if path == "" {
		return func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating the profile %s: %w", path, err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("starting the profile: %w", err)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}, nil
}

// startHeapProfile arranges for a heap profile when GIT_FIXED_MEMPROFILE names a file.
func startHeapProfile(stderr io.Writer) (func(), error) {
	path := os.Getenv("GIT_FIXED_MEMPROFILE")
	if path == "" {
		return func() {}, nil
	}
	stop := peakHeap(stderr)
	return func() {
		stop()
		f, err := os.Create(path)
		if err != nil {
			return
		}
		defer f.Close()
		runtime.GC()
		_ = pprof.WriteHeapProfile(f)
	}, nil
}

// peakHeap reports the largest live heap the run reached.
//
// Peak RSS from the outside counts the mapped packfile pages, which the kernel
// reclaims and which no amount of work here changes. This counts the Go heap
// and nothing else, which is the number a change to a per-object structure
// moves. It samples rather than hooking the collector: ReadMemStats stops the
// world, so it runs at 20ms and only when a profile was asked for.
func peakHeap(stderr io.Writer) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var peak uint64
		var stats runtime.MemStats
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			runtime.ReadMemStats(&stats)
			peak = max(peak, stats.HeapAlloc)
			select {
			case <-done:
				fmt.Fprintf(stderr, "peak heap: %.1f MiB\n", float64(peak)/(1<<20))
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}
