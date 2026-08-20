// Command git-fsck verifies the connectivity and validity of the objects in a
// repository. It accepts the same options as git fsck and reports the same
// findings, with the work spread across every core.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"

	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/parseopt"
)

var usage = []string{
	"git fsck [--tags] [--root] [--unreachable] [--cache] [--no-reflogs]\n" +
		"         [--[no-]full] [--strict] [--verbose] [--lost-found]\n" +
		"         [--[no-]dangling] [--[no-]progress] [--connectivity-only]\n" +
		"         [--[no-]name-objects] [<object>...]",
}

func main() {
	stop := startProfile()
	defer stop()
	code := run(os.Args[1:])
	stop()
	writeMemProfile()
	os.Exit(code)
}

func run(args []string) int {
	var (
		verbose         int
		showUnreachable int
		showDangling    = 1
		showTags        int
		showRoot        int
		keepCache       int
		includeReflogs  = 1
		checkFull       = 1
		connectivity    int
		strict          int
		lostFound       int
		progress        = -1
		nameObjects     int
	)
	set := &parseopt.Set{
		Usage: usage,
		Opts: []*parseopt.Bool{
			{Short: 'v', Long: "verbose", Help: "be verbose", Value: &verbose},
			{Long: "unreachable", Help: "show unreachable objects", Value: &showUnreachable},
			{Long: "dangling", Help: "show dangling objects", Value: &showDangling},
			{Long: "tags", Help: "report tags", Value: &showTags},
			{Long: "root", Help: "report root nodes", Value: &showRoot},
			{Long: "cache", Help: "make index objects head nodes", Value: &keepCache},
			{Long: "reflogs", Help: "make reflogs head nodes (default)", Value: &includeReflogs},
			{Long: "full", Help: "also consider packs and alternate objects", Value: &checkFull},
			{Long: "connectivity-only", Help: "check only connectivity", Value: &connectivity},
			{Long: "strict", Help: "enable more strict checking", Value: &strict},
			{Long: "lost-found", Help: "write dangling objects in .git/lost-found", Value: &lostFound},
			{Long: "progress", Help: "show progress", Value: &progress},
			{Long: "name-objects", Help: "show verbose names for reachable objects", Value: &nameObjects},
		},
	}
	rest, err := set.Parse(args)
	if err != nil {
		var usageErr parseopt.ErrUsage
		if errors.As(err, &usageErr) {
			fmt.Fprintf(os.Stderr, "error: %s\n", usageErr.Msg)
			set.PrintUsage(os.Stderr)
			return 129
		}
		set.PrintUsage(os.Stdout)
		return 129
	}

	o := fsckcmd.DefaultOptions()
	o.Verbose = verbose != 0
	o.ShowUnreachable = showUnreachable != 0
	o.ShowDangling = showDangling != 0
	o.ShowTags = showTags != 0
	o.ShowRoot = showRoot != 0
	o.KeepCacheObjects = keepCache != 0
	o.IncludeReflogs = includeReflogs != 0
	o.CheckFull = checkFull != 0
	o.ConnectivityOnly = connectivity != 0
	o.Strict = strict != 0
	o.WriteLostFound = lostFound != 0
	o.NameObjects = nameObjects != 0
	o.Args = rest

	if progress == -1 {
		o.ShowProgress = isTerminal(os.Stderr)
	} else {
		o.ShowProgress = progress != 0
	}
	if o.Verbose {
		o.ShowProgress = false
	}
	if o.WriteLostFound {
		o.CheckFull = true
		o.IncludeReflogs = false
	}
	if n, err := strconv.Atoi(os.Getenv("GIT_FIXED_THREADS")); err == nil && n > 0 {
		o.Workers = n
	} else {
		o.Workers = runtime.GOMAXPROCS(0)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		return 128
	}
	o.Dir = cwd
	return fsckcmd.Run(o)
}

// isTerminal reports whether the stream is a character device, which is how
// git decides to show progress when nobody said either way.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// TEMPORARY: profiling hook for the benchmark work. Remove before commit.
func startProfile() func() {
	path := os.Getenv("GIT_FIXED_CPUPROFILE")
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		panic(err)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}
}

// TEMPORARY: allocation profile for the benchmark work. Remove before commit.
func writeMemProfile() {
	path := os.Getenv("GIT_FIXED_MEMPROFILE")
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
		panic(err)
	}
}
