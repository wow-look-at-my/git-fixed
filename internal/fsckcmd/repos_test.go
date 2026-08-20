package fsckcmd_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
)

func TestIndexIsAHead(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.Write("staged", "staged content\n")
	r.Git("add", "staged")
	sameAsGit(t, r)
	sameAsGit(t, r, "--cache")
	sameAsGit(t, r, "--unreachable")
}

func TestCacheTreeAndResolveUndo(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Git("write-tree")
	sameAsGit(t, r)
	sameAsGit(t, r, "--cache")
}

func TestLinkedWorktree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Git("branch", "side")
	r.Git("worktree", "add", "-q", filepath.Join(r.Dir, "..", "wt"), "side")
	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable")
}

func TestAlternateObjectDirectory(t *testing.T) {
	gittest.RequireGit(t)
	donor := gittest.New(t)
	_, _, commit := donor.SimpleHistory()

	r := gittest.New(t)
	alts := filepath.Join(r.GitDir(), "objects", "info", "alternates")
	require.NoError(t, os.MkdirAll(filepath.Dir(alts), 0o777))
	require.NoError(t, os.WriteFile(alts, []byte(filepath.Join(donor.GitDir(), "objects")+"\n"), 0o666))
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	sameAsGit(t, r, "--no-full")
}

func TestReflogEntriesAreHeads(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Write("f", "two\n")
	r.Git("commit", "-qam", "two")
	r.Git("reset", "-q", "--hard", "HEAD~1")
	// The dropped commit is still reachable from the reflog.
	sameAsGit(t, r)
	sameAsGit(t, r, "--no-reflogs")
	sameAsGit(t, r, "--unreachable", "--no-reflogs")
}

func TestDetachedHead(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	_, _, commit := r.SimpleHistory()
	require.NoError(t, os.WriteFile(filepath.Join(r.GitDir(), "HEAD"), []byte(commit.String()+"\n"), 0o666))
	sameAsGit(t, r)
}

func TestHeadPointsAtNothing(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	require.NoError(t, os.WriteFile(filepath.Join(r.GitDir(), "HEAD"),
		[]byte(gitobj.SHA1.Null().String()+"\n"), 0o666))
	sameAsGit(t, r)
}

func TestHeadPointsSomewhereStrange(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.SetHEAD("refs/strange/place")
	sameAsGit(t, r)
}

func TestBranchRefIsNotACommit(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob, _, _ := r.SimpleHistory()
	r.UpdateRef("refs/heads/blobby", blob)
	sameAsGit(t, r)
}

func TestPackedRefs(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Git("tag", "v1")
	r.Git("pack-refs", "--all")
	sameAsGit(t, r)
	sameAsGit(t, r, "--tags")
}

func TestDeltaChainsInPack(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	base := strings.Repeat("a line of text that compresses and deltas well\n", 200)
	for i := 0; i < 40; i++ {
		r.Write("f", base+strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "revision "+strconv.Itoa(i))
	}
	r.Git("repack", "-adq", "--depth=50", "--window=50")
	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable")
}

func TestCorruptPackData(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", strings.Repeat("content\n", 100))
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Git("repack", "-adq")
	packs, err := filepath.Glob(filepath.Join(r.GitDir(), "objects", "pack", "*.pack"))
	require.NoError(t, err)
	require.Len(t, packs, 1)
	data, err := os.ReadFile(packs[0])
	require.NoError(t, err)
	// Flip a byte inside the first object's compressed data.
	data[20] ^= 0xff
	gittest.WriteOver(t, packs[0], data)
	got := sameAsGit(t, r)
	assert.NotEqual(t, 0, got.Code, "a corrupt pack must not report success")
	assert.NotEmpty(t, corruptObjects(got), "the report must name the object")
}

// corruptObjects picks the object names out of a report about a broken pack.
func corruptObjects(res gittest.Result) []string {
	var out []string
	for _, line := range res.Lines() {
		if strings.Contains(line, "is corrupt") || strings.Contains(line, "cannot unpack") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if len(f) == 40 {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

func TestTruncatedPackIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	r.Git("repack", "-adq")
	idxs, err := filepath.Glob(filepath.Join(r.GitDir(), "objects", "pack", "*.idx"))
	require.NoError(t, err)
	require.Len(t, idxs, 1)
	data, err := os.ReadFile(idxs[0])
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	gittest.WriteOver(t, idxs[0], data)
	got := ours(t, r.Dir)
	want := r.GitFsck()
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Lines(), got.Lines())
}

func TestCommitGraph(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	for i := 0; i < 5; i++ {
		r.Write("f", strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "c"+strconv.Itoa(i))
	}
	r.Git("commit-graph", "write", "--reachable")
	require.FileExists(t, filepath.Join(r.GitDir(), "objects", "info", "commit-graph"))
	sameAsGit(t, r)

	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(r.GitDir(), "objects", "info", "commit-graph")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[len(data)-1] ^= 0xff
		gittest.WriteOver(t, path, data)
		got := ours(t, r.Dir)
		want := r.GitFsck()
		assert.Equal(t, want.Code, got.Code)
		assert.Equal(t, want.Lines(), got.Lines())
	})
}

func TestMultiPackIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	for i := 0; i < 3; i++ {
		r.Write("f", strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "c"+strconv.Itoa(i))
		r.Git("repack", "-dq")
	}
	r.Git("multi-pack-index", "write")
	require.FileExists(t, filepath.Join(r.GitDir(), "objects", "pack", "multi-pack-index"))
	sameAsGit(t, r)

	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(r.GitDir(), "objects", "pack", "multi-pack-index")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[len(data)-1] ^= 0xff
		gittest.WriteOver(t, path, data)
		got := ours(t, r.Dir)
		want := r.GitFsck()
		assert.Equal(t, want.Code, got.Code)
	})
}

func TestSHA256Repository(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.NewSHA256(t)
	r.Write("f", "one\n")
	r.Git("add", "f")
	r.Git("commit", "-qm", "one")
	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable", "--root")
	r.Git("repack", "-adq")
	sameAsGit(t, r)
}

func TestGitmodulesChecks(t *testing.T) {
	gittest.RequireGit(t)
	cases := []struct {
		name    string
		content string
	}{
		{"clean", "[submodule \"ok\"]\n\tpath = sub\n\turl = https://example.com/sub.git\n"},
		{"bad name", "[submodule \"../evil\"]\n\turl = https://example.com/sub.git\n"},
		{"bad url", "[submodule \"ok\"]\n\turl = --upload-pack=evil\n"},
		{"bad path", "[submodule \"ok\"]\n\tpath = --evil\n\turl = https://example.com/s.git\n"},
		{"command update", "[submodule \"ok\"]\n\turl = https://example.com/s.git\n\tupdate = !evil\n"},
		{"unparsable", "[submodule \"ok\"\n\turl = https://example.com/s.git\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gittest.New(t)
			blob := r.Blob(tc.content)
			tree := r.WriteRaw("tree", gittest.Tree(
				gittest.TreeEntry{Mode: "100644", Name: ".gitmodules", OID: blob}))
			commit := r.Commit(tree, nil, "modules")
			r.UpdateRef("refs/heads/master", commit)
			r.SetHEAD("refs/heads/master")
			sameAsGit(t, r)
			sameAsGit(t, r, "--strict")
		})
	}
}

func TestSkipList(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: ".git", OID: blob}))
	commit := r.Commit(tree, nil, "sneaky")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	skip := filepath.Join(r.Dir, "skiplist")
	require.NoError(t, os.WriteFile(skip, []byte(tree.String()+"\n"), 0o666))
	r.Git("config", "fsck.skipList", skip)
	r.Git("config", "fsck.hasDotgit", "error")
	sameAsGit(t, r)
}

func TestFsckConfigSeverity(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: ".git", OID: blob}))
	commit := r.Commit(tree, nil, "sneaky")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")

	r.Git("config", "fsck.hasDotgit", "ignore")
	sameAsGit(t, r)
	r.Git("config", "fsck.hasDotgit", "error")
	sameAsGit(t, r)
	r.Git("config", "fsck.hasDotgit", "warn")
	sameAsGit(t, r)
}

func TestLostFound(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	_, tree, _ := r.SimpleHistory()
	r.Blob("orphan\n")
	r.Commit(tree, nil, "orphan commit")
	res := ours(t, r.Dir, "--lost-found")
	require.Equal(t, 0, res.Code)
	entries, err := filepath.Glob(filepath.Join(r.GitDir(), "lost-found", "*", "*"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "--lost-found must write the dangling objects out")
}

func TestNamedObjectPaths(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("deep\n")
	inner := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: "file", OID: blob}))
	outer := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "40000", Name: "dir", OID: inner}))
	first := r.Commit(outer, nil, "one")
	second := r.Commit(outer, []gitobj.OID{first}, "two")
	r.UpdateRef("refs/heads/master", second)
	r.SetHEAD("refs/heads/master")
	r.Delete(blob)
	sameAsGit(t, r, "--name-objects")
}

func TestObjectArguments(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	_, tree, commit := r.SimpleHistory()
	sameAsGit(t, r, commit.String())
	sameAsGit(t, r, tree.String())
	sameAsGit(t, r, gitobj.SHA1.Null().String())
}

func TestWorkerCountsAgree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	for i := 0; i < 30; i++ {
		r.Write("f", strconv.Itoa(i)+"\n")
		r.Git("add", "f")
		r.Git("commit", "-qm", "c"+strconv.Itoa(i))
	}
	r.Git("repack", "-adq")
	want := r.GitFsck().Lines()
	for _, workers := range []int{1, 2, 8, 64} {
		o := defaultTestOptions(t, r.Dir)
		o.Workers = workers
		got := runWith(o)
		assert.Equal(t, want, got.Lines(), "output changed with %d workers", workers)
	}
}
