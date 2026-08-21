package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
)

// repo builds a repository with a commit and an index, which is the shape both
// halves of the command have something to say about.
func repo(t *testing.T) *gittest.Repo {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.Git("read-tree", "HEAD")
	return r
}

// invoke runs the command over a repository and collects what it printed.
func invoke(t *testing.T, r *gittest.Repo, args ...string) gittest.Result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"-C", r.Dir}, args...), &out, &errOut)
	return gittest.Result{Stdout: out.String(), Stderr: errOut.String(), Code: code}
}

// fingerprint is every file under a directory and what is in it. Two of these
// are equal exactly when nothing was written, added, or removed.
func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s %x", path, sha256.Sum256(data)))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// breakIndex truncates the index so it will not parse, and returns what it held
// before, so an undo can be checked against it.
func breakIndex(t *testing.T, r *gittest.Repo) []byte {
	t.Helper()
	path := filepath.Join(r.GitDir(), "index")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Greater(t, len(data), 12, "git wrote no index to break")
	gittest.WriteOver(t, path, data[:8])
	return data
}

// TestDryRunOnAHealthyRepositoryIsGitFsck is the drop-in case, and the reason
// --dry-run prints nothing of its own when there is nothing to repair. One
// binary now does both jobs, so this is the surface that has to keep standing
// in for git fsck exactly: same lines, same exit status.
func TestDryRunOnAHealthyRepositoryIsGitFsck(t *testing.T) {
	r := repo(t)
	before := fingerprint(t, r.GitDir())

	want := r.GitFsck()
	got := invoke(t, r, "--dry-run")

	assert.Equal(t, want.Lines(), got.Lines(), "a --dry-run said something git fsck does not")
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, before, fingerprint(t, r.GitDir()), "a --dry-run wrote to the repository")
}

// TestDryRunPassesGitFsckOptionsThrough covers the other half of standing in
// for git fsck: the options still reach it and still mean what git means.
func TestDryRunPassesGitFsckOptionsThrough(t *testing.T) {
	r := repo(t)
	r.Blob("loose and unreferenced\n")

	for _, args := range [][]string{
		{"--dangling"},
		{"--unreachable"},
		{"--no-dangling"},
		{"--strict", "--root", "--tags"},
		{"--connectivity-only"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			want := r.GitFsck(args...)
			got := invoke(t, r, append([]string{"--dry-run"}, args...)...)
			assert.Equal(t, want.Lines(), got.Lines())
			assert.Equal(t, want.Code, got.Code)
		})
	}
}

// TestHealthyRepositoryIsLeftAlone is the case that must stay boring: the
// repair half of a default run has nothing to do and touches nothing.
func TestHealthyRepositoryIsLeftAlone(t *testing.T) {
	r := repo(t)
	before := fingerprint(t, r.GitDir())

	got := invoke(t, r)
	assert.Equal(t, 0, got.Code)
	assert.Contains(t, got.Stdout, "Nothing to repair.")
	assert.Equal(t, before, fingerprint(t, r.GitDir()))
	assert.NoDirExists(t, filepath.Join(r.GitDir(), "git-fixed"))
}

// TestDryRunPlansARepairWithoutMakingOne proves the plan is a plan. The index
// is unreadable, the command says what it would rebuild, and the broken file is
// still exactly as broken afterwards.
func TestDryRunPlansARepairWithoutMakingOne(t *testing.T) {
	r := repo(t)
	breakIndex(t, r)
	before := fingerprint(t, r.GitDir())

	got := invoke(t, r, "--dry-run")
	assert.Contains(t, got.Stdout, "would rebuild: .git/index")
	assert.Contains(t, got.Stderr, ".git/index: index file smaller than expected",
		"the diagnosis git prints must come out before the plan")
	// git exits 128 here and stops. This says which file is unusable and
	// checks the rest of the repository. see docs/exit-status.md
	assert.Equal(t, 128, r.GitFsck().Code)
	assert.Equal(t, fsckcmd.ErrorIndex, got.Code,
		"the status must name the fault, not say the run gave up")
	assert.Equal(t, before, fingerprint(t, r.GitDir()), "a --dry-run repaired something")
}

// TestDefaultRunDiagnosesThenRepairs is the whole point of one binary: what
// used to need two commands is one, and the diagnosis is still git's.
func TestDefaultRunDiagnosesThenRepairs(t *testing.T) {
	r := repo(t)
	breakIndex(t, r)

	got := invoke(t, r)
	assert.Equal(t, 0, got.Code)
	assert.Contains(t, got.Stderr, ".git/index: index file smaller than expected")
	assert.Contains(t, got.Stdout, "rebuilt: .git/index")
	assert.Contains(t, got.Stdout, "The repository is whole.")
	assert.Equal(t, "f", strings.TrimSpace(r.Git("ls-files")), "the rebuilt index lost the tracked file")
}

