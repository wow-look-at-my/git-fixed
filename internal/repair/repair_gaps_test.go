package repair_test

// The three faults that used to be reported and not repaired: a packfile that
// will not verify, an index that will not parse, and a packed-refs file git's
// own reader refuses.
//
// Each test follows the same shape as the ones next door: record the whole
// repository, damage it, repair it, then require that the real git is happy AND
// that every commit, tree, blob and ref came back byte for byte.

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

// overwrite replaces a file's content whatever mode it carries.
//
// git writes a packfile read-only, so a plain WriteFile over one fails for
// every user except root. Damaging a pack is exactly what these tests do.
func overwrite(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o644))
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// packFile finds the one packfile in a repository.
func packFile(t *testing.T, r *gittest.Repo) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(r.GitDir(), "objects", "pack", "*.pack"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "the test wants exactly one pack")
	return matches[0]
}

// packed builds a history with everything in one packfile, which is what a
// repository looks like after any ordinary maintenance run.
func packed(t *testing.T) *gittest.Repo {
	r := history(t)
	r.Git("repack", "-adq")
	require.Empty(t, looseObjects(t, r), "repack left loose objects behind")
	return r
}

// TestRepairsACorruptPackfile flips a byte in the middle of a pack, where an
// object's compressed data lives.
//
// This is the fault the tool used to loop on. A corrupt entry in a pack shadows
// every loose copy of the same object, because the database answers from packs
// first, so putting the object back changed nothing while the pack stayed. The
// repair writes out everything the pack still yields and then displaces it.
func TestRepairsACorruptPackfile(t *testing.T) {
	gittest.RequireGit(t)
	r := packed(t)
	before := record(t, r)
	pack := packFile(t, r)

	data, err := os.ReadFile(pack)
	require.NoError(t, err)
	// Past the twelve-byte header and before the trailing checksum, so the
	// damage lands on an object rather than on the frame around them.
	at := len(data) / 2
	data[at] ^= 0xff
	overwrite(t, pack, data)
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res := fix(t, r)

	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	assert.Len(t, res.Packs, 1, "the pack was not taken out")
	assert.Positive(t, res.Packs[0].Extracted, "nothing was written out of the pack")
	requireGitClean(t, r)
	requireSame(t, before, r)

	// The pack and its index went to quarantine together. An index left
	// behind describes a pack that is no longer there.
	assert.NoFileExists(t, pack)
	assert.NoFileExists(t, strings.TrimSuffix(pack, ".pack")+".idx")
}

// TestAPackThatYieldsNothingIsLeftAlone is the other half, and it was a real
// hole rather than a hypothetical one.
//
// A pack that yields no object at all used to be displaced anyway. Every object
// in it went with it, and the run only ever ended well because a remote happened
// to have the history. Moving such a pack buys nothing either: there is no loose
// copy for it to stop shadowing. So it stays where it is and the run says so.
//
// Two damage shapes reach that state. Destroying the body stops every entry from
// decoding; destroying the four-byte signature stops the read before the first
// entry, with every object still in the file byte for byte.
func TestAPackThatYieldsNothingIsLeftAlone(t *testing.T) {
	gittest.RequireGit(t)
	for _, tc := range []struct {
		name   string
		damage func(data []byte)
	}{
		{"body destroyed", func(data []byte) {
			body := data[12 : len(data)-20]
			for i := range body {
				body[i] ^= 0x5a
			}
		}},
		{"signature destroyed", func(data []byte) { copy(data[:4], "XXXX") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := packed(t)
			pack := packFile(t, r)
			data, err := os.ReadFile(pack)
			require.NoError(t, err)
			tc.damage(data)
			overwrite(t, pack, data)

			res := fix(t, r)

			assert.False(t, res.Ok(), "a pack nothing came out of is not a repair")
			assert.Empty(t, res.Packs, "the pack was displaced after yielding nothing")
			assert.NotEmpty(t, res.Refused, "leaving the pack alone was not reported")
			assert.FileExists(t, pack, "the pack was moved out of the repository")
		})
	}
}

