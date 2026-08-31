package repair_test

// Every test here follows the same shape: record the whole repository, damage it, repair it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/repair"
)

// snapshot is everything about a repository that must survive a repair.
type snapshot struct {
	// log is every commit, with its tree and message.
	log string
	// trees is the full content of every commit's tree.
	trees map[string]string
	// refs is every reference and what it points at.
	refs string
	// blobs is the content of every blob, keyed by name.
	blobs map[string]string
}

// record reads everything a repair must preserve.
func record(t *testing.T, r *gittest.Repo) snapshot {
	t.Helper()
	s := snapshot{trees: map[string]string{}, blobs: map[string]string{}}
	s.log = r.Git("log", "--all", "--format=%H %T %P %s")
	s.refs = r.Git("show-ref")
	for _, line := range strings.Fields(r.Git("rev-list", "--all")) {
		s.trees[line] = r.Git("ls-tree", "-r", "-t", line)
	}
	for _, line := range strings.Split(r.Git("rev-list", "--objects", "--all"), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		if strings.TrimSpace(r.Git("cat-file", "-t", name)) == "blob" {
			s.blobs[name] = r.Git("cat-file", "blob", name)
		}
	}
	return s
}

// requireSame fails when anything the repository held has changed.
func requireSame(t *testing.T, want snapshot, r *gittest.Repo) {
	t.Helper()
	got := record(t, r)
	assert.Equal(t, want.log, got.log, "the history changed")
	assert.Equal(t, want.refs, got.refs, "the references changed")
	assert.Equal(t, want.trees, got.trees, "a tree changed")
	assert.Equal(t, want.blobs, got.blobs, "a blob's content changed")
}

// requireGitClean fails when the real git still finds something wrong.
func requireGitClean(t *testing.T, r *gittest.Repo) {
	t.Helper()
	res := r.GitFsck()
	assert.Equal(t, 0, res.Code, "git fsck is still unhappy:\n%s", res.Stderr+res.Stdout)
}

// fix runs a repair over the repository under test.
func fix(t *testing.T, r *gittest.Repo) *repair.Result {
	t.Helper()
	res, err := repair.Run(&repair.Options{
		Dir:    r.Dir,
		Run:    "test",
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	require.NoError(t, err)
	return res
}

// history builds a repository with a couple of commits and a nested directory, the
// smallest thing that exercises trees at more than a single depth.
func history(t *testing.T) *gittest.Repo {
	r := gittest.New(t)
	require.NoError(t, os.MkdirAll(filepath.Join(r.Dir, "src", "deep"), 0o777))
	for _, f := range [][2]string{
		{"a.txt", "one\n"},
		{"src/b.txt", "two\n"},
		{"src/deep/c.txt", "three\n"},
	} {
		r.Write(f[0], f[1])
	}
	r.Git("add", "-A")
	r.Git("commit", "-m", "one")
	r.Write("a.txt", "changed\n")
	r.Git("add", "-A")
	r.Git("commit", "-m", "two")
	return r
}

// looseObjects lists the loose object files a repository holds.
func looseObjects(t *testing.T, r *gittest.Repo) []string {
	t.Helper()
	var out []string
	objects := filepath.Join(r.GitDir(), "objects")
	for i := 0; i < 256; i++ {
		sub := filepath.Join(objects, strings.ToLower(hex2(i)))
		entries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			out = append(out, filepath.Join(sub, e.Name()))
		}
	}
	return out
}

func hex2(i int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[i>>4], digits[i&0xf]})
}

// TestRecoversHeadFromWorktreeAndIndex is the disaster this tool exists for:
// maintenance deleted objects the refs still need. Everything HEAD needs comes
// back, because the worktree holds the blobs and the index describes the trees.
func TestRecoversHeadFromWorktreeAndIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	head := strings.TrimSpace(r.Git("rev-parse", "HEAD"))
	tree := r.Git("ls-tree", "-r", "HEAD")

	// Delete every loose blob and tree the tip needs, which is what a bad gc leaves behind.
	deleted := 0
	for _, line := range strings.Split(r.Git("rev-list", "--objects", "HEAD"), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		typ := strings.TrimSpace(r.Git("cat-file", "-t", name))
		if typ != "blob" && typ != "tree" {
			continue
		}
		path := r.ObjectPath(mustOID(t, r, name))
		if err := os.Remove(path); err == nil {
			deleted++
		}
	}
	require.Positive(t, deleted, "the test deleted nothing, so it proves nothing")

	fix(t, r)

	assert.Equal(t, head, strings.TrimSpace(r.Git("rev-parse", "HEAD")), "HEAD moved")
	assert.Equal(t, tree, r.Git("ls-tree", "-r", "HEAD"), "the tip's tree changed")
	assert.Equal(t, "changed\n", r.Git("show", "HEAD:a.txt"))
	assert.Equal(t, "three\n", r.Git("show", "HEAD:src/deep/c.txt"))
}

