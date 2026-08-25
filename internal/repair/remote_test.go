package repair_test

// What the recovery ladder asks a remote for.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// withRemote builds a repository whose objects a remote also has.
func withRemote(t *testing.T) *gittest.Repo {
	t.Helper()
	gittest.RequireGit(t)
	r := history(t)
	upstream := t.TempDir()
	r.Git("init", "--bare", upstream)
	r.Git("remote", "add", "origin", upstream)
	r.Git("push", "origin", "HEAD:refs/heads/master")
	return r
}

// runWith repairs and gives back everything the run wrote to stderr, which is
// where the fetch reports itself.
func runWith(t *testing.T, r *gittest.Repo, dryRun bool) (*repair.Result, string) {
	t.Helper()
	var errOut bytes.Buffer
	res, err := repair.Run(&repair.Options{
		Dir:    r.Dir,
		DryRun: dryRun,
		Run:    "test",
		Stdout: os.Stdout,
		Stderr: &errOut,
	})
	require.NoError(t, err)
	return res, errOut.String()
}

// TestTheRemoteIsAskedForTheObjectsByName is the whole point of the change: a
// repository missing three objects costs three objects, not a copy of itself.
//
// The fallback announces itself, so its absence is the assertion. A server that
// refuses to serve an object by name still gets the old behaviour, and says so.
func TestTheRemoteIsAskedForTheObjectsByName(t *testing.T) {
	r := withRemote(t)
	before := record(t, r)
	for _, path := range looseObjects(t, r) {
		require.NoError(t, os.Remove(path))
	}

	res, stderr := runWith(t, r, false)
	assert.True(t, res.Ok(), "the repair did not finish: %+v", res.Unrecovered)
	assert.Empty(t, res.Unrecovered)
	assert.NotContains(t, stderr, "will not serve objects by name",
		"the objects were there to be asked for by name, and it fetched every ref instead")
	requireSame(t, before, r)
}

// TestADryRunNeverFetchesEveryRef is the disk nobody offered.
//
// A run that promises to change nothing may ask a remote for an object by name,
// which costs a round trip. It may not fall back to fetching every branch and
// tag, because that writes a copy of the repository into a temporary directory
// -- and on the repositories this tool is for, that fills the disk the
// repository is on.
//
// The repository behind this one is ninety objects, so what a bounded ask costs
// and what a copy costs are far enough apart to tell apart.
func TestADryRunNeverFetchesEveryRef(t *testing.T) {
	r := deepRemote(t, 30)
	for _, path := range looseObjects(t, r) {
		require.NoError(t, os.Remove(path))
	}

	_, stderr := runWith(t, r, true)
	assert.NotContains(t, stderr, "will not serve objects by name",
		"a dry run fetched every ref")
	assert.LessOrEqual(t, received(stderr), 10,
		"a dry run pulled %d objects, which is the repository", received(stderr))
}

// gitCall is one git command a run made, and the directory it ran in.
type gitCall struct {
	dir  string
	args []string
}

// traceGit puts a shim on the path that records every git command a run makes.
//
// The scratch repository a fetch writes into is deleted when the run ends, so
// what it cost has to be recorded while the run is happening.
func traceGit(t *testing.T) func() []gitCall {
	t.Helper()
	// Before the path changes, or this finds the shim.
	real, err := exec.LookPath("git")
	require.NoError(t, err)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	shim := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\t%%s\\n' \"$PWD\" \"$*\" >> %q\nexec %q \"$@\"\n", log, real)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o777))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []gitCall {
		data, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		var out []gitCall
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			where, args, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			out = append(out, gitCall{dir: where, args: strings.Fields(args)})
		}
		return out
	}
}

// remoteFetches are the fetches a run made into a scratch repository of its own.
func remoteFetches(calls []gitCall) []gitCall {
	var out []gitCall
	for _, c := range calls {
		if len(c.args) > 0 && c.args[0] == "fetch" && strings.Contains(c.dir, "git-fixed-remote-") {
			out = append(out, c)
		}
	}
	return out
}

// transferred finds the objects one fetch pulled, in git's own progress.
var transferred = regexp.MustCompile(`Receiving objects: 100% \((\d+)/\d+\), `)

func received(stderr string) int {
	n := 0
	for _, m := range transferred.FindAllStringSubmatch(stderr, -1) {
		got, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		n += got
	}
	return n
}