// TestRebuildsAnUnparseableIndex truncates the index in the middle of its
// entries. Everything that still parses is kept, and HEAD supplies the rest.
func TestRebuildsAnUnparseableIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	before := record(t, r)
	wanted := r.Git("ls-files", "--stage")

	path := filepath.Join(r.GitDir(), "index")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	overwrite(t, path, data[:len(data)/2])
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res := fix(t, r)

	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	require.NotNil(t, res.Index)
	requireGitClean(t, r)
	requireSame(t, before, r)
	// Every tracked path is staged again, at the same mode and object.
	assert.Equal(t, wanted, r.Git("ls-files", "--stage"), "the rebuilt index lists something else")
	// The original is in quarantine, not deleted.
	assert.FileExists(t, filepath.Join(res.Quarantine, "index"))
}

// TestAnIndexRebuildKeepsStagedWork stages a file that is in no commit, then
// breaks the index below that entry. The entry itself still parses, so it must
// survive: rebuilding from HEAD alone would silently unstage it.
func TestAnIndexRebuildKeepsStagedWork(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Write("staged-only.txt", "never committed\n")
	r.Git("add", "staged-only.txt")
	staged := strings.TrimSpace(r.Git("rev-parse", ":staged-only.txt"))

	// Cut the checksum off. Every entry still parses; git refuses the file.
	path := filepath.Join(r.GitDir(), "index")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	overwrite(t, path, data[:len(data)-8])

	res := fix(t, r)

	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	assert.Contains(t, r.Git("ls-files"), "staged-only.txt", "the staged path was dropped")
	assert.Equal(t, staged, strings.TrimSpace(r.Git("rev-parse", ":staged-only.txt")),
		"the staged content changed")
}

// TestRewritesAMalformedPackedRefs puts a garbage line above a real reference.
// git's reader stops at the first line it refuses, so the branch below it
// disappears until the file is rewritten.
func TestRewritesAMalformedPackedRefs(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("branch", "keep-me")
	r.Git("tag", "-a", "v1", "-m", "a tag")
	r.Git("pack-refs", "--all")
	before := record(t, r)

	path := filepath.Join(r.GitDir(), "packed-refs")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	require.Greater(t, len(lines), 1, "pack-refs wrote nothing to damage")
	// After the header, so the references below it are the ones hidden.
	broken := append([]string{lines[0], "this is not a reference line"}, lines[1:]...)
	overwrite(t, path, []byte(strings.Join(broken, "\n")+"\n"))
	require.NotEqual(t, 0, r.GitFsck().Code, "the test did not break anything")

	res := fix(t, r)

	require.NotNil(t, res.PackedRefs)
	assert.Positive(t, res.PackedRefs.Kept, "no reference was carried over")
	requireGitClean(t, r)
	requireSame(t, before, r)
	assert.Contains(t, r.Git("show-ref"), "refs/heads/keep-me", "a branch was lost")
	assert.Contains(t, r.Git("show-ref"), "refs/tags/v1", "a tag was lost")
	assert.FileExists(t, filepath.Join(res.Quarantine, "packed-refs"))

	// The repository is whole and git is happy, and the run still does not
	// call itself Ok. One line named nothing this repository knows, and a
	// line like that MIGHT have been a branch. Saying so is the whole point:
	// a rewrite that quietly drops a line the owner cannot see is how a
	// branch disappears without anybody noticing.
	assert.True(t, res.Clean, "git fsck disagrees")
	assert.False(t, res.Ok(), "an unreadable line must be reported, not glossed over")
	assert.Equal(t, []string{"this is not a reference line"}, res.PackedRefs.Dropped)
}