// TestRecoversEverythingFromARemote deletes every object in the repository and
// requires the whole history back, byte for byte.
func TestRecoversEverythingFromARemote(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	upstream := t.TempDir()
	r.Git("init", "--bare", upstream)
	r.Git("remote", "add", "origin", upstream)
	r.Git("push", "origin", "HEAD:refs/heads/master")

	before := record(t, r)

	for _, path := range looseObjects(t, r) {
		require.NoError(t, os.Remove(path))
	}

	res := fix(t, r)
	assert.True(t, res.Ok(), "the repair did not finish: %+v", res.Unrecovered)
	assert.Empty(t, res.Unrecovered)
	requireGitClean(t, r)
	requireSame(t, before, r)
}

// TestReportsWhatNoSourceHas is the rule that a repository with a hole is
// reported, never trimmed to look healthy. Nothing may be deleted and no
// reference may move.
func TestReportsWhatNoSourceHas(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	before := record(t, r)

	// The older commit's tree has no local source: the index describes only the tip.
	old := strings.TrimSpace(r.Git("rev-parse", "HEAD~1^{tree}"))
	require.NoError(t, os.Remove(r.ObjectPath(mustOID(t, r, old))))

	res := fix(t, r)
	assert.False(t, res.Ok(), "a repository with a hole must not be reported as repaired")
	require.Len(t, res.Unrecovered, 1)
	assert.Equal(t, old, res.Unrecovered[0].OID.String())

	// The report must say where it is needed, or it is not actionable.
	assert.Contains(t, res.Unrecovered[0].Describe(), "needed by")

	// Nothing else may have changed: no ref wound back, no history rewritten.
	assert.Equal(t, before.log, r.Git("log", "--all", "--format=%H %T %P %s"))
	assert.Equal(t, before.refs, r.Git("show-ref"))
}

// TestQuarantineRatherThanDelete proves the no-delete rule holds for a file the
// repair had to move, and that undoing a run puts it back.
func TestQuarantineRatherThanDelete(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	// A corrupt loose blob whose content the worktree still has.
	blob := strings.TrimSpace(r.Git("rev-parse", "HEAD:src/b.txt"))
	path := r.ObjectPath(mustOID(t, r, blob))
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	gittest.WriteOver(t, path, []byte("not a zlib stream at all"))

	res := fix(t, r)
	assert.True(t, res.Ok(), "the blob was in the worktree and should have come back")
	assert.Equal(t, "two\n", r.Git("show", "HEAD:src/b.txt"))
	requireGitClean(t, r)

	// The corrupt file was moved, not removed.
	require.NotEmpty(t, res.Quarantine)
	quarantined := filepath.Join(res.Quarantine, "objects", blob[:2], blob[2:])
	kept, err := os.ReadFile(quarantined)
	require.NoError(t, err, "the corrupt file was deleted instead of quarantined")
	assert.Equal(t, "not a zlib stream at all", string(kept))

	// Undo puts the repository back exactly as the repair found it.
	require.NoError(t, os.Remove(path))
	restored, err := repair.Undo(r.GitDir(), "test")
	require.NoError(t, err)
	assert.NotEmpty(t, restored)
	now, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "not a zlib stream at all", string(now))
	assert.NotEqual(t, string(original), string(now))
}

// TestRecoversEachObjectOnce pins the order the repair works in. Writing the
// replacement before displacing the corrupt file leaves the quarantine holding
// the repaired object and the repository holding nothing, so the next pass
// recovers the same object again. That showed up as a doubled line in the
// report, and it meant the corrupt bytes were never actually kept.
func TestRecoversEachObjectOnce(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	// A commit has no local source, so this exercises the path where the replacement can only come from somewhere.
	blob := strings.TrimSpace(r.Git("rev-parse", "HEAD:a.txt"))
	path := r.ObjectPath(mustOID(t, r, blob))
	gittest.WriteOver(t, path, []byte("garbage"))

	res := fix(t, r)
	require.True(t, res.Ok())

	seen := map[string]int{}
	for _, rec := range res.Objects {
		seen[rec.OID.String()]++
	}
	for oid, n := range seen {
		assert.Equal(t, 1, n, "%s was recovered %d times, so a pass undid the one before it", oid, n)
	}

	// The corrupt bytes are the ones in quarantine, not the repaired object.
	kept, err := os.ReadFile(filepath.Join(res.Quarantine, "objects", blob[:2], blob[2:]))
	require.NoError(t, err)
	assert.Equal(t, "garbage", string(kept), "the quarantine holds the repaired object instead of the corrupt one")
}