// TestUndoPutsTheRunBack covers the escape hatch the no-delete rule rests on,
// through the one binary's own spelling of it.
func TestUndoPutsTheRunBack(t *testing.T) {
	r := repo(t)
	broken := breakIndex(t, r)
	require.Equal(t, 0, invoke(t, r).Code)

	listed := invoke(t, r, "--list-runs")
	assert.Equal(t, 0, listed.Code)
	assert.NotContains(t, listed.Stdout, "No run has displaced anything")

	undone := invoke(t, r, "--undo")
	assert.Equal(t, 0, undone.Code)
	assert.Contains(t, undone.Stdout, "is undone.")

	back, err := os.ReadFile(filepath.Join(r.GitDir(), "index"))
	require.NoError(t, err)
	assert.Equal(t, broken[:8], back, "the undo did not put the original index back")
}

// TestUndoCannotBeADryRun refuses the one combination with no honest reading:
// an undo that changed nothing would put nothing back.
func TestUndoCannotBeADryRun(t *testing.T) {
	r := repo(t)
	got := invoke(t, r, "--undo", "--dry-run")
	assert.Equal(t, 129, got.Code)
	assert.Contains(t, got.Stderr, "cannot be a --dry-run")
}

// TestUnknownOptionIsAUsageError keeps git's own exit status for a command line
// it will not take.
func TestUnknownOptionIsAUsageError(t *testing.T) {
	r := repo(t)
	got := invoke(t, r, "--not-an-option")
	assert.Equal(t, 129, got.Code)
	assert.Contains(t, got.Stderr, "unknown option `not-an-option'")
}

// TestSameVerdictOnlyWhenTheQuestionIs guards the shortcut that stops a healthy
// repository being read twice. Reusing the answer is only sound when the fsck
// the command ran asked what the repair's own verification asks; every case
// below asks something else, and must fall back to asking again.
func TestSameVerdictOnlyWhenTheQuestionIs(t *testing.T) {
	assert.True(t, sameVerdict(newFsckFlags().options(".", nil, os.Stdout, os.Stderr)))

	narrower := map[string]func(*fsckFlags){
		"--strict":            func(f *fsckFlags) { f.strict = 1 },
		"--connectivity-only": func(f *fsckFlags) { f.connectivity = 1 },
		"--cache":             func(f *fsckFlags) { f.cache = 1 },
		"--lost-found":        func(f *fsckFlags) { f.lostFound = 1 },
		"--no-full":           func(f *fsckFlags) { f.full = 0 },
		"--no-references":     func(f *fsckFlags) { f.references = 0 },
		"--no-reflogs":        func(f *fsckFlags) { f.reflogs = 0 },
	}
	for name, set := range narrower {
		t.Run(name, func(t *testing.T) {
			f := newFsckFlags()
			set(f)
			assert.False(t, sameVerdict(f.options(".", nil, os.Stdout, os.Stderr)),
				"%s asks fsck something else, so its answer cannot stand in", name)
		})
	}

	t.Run("a named object", func(t *testing.T) {
		o := newFsckFlags().options(".", []string{"HEAD"}, os.Stdout, os.Stderr)
		assert.False(t, sameVerdict(o), "a check of one object says nothing about the rest")
	})

	// Everything that only shapes the report leaves the verdict alone.
	f := newFsckFlags()
	f.verbose, f.unreachable, f.tags, f.root, f.nameObjects, f.dangling = 1, 1, 1, 1, 1, 0
	assert.True(t, sameVerdict(f.options(".", nil, os.Stdout, os.Stderr)))
}

// TestProgressFollowsTheStreamItIsWrittenTo keeps a progress meter out of
// output that is being read by something other than a person.
func TestProgressFollowsTheStreamItIsWrittenTo(t *testing.T) {
	var buf bytes.Buffer
	o := newFsckFlags().options(".", nil, &buf, &buf)
	assert.False(t, o.ShowProgress)

	f := newFsckFlags()
	f.progress = 1
	assert.True(t, f.options(".", nil, &buf, &buf).ShowProgress)
}