// TestAPackedRefsRewriteRestoresAMangledLine damages the hash on a branch's
// line, leaving the name legible. The reflog still knows what the branch
// pointed at, so it comes back rather than being reported gone.
func TestAPackedRefsRewriteRestoresAMangledLine(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("branch", "keep-me")
	head := strings.TrimSpace(r.Git("rev-parse", "HEAD"))
	r.Git("pack-refs", "--all")

	path := filepath.Join(r.GitDir(), "packed-refs")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	mangled := strings.ReplaceAll(string(data), head+" refs/heads/keep-me", "zzz refs/heads/keep-me")
	require.NotEqual(t, string(data), mangled, "the test damaged nothing")
	overwrite(t, path, []byte(mangled))

	res := fix(t, r)

	require.NotNil(t, res.PackedRefs)
	assert.Len(t, res.PackedRefs.Restored, 1, "the mangled line was not restored")
	assert.Equal(t, head, strings.TrimSpace(r.Git("rev-parse", "refs/heads/keep-me")),
		"the branch came back pointing somewhere else")
	requireGitClean(t, r)
}

// TestAnIndexRebuiltFromHeadKeepsEveryMode cuts the index down to its header, so
// every entry has to come from HEAD's tree.
//
// An entry rebuilt that way has no stat data, and the mode lives INSIDE the stat
// block, so it is written there by hand. Getting that wrong is silent: git reads
// mode 000000 and the file stops being executable, or stops being a symlink.
func TestAnIndexRebuiltFromHeadKeepsEveryMode(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Write("run.sh", "#!/bin/sh\necho hi\n")
	require.NoError(t, os.Chmod(filepath.Join(r.Dir, "run.sh"), 0o755))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(r.Dir, "link.txt")))
	r.Git("add", "-A")
	r.Git("commit", "-m", "modes")
	wanted := r.Git("ls-files", "--stage")
	require.Contains(t, wanted, "100755", "the test wrote no executable file")
	require.Contains(t, wanted, "120000", "the test wrote no symlink")

	// Header only: the entry count still says how many there were, and not one
	// of them can be read, so HEAD supplies every path.
	path := filepath.Join(r.GitDir(), "index")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	overwrite(t, path, data[:12])

	res := fix(t, r)

	require.NotNil(t, res.Index)
	assert.Zero(t, res.Index.Salvaged, "the test left something salvageable")
	assert.Positive(t, res.Index.FromHead, "nothing came from HEAD")
	assert.Equal(t, wanted, r.Git("ls-files", "--stage"), "a mode did not survive")
	assert.Empty(t, r.Git("status", "--porcelain"), "the rebuilt index disagrees with the worktree")
}

// TestRepairsASHA256Repository runs all three container repairs at once over a
// repository whose object names are SHA-256.
//
// Every one of them reads or writes a hash-width-dependent format: the pack
// index, the index's fixed-size entry prefix, and packed-refs' line grammar. A
// width baked in anywhere would show up here and nowhere else, because every
// other test in this package uses the default.
func TestRepairsASHA256Repository(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.NewSHA256(t)
	require.NoError(t, os.MkdirAll(filepath.Join(r.Dir, "src"), 0o777))
	r.Write("a.txt", "one\n")
	r.Write("src/b.txt", "two\n")
	r.Git("add", "-A")
	r.Git("commit", "-m", "one")
	r.Git("branch", "keep-me")
	r.Git("tag", "-a", "v1", "-m", "a tag")
	r.Git("repack", "-adq")
	r.Git("pack-refs", "--all")
	before := record(t, r)
	staged := r.Git("ls-files", "--stage")

	// packed-refs and the index, but not the pack: damaging an object here
	// would have no source to come back from, and this test is about the
	// three container formats rather than about the recovery ladder.
	refs := filepath.Join(r.GitDir(), "packed-refs")
	data, err := os.ReadFile(refs)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	broken := append([]string{lines[0], "this is not a reference line"}, lines[1:]...)
	overwrite(t, refs, []byte(strings.Join(broken, "\n")+"\n"))

	idx := filepath.Join(r.GitDir(), "index")
	data, err = os.ReadFile(idx)
	require.NoError(t, err)
	overwrite(t, idx, data[:len(data)-8])

	res := fix(t, r)

	require.NotNil(t, res.PackedRefs)
	require.NotNil(t, res.Index)
	assert.True(t, res.Clean, "git fsck is still unhappy")
	requireGitClean(t, r)
	requireSame(t, before, r)
	assert.Equal(t, staged, r.Git("ls-files", "--stage"), "the rebuilt index lists something else")
	assert.Contains(t, r.Git("show-ref"), "refs/heads/keep-me", "a branch was lost")
	assert.Contains(t, r.Git("show-ref"), "refs/tags/v1", "a tag was lost")
}

