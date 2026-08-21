package repair_test

// What a second pass of a run is allowed to read again.
//
// A repair goes round more than once, because one pass repairs one layer. Every
// pass used to start by reading every packfile in the repository from end to
// end. Over a hundred million objects that is fifteen minutes, and a run of
// four passes spent an hour of it reaching an answer it had before it started.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// meterRuns counts the meters one run drew under a title.
//
// A meter rewrites its own line, so it returns the cursor with a carriage
// return and writes a newline only when the phase ends. One meter is therefore
// one newline-terminated chunk, however many times it redrew inside it, and
// counting the title alone counts redraws.
//
// The meter is the evidence a person actually sees, and it is what the report of
// this bug was made of: four "Verifying packs: 100% ... done." lines from one
// run. A scan with nothing to verify starts no meter at all.
func meterRuns(drawn, title string) int {
	n := 0
	for _, line := range strings.Split(drawn, "\n") {
		if strings.Contains(line, title+":") {
			n++
		}
	}
	return n
}

// fixWithMeters repairs the repository and gives back what the meters drew.
func fixWithMeters(t *testing.T, r *gittest.Repo) (*repair.Result, string) {
	t.Helper()
	var meters bytes.Buffer
	res, err := repair.Run(&repair.Options{
		Dir:          r.Dir,
		Run:          "test",
		ShowProgress: true,
		Stdout:       os.Stdout,
		Stderr:       &meters,
	})
	require.NoError(t, err)
	return res, meters.String()
}

// TestASecondPassDoesNotReadThePacksAgain is the fault itself, in the shape a
// person hit it: a repository whose packs are perfectly fine and whose
// packed-refs is not.
//
// Repairing packed-refs makes the run scan again, because a line git's reader
// refuses hides every reference below it, and the objects those references lead
// to look unreferenced until the file is back. That second scan has every reason
// to walk the references again. It has no reason whatever to re-read a pack: a
// pass writes loose objects and moves whole packs away, and it never writes into
// one.
func TestASecondPassDoesNotReadThePacksAgain(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("branch", "keep-me")
	r.Git("repack", "-adq")
	r.Git("pack-refs", "--all")
	before := record(t, r)

	path := filepath.Join(r.GitDir(), "packed-refs")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	require.Greater(t, len(lines), 1, "pack-refs wrote nothing to damage")
	broken := append([]string{lines[0], "this is not a reference line"}, lines[1:]...)
	overwrite(t, path, []byte(strings.Join(broken, "\n")+"\n"))
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res, meters := fixWithMeters(t, r)

	require.NotNil(t, res.PackedRefs, "the run did not repair packed-refs, so it never scanned twice")
	require.Equal(t, 2, meterRuns(meters, "Checking what the references reach"),
		"the run made one scan, so it never had the chance to re-read a pack")
	assert.Equal(t, 1, meterRuns(meters, "Verifying packs"),
		"the packs were read once per pass instead of once per run:\n%s", meters)
	requireGitClean(t, r)
	requireSame(t, before, r)
}

// TestAChainOfMissingObjectsCostsOneWalk is the other half of the same hour.
//
// A missing tree hides everything under it, so a scan sees only the top of a
// chain and one pass repairs one layer. Every later pass used to find its layer
// by walking every object every reference reaches -- over a hundred million
// objects that is five minutes, once per layer, to arrive under the object the
// run was already holding.
//
// The chain here is four deep: the commit's tree, a directory in it, a
// directory in that, and a file at the bottom. Only the first is visible when
// the run starts.
func TestAChainOfMissingObjectsCostsOneWalk(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	before := record(t, r)

	// Every name first. Destroying the tree at the top makes git unable to
	// resolve the ones below it.
	var chain []string
	for _, rev := range []string{"HEAD^{tree}", "HEAD:src", "HEAD:src/deep", "HEAD:src/deep/c.txt"} {
		chain = append(chain, strings.TrimSpace(r.Git("rev-parse", rev)))
	}
	for _, oid := range chain {
		gittest.WriteOver(t, r.ObjectPath(mustOID(t, r, oid)), []byte("garbage"))
	}
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res, meters := fixWithMeters(t, r)

	require.True(t, res.Ok(), "the chain was not repaired: %+v", res)
	requireGitClean(t, r)
	requireSame(t, before, r)
	assert.Len(t, res.Objects, len(chain), "a layer of the chain was missed")

	assert.Equal(t, 1, meterRuns(meters, "Checking what the references reach"),
		"the run walked from the references once per layer:\n%s", meters)
	assert.Positive(t, meterRuns(meters, "Checking what came back"),
		"the run never went looking under what it had put back:\n%s", meters)
}

// TestAScanVouchesForThePacksItRead is what makes the pass above possible: a
// scan hands the next one the packs it read, and what each file was when it
// read it.
func TestAScanVouchesForThePacksItRead(t *testing.T) {
	gittest.RequireGit(t)
	r := packed(t)
	pack := packFile(t, r)
	fi, err := os.Stat(pack)
	require.NoError(t, err)

	repo, err := gitrepo.Open(r.Dir)
	require.NoError(t, err)
	db, err := odb.Open(repo.ObjectsDir, repo.DisplayObjectsDir, repo.Algo)
	require.NoError(t, err)
	defer db.Close()

	d, err := repair.Scan(repo, db, repair.Meters{})
	require.NoError(t, err)
	require.Len(t, d.Verified, 1, "the scan did not vouch for the pack it read")
	assert.Equal(t, pack, d.Verified[0].Path)
	assert.Equal(t, fi.Size(), d.Verified[0].Size)
	assert.True(t, fi.ModTime().Equal(d.Verified[0].ModTime))
}