// TestOptionsCarryGitsResolvedDefaults covers the two rules git applies after
// it has read the whole command line, rather than as it reads it.
func TestOptionsCarryGitsResolvedDefaults(t *testing.T) {
	f := newFsckFlags()
	f.lostFound = 1
	f.full = 0
	f.reflogs = 1
	o := f.options(".", nil, os.Stdout, os.Stderr)
	assert.True(t, o.CheckFull, "--lost-found needs the full check")
	assert.False(t, o.IncludeReflogs, "--lost-found drops the reflogs as roots")

	f = newFsckFlags()
	f.verbose = 1
	f.progress = 1
	assert.False(t, f.options(".", nil, os.Stdout, os.Stderr).ShowProgress,
		"verbose output and a progress meter cannot share a stream")

	o2 := newFsckFlags().options("/somewhere", []string{"HEAD"}, os.Stdout, os.Stderr)
	assert.Equal(t, "/somewhere", o2.Dir)
	assert.Equal(t, []string{"HEAD"}, o2.Args)
}

// TestTheFsckVerdictIsJudgedBeforeItRuns pins the ordering the shortcut needs,
// and the trap that made it dead code on every single run.
//
// fsck resolves some options as it goes and writes them back into the struct it
// was given: with no object named, the index becomes a head. Asking afterwards
// therefore asks about a command line nobody typed, the answer is always no,
// and the repair reads the whole repository a second time for nothing.
func TestTheFsckVerdictIsJudgedBeforeItRuns(t *testing.T) {
	r := repo(t)
	o := newFsckFlags().options(r.Dir, nil, io.Discard, io.Discard)
	require.True(t, sameVerdict(o), "a plain command line asks fsck the question the repair asks")

	require.Equal(t, 0, fsckcmd.Run(o))
	assert.False(t, sameVerdict(o),
		"fsck no longer rewrites the options it was given, so run() can stop working around it")
}

// TestDryRunStillWritesWhatLostFoundWrites pins the one thing a --dry-run does
// put on disk, so that it stays a decision rather than becoming a surprise.
//
// --lost-found is git's own option and writing dangling objects is the whole of
// what it does. A --dry-run promises to repair nothing, not to write nothing,
// and refusing the pair would take git's one command for saving those objects
// away from the mode that stands in for git fsck.
func TestDryRunStillWritesWhatLostFoundWrites(t *testing.T) {
	r := repo(t)
	r.Blob("loose and unreferenced\n")

	got := invoke(t, r, "--dry-run", "--lost-found")
	assert.Equal(t, 0, got.Code)
	assert.DirExists(t, filepath.Join(r.GitDir(), "lost-found"),
		"--lost-found stopped writing, so it no longer does what git's option does")
}

// TestDryRunSaysWhatItCouldAndCouldNotRepair is the whole job of a plan.
//
// It used to list every damaged object as one that could not be recovered,
// because it never asked. A person whose objects were all one command away
// from being put back was told they were gone.
func TestDryRunSaysWhatItCouldAndCouldNotRepair(t *testing.T) {
	gittest.RequireGit(t)

	t.Run("recoverable", func(t *testing.T) {
		r := gittest.New(t)
		_, tree, _ := r.SimpleHistory()
		r.Git("read-tree", "HEAD")
		// The tree's only copy is corrupt and the index still records what
		// was in it, so a repair rebuilds the tree from there.
		overwriteObject(t, r, tree)
		before := fingerprint(t, r.GitDir())

		got := invoke(t, r, "--dry-run")
		assert.Contains(t, got.Stdout, "would recover: tree "+tree.String())
		assert.Contains(t, got.Stdout, "1 fault(s) would be repaired, 0 would not.")
		assert.NotContains(t, got.Stdout, "could not be recovered")
		assert.Equal(t, before, fingerprint(t, r.GitDir()), "a --dry-run repaired something")
	})

	t.Run("unrecoverable", func(t *testing.T) {
		r := gittest.New(t)
		blob, _, _ := r.SimpleHistory()
		// Nothing else in this repository holds the blob's content, and
		// there is no worktree and no remote to ask.
		overwriteObject(t, r, blob)

		got := invoke(t, r, "--dry-run")
		assert.Contains(t, got.Stdout, "0 fault(s) would be repaired, 1 would not.")
		assert.Contains(t, got.Stdout, "could not be recovered")
	})
}

// overwriteObject makes one object's only file unreadable.
func overwriteObject(t *testing.T, r *gittest.Repo, oid gitobj.OID) {
	t.Helper()
	name := oid.String()
	gittest.WriteOver(t, filepath.Join(r.GitDir(), "objects", name[:2], name[2:]),
		[]byte("this is not a git object"))
}