// TestRepairsALinkedWorktreesIndex breaks the index of a linked worktree, which
// has its own HEAD and its own git directory while sharing the objects.
//
// It also pins how the report names that index. Paths are measured from the
// directory DisplayGitDir names, and measuring from the common one instead
// printed ".git/worktrees/w/worktrees/w/index" -- a path that does not exist.
func TestRepairsALinkedWorktreesIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := history(t)
	r.Git("branch", "side")
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-q", linked, "side")

	idx := filepath.Join(r.GitDir(), "worktrees", filepath.Base(linked), "index")
	require.FileExists(t, idx, "the worktree has no index of its own")
	data, err := os.ReadFile(idx)
	require.NoError(t, err)
	overwrite(t, idx, data[:len(data)-8])

	res, err := repair.Run(&repair.Options{Dir: linked, Run: "test", Stdout: os.Stdout, Stderr: os.Stderr})
	require.NoError(t, err)

	require.NotNil(t, res.Index)
	assert.True(t, res.Ok(), "the repair did not finish: %+v", res)
	assert.Equal(t, "worktrees/"+filepath.Base(linked)+"/index",
		strings.TrimPrefix(res.Index.Path, filepath.ToSlash(r.GitDir())+"/"),
		"the report named the index a path that does not exist")
	assert.FileExists(t, idx)
	requireGitClean(t, r)
}

// TestExtractionReplacesACorruptLooseCopy covers the branch that guards a real
// loss path.
//
// An object can be both loose and packed. When the pack is displaced, the loose
// copy is what survives -- so if that copy is corrupt and extraction skips it as
// "already here", the object is gone the moment the pack moves. The check has to
// read the loose file rather than trust that it exists.
//
// The pack is flagged by damaging its INDEX checksum, which leaves every entry
// decodable, so the only thing under test is what happens to the loose copy.
func TestExtractionReplacesACorruptLooseCopy(t *testing.T) {
	gittest.RequireGit(t)
	r := packed(t)
	before := record(t, r)

	// A blob that lives only in the pack, now given a corrupt loose copy too.
	blob := mustOID(t, r, strings.TrimSpace(r.Git("rev-parse", "HEAD:src/deep/c.txt")))
	r.WriteObjectFile(blob, []byte("this is not a zlib stream"))
	require.FileExists(t, r.ObjectPath(blob))

	idx := strings.TrimSuffix(packFile(t, r), ".pack") + ".idx"
	data, err := os.ReadFile(idx)
	require.NoError(t, err)
	// The last twenty bytes are the index's own checksum. Breaking it fails
	// the pack without touching a single entry.
	data[len(data)-1] ^= 0xff
	overwrite(t, idx, data)

	res := fix(t, r)

	require.True(t, res.Ok(), "the repair did not finish: %+v", res)
	requireGitClean(t, r)
	requireSame(t, before, r)
	assert.Equal(t, "three\n", r.Git("show", "HEAD:src/deep/c.txt"),
		"the corrupt loose copy outlived the pack")
	assert.FileExists(t, filepath.Join(res.Quarantine, "objects",
		blob.String()[:2], blob.String()[2:]),
		"the corrupt loose copy was not kept")
}
