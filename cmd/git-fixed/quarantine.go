package main

// The two modes that only read or undo a quarantine, and never scan anything.

import (
	"fmt"
	"io"
	"time"

	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// runName is the quarantine directory this run writes to. The clock names it,
// so the runs sort in the order they happened.
func runName() string {
	return time.Now().UTC().Format("20060102-150405")
}

// listQuarantines prints the runs whose files can still be put back.
func listQuarantines(dir string, stdout, stderr io.Writer) int {
	gitDir, code := commonDir(dir, stderr)
	if code != 0 {
		return code
	}
	runs := repair.Runs(gitDir)
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No run has displaced anything.")
		return 0
	}
	for _, r := range runs {
		fmt.Fprintln(stdout, r)
	}
	return 0
}

// undoRun puts one run's files back, the newest when none is named.
func undoRun(dir string, rest []string, stdout, stderr io.Writer) int {
	gitDir, code := commonDir(dir, stderr)
	if code != 0 {
		return code
	}
	name := ""
	if len(rest) > 0 {
		name = rest[0]
	} else if runs := repair.Runs(gitDir); len(runs) > 0 {
		name = runs[len(runs)-1]
	}
	if name == "" {
		fmt.Fprintln(stderr, "fatal: no run has displaced anything")
		return 128
	}
	restored, err := repair.Undo(gitDir, name)
	for _, f := range restored {
		fmt.Fprintf(stdout, "restored: %s\n", f.From)
	}
	if err != nil {
		fmt.Fprintf(stderr, "fatal: %s\n", err)
		return 128
	}
	fmt.Fprintf(stdout, "Run %s is undone.\n", name)
	if aside := repair.ReplacedDir(gitDir, name); aside != "" {
		fmt.Fprintf(stdout, "What the repair had written is in %s.\n", aside)
	}
	return 0
}

// commonDir finds the directory a repository's quarantine lives under.
func commonDir(dir string, stderr io.Writer) (string, int) {
	repo, err := gitrepo.Open(dir)
	if err != nil {
		fmt.Fprintf(stderr, "fatal: %s\n", err)
		return "", 128
	}
	return repo.CommonDir, 0
}
