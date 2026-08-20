package repair_test

// Putting a run back: the quarantine holds every file a repair displaced, and
// an undo returns them without deleting what the repair had written.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// TestUndoPutsBackWhatARepairReplaced is the escape hatch the whole design
// rests on, and it did not work.
//
// Undo refused to restore over a file that was there again. But most of what a
// repair displaces, it also REPLACES -- a whole index over a broken one, a valid
// packed-refs over a malformed one -- so the path is always occupied and the
// undo failed on every run worth undoing, part way through, with exit 128.
//
// Nothing is overwritten even so: what the repair had written moves into the
// run's own "replaced" directory first.
func TestUndoPutsBackWhatARepairReplaced(t *testing.T) {
	gittest.RequireGit(t)
	r := packed(t)
	r.Git("pack-refs", "--all")

	pack := packFile(t, r)
	idxPath := filepath.Join(r.GitDir(), "index")
	refsPath := filepath.Join(r.GitDir(), "packed-refs")

	damage := func(path string, f func([]byte) []byte) []byte {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		out := f(data)
		overwrite(t, path, out)
		return out
	}
	wantPack := damage(pack, func(d []byte) []byte { d[len(d)/2] ^= 0xff; return d })
	wantIndex := damage(idxPath, func(d []byte) []byte { return d[:len(d)-8] })
	wantRefs := damage(refsPath, func(d []byte) []byte {
		lines := strings.Split(strings.TrimRight(string(d), "\n"), "\n")
		broken := append([]string{lines[0], "not a reference line"}, lines[1:]...)
		return []byte(strings.Join(broken, "\n") + "\n")
	})

	res := fix(t, r)
	require.NotEmpty(t, res.Quarantine)
	require.NotNil(t, res.Index)
	require.NotNil(t, res.PackedRefs)
	require.NoFileExists(t, pack, "the pack was not taken out")

	restored, err := repair.Undo(r.GitDir(), "test")
	require.NoError(t, err, "the undo did not finish")
	assert.NotEmpty(t, restored)

	// Byte for byte, the repository is back in the state the damage left it.
	for _, tc := range []struct {
		path string
		want []byte
	}{
		{pack, wantPack},
		{idxPath, wantIndex},
		{refsPath, wantRefs},
	} {
		got, err := os.ReadFile(tc.path)
		require.NoError(t, err, "%s was not restored", tc.path)
		assert.Equal(t, tc.want, got, "%s came back different", tc.path)
	}

	// And what the repair had written is kept, not deleted.
	aside := repair.ReplacedDir(r.GitDir(), "test")
	require.NotEmpty(t, aside, "the repaired files were deleted rather than set aside")
	assert.FileExists(t, filepath.Join(aside, "index"))
	assert.FileExists(t, filepath.Join(aside, "packed-refs"))
}

// TestUndoTwiceIsNotAnError runs the same undo again. The manifest still names
// every file, and none of them is in the quarantine any more.
func TestUndoTwiceIsNotAnError(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("commit-graph", "write", "--reachable")
	graph := filepath.Join(r.GitDir(), "objects", "info", "commit-graph")
	if _, err := os.Stat(graph); err != nil {
		t.Skipf("this git wrote no commit-graph: %v", err)
	}
	gittest.WriteOver(t, graph, []byte("XXXX not a commit graph"))

	fix(t, r)
	_, err := repair.Undo(r.GitDir(), "test")
	require.NoError(t, err)
	_, err = repair.Undo(r.GitDir(), "test")
	require.NoError(t, err, "a second undo of the same run must not fail")
	assert.FileExists(t, graph, "the first undo did not put the graph back")
}
