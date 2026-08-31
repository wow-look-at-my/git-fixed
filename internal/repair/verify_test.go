package repair_test

// What a run takes on trust from its caller, and what it must go and check for
// itself.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// TestTheCallersFsckAnswerIsUsed proves the shortcut that keeps a healthy
// repository from being scanned again needlessly.
//
// The command runs a full fsck of its own to report git's findings, so on a
// repository the scan finds nothing wrong with, the answer is already known.
// The value handed in below is a lie the test tells deliberately: the
// repository is healthy, so if Run went and asked fsck for itself it would come
// back with the opposite and the assertion would fail.
func TestTheCallersFsckAnswerIsUsed(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	res, err := repair.Run(&repair.Options{
		Dir:     r.Dir,
		Run:     "test",
		Verdict: &repair.Verdict{Status: fsckcmd.ErrorObject},
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	require.NoError(t, err)
	assert.False(t, res.Clean, "Run read the whole repository again instead of using the answer it was given")
	assert.True(t, res.FoundNothingToDo())

	// Without a caller-supplied verdict, it asks, and gets the truth.
	assert.True(t, fix(t, r).Nothing())
}

// TestATrustedScanStillChecksWhatFsckDoesNot is the other half of trusting the
// caller's answer. git never verifies objects/info/packs -- it is a cache for
// dumb HTTP clients -- so a stale cache leaves fsck perfectly happy. Skipping the
// whole scan on a clean fsck would have stopped repairing it, and said nothing.
func TestATrustedScanStillChecksWhatFsckDoesNot(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	info := filepath.Join(r.GitDir(), "objects", "info")
	require.NoError(t, os.MkdirAll(info, 0o777))
	require.NoError(t, os.WriteFile(filepath.Join(info, "packs"),
		[]byte("P pack-0000000000000000000000000000000000000000.pack\n\n"), 0o666))
	require.Equal(t, 0, r.GitFsck().Code, "git must be happy here, or this proves nothing")

	res, err := repair.Run(&repair.Options{
		Dir:     r.Dir,
		Run:     "test",
		Verdict: &repair.Verdict{},
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	require.NoError(t, err)
	require.Len(t, res.Derived, 1, "a scan that trusts fsck stopped looking at info/packs")
	assert.Equal(t, ".git/objects/info/packs", res.Derived[0])
}
