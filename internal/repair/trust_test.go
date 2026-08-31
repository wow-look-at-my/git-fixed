package repair

// What a scan of a run may take on trust from an earlier scan.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnlyAnUnchangedPackIsTakenOnTrust is what makes carrying a verified pack
// forward a check rather than an assumption.
//
// Trusting a pack across a repair pass is trusting that the file did not change.
// Nothing this tool does writes into a pack, but "nothing does" is a claim about
// code and this is a claim about the file, so the trust is keyed to the file's
// size and its modification time.
func TestOnlyAnUnchangedPackIsTakenOnTrust(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) VerifiedPack {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		fi, err := os.Stat(path)
		require.NoError(t, err)
		return VerifiedPack{Path: path, Size: fi.Size(), ModTime: fi.ModTime()}
	}

	same := write("same.pack", "unchanged")
	grown := write("grown.pack", "short")
	rewritten := write("rewritten.pack", "same length")
	gone := write("gone.pack", "about to vanish")

	require.NoError(t, os.WriteFile(grown.Path, []byte("longer than before"), 0o644))
	// The same number of bytes, written a moment later.
	touched := time.Now().Add(time.Second)
	require.NoError(t, os.WriteFile(rewritten.Path, []byte("SAME LENGTH"), 0o644))
	require.NoError(t, os.Chtimes(rewritten.Path, touched, touched))
	require.NoError(t, os.Remove(gone.Path))

	s := &scanner{}
	s.trustUnchanged([]VerifiedPack{same, grown, rewritten, gone})

	assert.True(t, s.trusted[same.Path], "an untouched pack was read again for nothing")
	assert.False(t, s.trusted[grown.Path], "a pack that grew was taken on trust")
	assert.False(t, s.trusted[rewritten.Path], "a pack rewritten to the same size was taken on trust")
	assert.False(t, s.trusted[gone.Path], "a pack that is no longer there was taken on trust")

	// And what it trusts, it can hand on in turn.
	require.Len(t, s.verified, 1)
	assert.Equal(t, same.Path, s.verified[0].Path)
}

// TestAScanHandsOnWhatItWasTold covers the packs a scan never read itself: the
// caller's own fsck read them, and the next scan of the run must hear about them
// too, or the saving lasts only a single pass.
func TestAScanHandsOnWhatItWasTold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "told.pack")
	require.NoError(t, os.WriteFile(path, []byte("read by fsck"), 0o644))

	s := &scanner{}
	s.trustNamed([]string{path, filepath.Join(dir, "never-existed.pack")})

	assert.True(t, s.trusted[path])
	require.Len(t, s.verified, 1, "a pack the fsck read was not carried forward")
	assert.Equal(t, path, s.verified[0].Path)
	assert.Equal(t, int64(len("read by fsck")), s.verified[0].Size)
}
