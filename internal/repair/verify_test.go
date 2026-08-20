package repair_test

// What a run takes on trust from its caller, and what it must go and check for
// itself.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// TestTheCallersFsckAnswerIsUsed proves the shortcut that keeps a healthy
// repository from being read twice.
//
// The command runs a full fsck of its own to report git's findings, so on a
// repository the scan finds nothing wrong with, the answer is already known.
// The value handed in below is a lie the test tells deliberately: the
// repository is healthy, so if Run went and asked fsck for itself it would come
// back with the opposite and the assertion would fail.
func TestTheCallersFsckAnswerIsUsed(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	lie := false
	res, err := repair.Run(&repair.Options{
		Dir:     r.Dir,
		Run:     "test",
		Healthy: &lie,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	require.NoError(t, err)
	assert.False(t, res.Clean, "Run read the whole repository again instead of using the answer it was given")
	assert.True(t, res.FoundNothingToDo())

	// Without one, it asks, and gets the truth.
	assert.True(t, fix(t, r).Nothing())
}
