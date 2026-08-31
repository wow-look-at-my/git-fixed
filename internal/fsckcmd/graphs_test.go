package fsckcmd_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
)

// history writes n commits, each touching the same file.
func history(t *testing.T, r *gittest.Repo, n int) {
	t.Helper()
	for i := range n {
		r.Write("f", strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "c"+strconv.Itoa(i))
	}
}

// onlyFile returns the single file matching a glob, failing unless the glob
// matches exactly a single file.
func onlyFile(t *testing.T, glob string) string {
	t.Helper()
	names, err := filepath.Glob(glob)
	require.NoError(t, err)
	require.Len(t, names, 1, "expected exactly one %s", glob)
	return names[0]
}

// flipLastByte corrupts a file by inverting its final byte, which lands inside
// the trailing checksum of every format here.
func flipLastByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	gittest.WriteOver(t, path, data)
}

func TestPackRevIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	history(t, r, 4)
	r.Git("-c", "pack.writeReverseIndex=true", "repack", "-adq")
	rev := onlyFile(t, filepath.Join(r.GitDir(), "objects", "pack", "*.rev"))
	sameAsGit(t, r)

	t.Run("corrupt checksum", func(t *testing.T) {
		flipLastByte(t, rev)
		got := ours(t, r.Dir)
		want := r.GitFsck()
		assert.Equal(t, want.Code, got.Code)
		assert.Equal(t, want.Lines(), got.Lines())
	})

	t.Run("truncated", func(t *testing.T) {
		gittest.WriteOver(t, rev, []byte("RIDX"))
		got := ours(t, r.Dir)
		want := r.GitFsck()
		assert.Equal(t, want.Code, got.Code)
		assert.Equal(t, want.Lines(), got.Lines())
	})

	t.Run("bad signature", func(t *testing.T) {
		r2 := gittest.New(t)
		history(t, r2, 3)
		r2.Git("-c", "pack.writeReverseIndex=true", "repack", "-adq")
		path := onlyFile(t, filepath.Join(r2.GitDir(), "objects", "pack", "*.rev"))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		copy(data, "XXXX")
		gittest.WriteOver(t, path, data)
		got := ours(t, r2.Dir)
		want := r2.GitFsck()
		assert.Equal(t, want.Code, got.Code)
		assert.Equal(t, want.Lines(), got.Lines())
	})
}

func TestCommitGraphTruncated(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	history(t, r, 4)
	r.Git("commit-graph", "write", "--reachable")
	path := filepath.Join(r.GitDir(), "objects", "info", "commit-graph")
	gittest.WriteOver(t, path, []byte("CGPH"))
	got := ours(t, r.Dir)
	want := r.GitFsck()
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Lines(), got.Lines())
}

func TestCommitGraphMissingCommit(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	history(t, r, 4)
	r.Git("commit-graph", "write", "--reachable")
	// Dropping the objects the graph indexes is the failure the graph check exists to catch.
	head := r.Git("rev-parse", "HEAD")
	r.Git("update-ref", "-d", "refs/heads/master")
	r.Git("update-ref", "-d", "refs/heads/main")
	require.NotEmpty(t, head)
	got := ours(t, r.Dir)
	want := r.GitFsck()
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Lines(), got.Lines())
}

func TestMultiPackIndexTruncated(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	for i := range 3 {
		r.Write("f", strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "c"+strconv.Itoa(i))
		r.Git("repack", "-dq")
	}
	r.Git("multi-pack-index", "write")
	path := filepath.Join(r.GitDir(), "objects", "pack", "multi-pack-index")
	gittest.WriteOver(t, path, []byte("MIDX"))
	got := ours(t, r.Dir)
	want := r.GitFsck()
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Lines(), got.Lines())
}
