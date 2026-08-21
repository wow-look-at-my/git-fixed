// Command git-fixed repairs a repository git has broken.
//
// A run is a diagnosis and then a repair. The diagnosis is a full fsck: the
// same checks git fsck makes, spread across every core, reporting the same
// findings with the same exit status. The repair puts back what the diagnosis
// found. --dry-run stops after the diagnosis and changes nothing.
//
// Nothing is ever deleted. A file the repair displaces goes to a quarantine
// directory that --undo empties back, and an object no source has is reported
// rather than amputated.
//
// see docs/repair.md
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/memwatch"
	"github.com/wow-look-at-my/git-fixed/internal/parseopt"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

var usage = []string{
	"git-fixed [-C <directory>] [--dry-run] [<fsck options>] [<object>...]",
	"git-fixed [-C <directory>] --undo [<run>]",
	"git-fixed [-C <directory>] --list-runs",
}

func main() {
	// Here rather than in an init, so it runs after the guard go-toolchain
	// injects and can see whether that guard already set a limit.
	capHeap()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var (
		dryRun   int
		undo     int
		listRuns int
		dir      string
	)
	f := newFsckFlags()
	set := &parseopt.Set{
		Usage: usage,
		Opts: append([]*parseopt.Bool{
			{Short: 'n', Long: "dry-run", Help: "report what is wrong, and repair nothing", Value: &dryRun},
			{Long: "undo", Help: "put a run's displaced files back", Value: &undo},
			{Long: "list-runs", Help: "list the runs whose files can be put back", Value: &listRuns},
		}, f.table()...),
		Strs: []*parseopt.Str{
			{Short: 'C', Long: "directory", Arg: "<directory>", Help: "run as if started in <directory>", Value: &dir},
		},
	}
	rest, err := set.Parse(args)
	if err != nil {
		var usageErr parseopt.ErrUsage
		if errors.As(err, &usageErr) {
			fmt.Fprintf(stderr, "error: %s\n", usageErr.Msg)
			set.PrintUsage(stderr)
			return 129
		}
		set.PrintUsage(stdout)
		return 129
	}

	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "fatal: %s\n", err)
			return 128
		}
		dir = cwd
	}

	if undo != 0 && dryRun != 0 {
		// An undo that changed nothing would put nothing back, so there is
		// no reading of this that does what both options ask for.
		fmt.Fprintln(stderr, "error: --undo puts files back, so it cannot be a --dry-run")
		set.PrintUsage(stderr)
		return 129
	}

	switch {
	case listRuns != 0:
		return listQuarantines(dir, stdout, stderr)
	case undo != 0:
		return undoRun(dir, rest, stdout, stderr)
	}

	stop, err := startProfile()
	if err != nil {
		fmt.Fprintf(stderr, "fatal: %s\n", err)
		return 128
	}
	defer stop()

	heapStop, err := startHeapProfile(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "fatal: %s\n", err)
		return 128
	}
	defer heapStop()

	// The diagnosis comes first, and it is git's own. A repair that printed
	// only what it changed would leave nobody able to see what was wrong.
	o := f.options(dir, rest, stdout, stderr)

	// What the run cost, said once at the end, in the words the meters have
	// been drawing all along.
	defer reportMemory(o.ShowProgress, stderr)

	// Collected while fsck runs, because it is the one thing the status word
	// cannot say: which packs were read end to end and were fine. The repair
	// scan is about to want exactly that, and reading them again is the
	// longest part of a run.
	var verified []string
	var verifiedMu sync.Mutex
	o.PackVerified = func(path string) {
		verifiedMu.Lock()
		verified = append(verified, path)
		verifiedMu.Unlock()
	}

	// The same for the other thing the status word cannot say: which objects
	// were actually unreadable, rather than merely badly written.
	var damaged []gitobj.OID
	damageWhole := false
	o.ObjectsDamaged = func(oids []gitobj.OID, whole bool) {
		damaged, damageWhole = oids, whole
	}

	// A run that stops part way has checked part of the repository, so
	// nothing it did not reach may be taken on trust.
	stopped := false
	o.Stopped = func(string) { stopped = true }

	// Asked before the run, not after. fsck resolves some of its options as it
	// goes and writes them back here: with no object named it makes the index
	// a head, so afterwards these options no longer describe the command line
	// and this reads false on every ordinary run.
	reusable := sameVerdict(o)
	status := fsckcmd.Run(o)

	// The whole status word, not a yes or no. Its bits say which of the
	// scan's own passes have just been made for it: a repository whose
	// references or caches are wrong has had every object read and approved
	// by the fsck that found those faults.
	var verdict *repair.Verdict
	if reusable && !stopped {
		verdict = &repair.Verdict{
			Status:      status,
			Verified:    verified,
			Damaged:     damaged,
			DamageWhole: damageWhole,
		}
	}

	res, err := repair.Run(&repair.Options{
		Dir:          dir,
		DryRun:       dryRun != 0,
		Run:          runName(),
		Verdict:      verdict,
		ShowProgress: o.ShowProgress,
		Stdout:       stdout,
		Stderr:       stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "fatal: %s\n", err)
		return 128
	}
	if dryRun != 0 {
		return reportPlan(res, status, stdout, stderr)
	}
	return reportRepair(res, stdout, stderr)
}

