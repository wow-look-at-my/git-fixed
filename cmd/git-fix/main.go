// Command git-fix repairs a damaged repository without losing anything.
//
// It recovers every object it can, from the repository itself before the
// network, and it never deletes: a file it has to displace goes to a quarantine
// directory that --undo empties back. An object no source has is reported and
// the command fails, rather than being amputated to make the report look clean.
//
// see docs/repair.md
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/parseopt"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

var usage = []string{
	"git fix [--dry-run] [<directory>]\n" +
		"git fix --undo [<directory>] [<run>]\n" +
		"git fix --list-runs [<directory>]",
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var (
		dryRun   int
		undo     int
		listRuns int
	)
	set := &parseopt.Set{
		Usage: usage,
		Opts: []*parseopt.Bool{
			{Short: 'n', Long: "dry-run", Help: "report what would be repaired, and change nothing", Value: &dryRun},
			{Long: "undo", Help: "put a run's displaced files back", Value: &undo},
			{Long: "list-runs", Help: "list the runs whose files can be put back", Value: &listRuns},
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

	dir := "."
	if len(rest) > 0 {
		dir = rest[0]
	}

	switch {
	case listRuns != 0:
		return listQuarantines(dir)
	case undo != 0:
		return undoRun(dir, rest)
	}

	res, err := repair.Run(&repair.Options{
		Dir:    dir,
		DryRun: dryRun != 0,
		Run:    runName(),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		return 128
	}

	if res.Nothing() {
		fmt.Fprintln(os.Stdout, "Nothing to repair.")
		return 0
	}
	res.Report(os.Stdout, dryRun != 0)
	if dryRun != 0 {
		return 0
	}
	if !res.Ok() {
		return 1
	}
	fmt.Fprintln(os.Stdout, "\nThe repository is whole.")
	return 0
}

// runName is the quarantine directory this run writes to. The clock names it,
// so the runs sort in the order they happened.
func runName() string {
	return time.Now().UTC().Format("20060102-150405")
}

// listQuarantines prints the runs whose files can still be put back.
func listQuarantines(dir string) int {
	gitDir, code := commonDir(dir)
	if code != 0 {
		return code
	}
	runs := repair.Runs(gitDir)
	if len(runs) == 0 {
		fmt.Fprintln(os.Stdout, "No run has displaced anything.")
		return 0
	}
	for _, r := range runs {
		fmt.Fprintln(os.Stdout, r)
	}
	return 0
}

// undoRun puts one run's files back, the newest when none is named.
func undoRun(dir string, rest []string) int {
	gitDir, code := commonDir(dir)
	if code != 0 {
		return code
	}
	name := ""
	if len(rest) > 1 {
		name = rest[1]
	} else if runs := repair.Runs(gitDir); len(runs) > 0 {
		name = runs[len(runs)-1]
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "fatal: no run has displaced anything")
		return 128
	}
	restored, err := repair.Undo(gitDir, name)
	for _, f := range restored {
		fmt.Fprintf(os.Stdout, "restored: %s\n", f.From)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		return 128
	}
	fmt.Fprintf(os.Stdout, "Run %s is undone.\n", name)
	return 0
}

// commonDir finds the directory a repository's quarantine lives under.
func commonDir(dir string) (string, int) {
	repo, err := gitrepo.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		return "", 128
	}
	return repo.CommonDir, 0
}
