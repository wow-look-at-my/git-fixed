package main

// CPU profiling, for the only question this tool is ever asked about itself:
// where does the time go.

import (
	"fmt"
	"os"
	"runtime/pprof"
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