// reportPlan says what a --dry-run would have repaired, and gives back the
// status the fsck above it reached.
//
// "repair nothing" is the promise, not "write nothing". --lost-found is git's
// own option and writing is what it does, so it still writes under a --dry-run,
// as it would under git fsck. Refusing the pair would take the one command git
// has for saving dangling objects away from the mode that stands in for it.
//
// A repository with nothing to repair gets no line at all. That silence is the
// point: there, a --dry-run is exactly git fsck, output and exit status alike,
// and a "nothing to repair" line would be one line git does not print.
func reportPlan(res *repair.Result, status int, stdout, stderr io.Writer) int {
	switch {
	case res.Nothing():
	case res.FoundNothingToDo():
		fmt.Fprintln(stderr, "Every finding above is something this tool does not repair, so a run\n"+
			"would change nothing. It repairs a corrupt or missing object, a broken\n"+
			"reference, a packfile that will not verify, an index or a packed-refs\n"+
			"that will not parse, and a rebuildable cache. Nothing above is one of those.")
	default:
		res.Report(stdout, true)
		res.ReportPlanTotals(stdout)
	}
	return status
}

// reportRepair says what a run did, and decides the status from whether the
// repository ended whole.
func reportRepair(res *repair.Result, stdout, stderr io.Writer) int {
	switch {
	case res.Nothing():
		fmt.Fprintln(stdout, "Nothing to repair.")
		return 0
	case res.FoundNothingToDo():
		// git is unhappy about something this tool does not know how to
		// repair. The findings above say what it is. A quiet exit here
		// would read as a clean bill of health.
		fmt.Fprintln(stderr, "The damage above is not something this tool repairs. Nothing was changed.")
		return 1
	}
	res.Report(stdout, false)
	if !res.Ok() {
		return 1
	}
	fmt.Fprintln(stdout, "\nThe repository is whole.")
	return 0
}

// reportMemory says what the run cost the machine at its worst moment.
//
// It rides with the progress meters: one switch turns both on and both go to
// stderr. A --dry-run stands in for git fsck, and a line here that git does not
// print would be a line in that output.
//
// A system that publishes no marks gets no line, as it gets no heap ceiling.
// see docs/memory.md
func reportMemory(show bool, stderr io.Writer) {
	if !show {
		return
	}
	marks, ok := memwatch.Peak()
	if !ok {
		return
	}
	fmt.Fprintf(stderr, "Peak memory: %s.\n", marks)
}
