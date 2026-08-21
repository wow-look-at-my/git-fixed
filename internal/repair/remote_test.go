package repair_test

// What the recovery ladder asks a remote for.
//
// The answer used to be "everything": every branch and every tag, into a
// scratch repository, to recover one object. On a monorepo that is a clone --
// tens of gigabytes over the network, onto whatever disk the temporary
// directory sits on -- and a --dry-run did it too.

import (
	"bytes"
	"os"
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
func TestADryRunNeverFetchesEveryRef(t *testing.T) {
	r := withRemote(t)
	for _, path := range looseObjects(t, r) {
		require.NoError(t, os.Remove(path))
	}

	_, stderr := runWith(t, r, true)
	assert.NotContains(t, stderr, "will not serve objects by name",
		"a dry run fetched every ref")
	for _, line := range strings.Split(stderr, "\n") {
		assert.NotContains(t, line, "Receiving objects",
			"a dry run pulled a whole repository")
	}
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