// deepRemote builds a repository with history behind it, and an upstream that
// serves objects the way a real server does.
func deepRemote(t *testing.T, commits int) *gittest.Repo {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.New(t)
	for i := range commits {
		r.Write("f.txt", fmt.Sprintf("revision %d\n", i))
		r.Git("add", "-A")
		r.Git("commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	upstream := t.TempDir()
	r.Git("init", "-q", "--bare", upstream)
	// What a real server allows: an object by name, and a filtered traversal.
	r.Git("-C", upstream, "config", "uploadpack.allowAnySHA1InWant", "true")
	r.Git("-C", upstream, "config", "uploadpack.allowFilter", "true")
	// file://, because a bare path is git's local transport: it copies rather
	// than negotiates, and reports nothing about what it moved.
	r.Git("remote", "add", "origin", "file://"+upstream)
	r.Git("push", "-q", "origin", "HEAD:refs/heads/master")
	return r
}

// TestARunFetchesEachObjectOnceAndNothingBehindIt is the cost of the remote
// rung, on the repositories this tool is for.
//
// Two commits are gone at different depths. The deeper one is invisible until
// the shallower one is back, so the run takes more than one pass -- and a pass
// used to open a scratch repository of its own, throw away what the pass before
// it had fetched, and ask the remote again. Nothing bounded the answer either:
// a commit asked for by name arrives with its whole ancestry behind it. On a
// repository of thirteen million objects that was eight gigabytes per pass.
//
// The three things measured here are the three that went wrong: one scratch
// repository for the run, no name asked for twice, and a transfer the size of
// what was asked for.
func TestARunFetchesEachObjectOnceAndNothingBehindIt(t *testing.T) {
	r := deepRemote(t, 30)
	before := record(t, r)

	var gone []string
	for _, rev := range []string{"HEAD", "HEAD~15"} {
		gone = append(gone, strings.TrimSpace(r.Git("rev-parse", rev)))
	}
	calls := traceGit(t)
	for _, oid := range gone {
		require.NoError(t, os.Remove(r.ObjectPath(mustOID(t, r, oid))))
	}
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res, stderr := runWith(t, r, false)
	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	requireGitClean(t, r)
	requireSame(t, before, r)

	fetched := remoteFetches(calls())
	require.NotEmpty(t, fetched, "nothing was fetched, so this measures nothing")
	assert.NotContains(t, stderr, "will not serve objects by name")

	scratch := map[string]bool{}
	for _, c := range fetched {
		scratch[c.dir] = true
	}
	assert.Len(t, scratch, 1,
		"the run fetched into %d scratch repositories, so it paid for the remote once per pass", len(scratch))

	asked := map[string]int{}
	for _, c := range fetched {
		for _, arg := range c.args {
			if name, _, _ := strings.Cut(arg, ":"); len(name) == 40 {
				asked[name]++
			}
		}
	}
	for _, oid := range gone {
		assert.Equal(t, 1, asked[oid], "%s was asked for %d times", oid, asked[oid])
	}

	// Thirty commits are ninety-odd objects. Two were missing.
	assert.LessOrEqual(t, received(stderr), 10,
		"%d objects came over the wire to recover %d: the fetch dragged the history behind it",
		received(stderr), len(gone))
}

// TestASecondPassAsksTheRemoteForNothingItAlreadyHas is the other half: a run
// that needs no new name must not fetch at all.
func TestASecondPassAsksTheRemoteForNothingItAlreadyHas(t *testing.T) {
	r := deepRemote(t, 10)
	oid := strings.TrimSpace(r.Git("rev-parse", "HEAD"))
	calls := traceGit(t)
	require.NoError(t, os.Remove(r.ObjectPath(mustOID(t, r, oid))))

	res, _ := runWith(t, r, false)
	require.True(t, res.Ok(), "the repair did not finish: %+v", res)

	fetched := remoteFetches(calls())
	assert.Len(t, fetched, 1, "one missing object cost %d fetches", len(fetched))
}

// TestAnObjectTheWorktreeHasIsNeverFetched keeps the remote at the bottom of
// the ladder where it belongs: it costs the network, and every rung above it
// costs a read.
func TestAnObjectTheWorktreeHasIsNeverFetched(t *testing.T) {
	r := deepRemote(t, 5)
	blob := strings.TrimSpace(r.Git("rev-parse", "HEAD:f.txt"))
	calls := traceGit(t)
	require.NoError(t, os.Remove(r.ObjectPath(mustOID(t, r, blob))))

	res, _ := runWith(t, r, false)
	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	assert.Empty(t, remoteFetches(calls()),
		"the file was sitting in the worktree and the run fetched it anyway")
}

// TestAnUnreachableRemoteIsSaidOutLoud keeps a failure to reach the network
// from reading as "your objects are gone".
func TestAnUnreachableRemoteIsSaidOutLoud(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("remote", "add", "origin", "file:///nowhere/there-is-no-repository-here.git")
	old := strings.TrimSpace(r.Git("rev-parse", "HEAD~1^{tree}"))
	require.NoError(t, os.Remove(r.ObjectPath(mustOID(t, r, old))))

	res, _ := runWith(t, r, false)
	assert.False(t, res.Ok())
	require.Error(t, res.RemoteError, "an unreachable remote is not the same as an object that is gone")
}