// TestDanglingIsNotDamage confirms an unreachable object is ordinary, and a
// repair must neither report it nor remove it.
func TestDanglingIsNotDamage(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)

	// An amended commit leaves its predecessor dangling.
	r.Write("a.txt", "amended\n")
	r.Git("add", "-A")
	r.Git("commit", "--amend", "-m", "two, amended")
	dangling := r.Git("fsck", "--dangling", "--no-reflogs")
	require.Contains(t, dangling, "dangling", "the test needs a dangling object to be about anything")

	before := record(t, r)
	loose := looseObjects(t, r)

	res := fix(t, r)
	assert.True(t, res.Nothing(), "a repository whose only oddity is a dangling object needs no repair")
	assert.Empty(t, res.Objects)
	assert.Empty(t, res.Derived)

	// Every object file is still there.
	assert.ElementsMatch(t, loose, looseObjects(t, r), "a repair removed an object")
	requireSame(t, before, r)
}

// TestRebuildsADerivedFile displaces a corrupt commit-graph and requires that
// git is happy afterwards. The graph is a cache, so losing it costs nothing.
func TestRebuildsADerivedFile(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("commit-graph", "write", "--reachable")

	graph := filepath.Join(r.GitDir(), "objects", "info", "commit-graph")
	if _, err := os.Stat(graph); err != nil {
		t.Skipf("this git wrote no commit-graph: %v", err)
	}

	// Record before the damage.
	before := record(t, r)
	gittest.WriteOver(t, graph, []byte("XXXX not a commit graph"))

	res := fix(t, r)
	// The report names a file the way a person would recognise it, relative
	// to the git directory, not as the absolute path the run opened.
	assert.Contains(t, res.Derived, ".git/objects/info/commit-graph",
		"the corrupt graph was left in place")
	assert.NoFileExists(t, graph, "the graph should have been displaced")
	requireGitClean(t, r)
	requireSame(t, before, r)

	// It was displaced, not destroyed.
	assert.FileExists(t, filepath.Join(res.Quarantine, "objects", "info", "commit-graph"))
}

// TestRestoresABrokenRefFromItsReflog covers the ref fault that loses nothing:
// the ref file is unreadable but the object it named is still here.
func TestRestoresABrokenRefFromItsReflog(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	before := record(t, r)
	head := strings.TrimSpace(r.Git("rev-parse", "HEAD"))

	branch := filepath.Join(r.GitDir(), "refs", "heads", "master")
	if _, err := os.Stat(branch); err != nil {
		t.Skipf("this git packed the branch, so there is no loose ref to break: %v", err)
	}
	gittest.WriteOver(t, branch, []byte("this is not an object name\n"))

	res := fix(t, r)
	require.Len(t, res.Refs, 1, "the broken ref was not restored")
	assert.Equal(t, head, res.Refs[0].OID.String(), "the ref came back pointing somewhere else")
	requireGitClean(t, r)
	requireSame(t, before, r)
}

// TestCleanRepositoryIsLeftAlone is the case that must stay boring: a healthy
// repository is not touched, and no quarantine directory appears.
func TestCleanRepositoryIsLeftAlone(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	before := record(t, r)

	res := fix(t, r)
	assert.True(t, res.Nothing())
	assert.Empty(t, res.Quarantine)
	requireSame(t, before, r)
	assert.NoDirExists(t, filepath.Join(r.GitDir(), "git-fixed"))
}

// TestDamageThisToolDoesNotRepairIsStillReported is the honesty case. The scan
// looks for what this package can put back; fsck looks for everything. A
// repository whose damage falls outside what the scan covers must not be
// reported as healthy just because the repair had nothing to do.
func TestDamageThisToolDoesNotRepairIsStillReported(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("repack", "-ad")

	packs, err := filepath.Glob(filepath.Join(r.GitDir(), "objects", "pack", "*.pack"))
	require.NoError(t, err)
	require.NotEmpty(t, packs, "the test needs a pack to damage")

	// Truncate the pack.
	data, err := os.ReadFile(packs[0])
	require.NoError(t, err)
	gittest.WriteOver(t, packs[0], data[:len(data)/2])

	// This also pins termination.
	res := fix(t, r)

	assert.False(t, res.Ok(), "a repository git still refuses must not be reported as repaired")
	assert.False(t, res.Clean, "the pack is still truncated, so the repository is not whole")
	assert.False(t, res.Nothing(), "this must never read as a healthy repository")

	// Each object was attempted a single time overall, never repeated per pass.
	seen := map[string]int{}
	for _, rec := range res.Objects {
		seen[rec.OID.String()]++
	}
	for oid, n := range seen {
		assert.Equal(t, 1, n, "%s was recovered %d times: the run was not making progress", oid, n)
	}
}

// mustOID parses an object name the test just read back from git.
func mustOID(t *testing.T, r *gittest.Repo, hex string) gitobj.OID {
	t.Helper()
	oid, ok := r.Algo.Parse(strings.TrimSpace(hex))
	require.True(t, ok, "git printed an object name this test cannot parse: %q", hex)
	return oid
}
