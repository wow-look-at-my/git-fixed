package fsckcmd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/fsckcmd"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
)

// ours runs this implementation over a repository and collects what it printed.
func ours(t *testing.T, dir string, args ...string) gittest.Result {
	t.Helper()
	o := fsckcmd.DefaultOptions()
	var out, errBuf bytes.Buffer
	o.Stdout = &out
	o.Stderr = &errBuf
	o.Dir = dir
	o.Workers = 4
	rest := applyFlags(t, o, args)
	o.Args = rest
	code := fsckcmd.Run(o)
	return gittest.Result{Stdout: out.String(), Stderr: errBuf.String(), Code: code}
}

// applyFlags sets the options a test names, using the same spellings git takes.
func applyFlags(t *testing.T, o *fsckcmd.Options, args []string) []string {
	t.Helper()
	var rest []string
	for _, a := range args {
		switch a {
		case "--strict":
			o.Strict = true
		case "--unreachable":
			o.ShowUnreachable = true
		case "--no-dangling":
			o.ShowDangling = false
		case "--dangling":
			o.ShowDangling = true
		case "--root":
			o.ShowRoot = true
		case "--tags":
			o.ShowTags = true
		case "--cache":
			o.KeepCacheObjects = true
		case "--no-reflogs":
			o.IncludeReflogs = false
		case "--connectivity-only":
			o.ConnectivityOnly = true
		case "--name-objects":
			o.NameObjects = true
		case "--no-full":
			o.CheckFull = false
		case "--lost-found":
			o.WriteLostFound = true
			o.CheckFull = true
			o.IncludeReflogs = false
		default:
			require.False(t, strings.HasPrefix(a, "-"))

			rest = append(rest, a)
		}
	}
	return rest
}

// sameAsGit runs both implementations and requires that they agree.
func sameAsGit(t *testing.T, r *gittest.Repo, args ...string) gittest.Result {
	t.Helper()
	want := r.GitFsck(args...)
	got := ours(t, r.Dir, args...)
	assert.Equal(t, want.Lines(), got.Lines(), "output differs from git fsck %s", strings.Join(args, " "))
	assert.Equal(t, want.Code, got.Code, "exit status differs from git fsck %s", strings.Join(args, " "))
	return got
}

func TestCleanRepository(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	sameAsGit(t, r)
	sameAsGit(t, r, "--strict")
	sameAsGit(t, r, "--root", "--tags", "--unreachable", "--dangling")
	sameAsGit(t, r, "--connectivity-only")
	sameAsGit(t, r, "--name-objects")
}

func TestEmptyRepository(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable")
}

func TestMissingBlob(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob, _, _ := r.SimpleHistory()
	r.Delete(blob)
	res := sameAsGit(t, r)
	require.Equal(t, 2, res.Code)
	sameAsGit(t, r, "--name-objects")
	sameAsGit(t, r, "--connectivity-only")
}

func TestDanglingObjects(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	_, tree, _ := r.SimpleHistory()
	r.Blob("loose and unreferenced\n")
	r.Commit(tree, nil, "dangling")

	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable")
	sameAsGit(t, r, "--no-dangling")
	sameAsGit(t, r, "--root", "--tags")
}

func TestDotGitInTree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: ".git", OID: blob}))
	commit := r.Commit(tree, nil, "sneaky")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	res := sameAsGit(t, r, "--strict")
	require.Equal(t, 1, res.Code)
}

func TestZeroPaddedFileMode(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "0100644", Name: "f", OID: blob}))
	commit := r.Commit(tree, nil, "padded")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	sameAsGit(t, r, "--strict")
}

func TestBadFileMode(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100664", Name: "f", OID: blob}))
	commit := r.Commit(tree, nil, "odd mode")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	sameAsGit(t, r, "--strict")
}

func TestUnsortedTree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(
		gittest.TreeEntry{Mode: "100644", Name: "b", OID: blob},
		gittest.TreeEntry{Mode: "100644", Name: "a", OID: blob},
	))
	commit := r.Commit(tree, nil, "unsorted")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	sameAsGit(t, r, "--strict")
}

func TestDuplicateTreeEntries(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("payload\n")
	tree := r.WriteRaw("tree", gittest.Tree(
		gittest.TreeEntry{Mode: "100644", Name: "a", OID: blob},
		gittest.TreeEntry{Mode: "100644", Name: "a", OID: blob},
	))
	commit := r.Commit(tree, nil, "duplicate")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
}

func TestNullSHA1InTree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	tree := r.WriteRaw("tree", gittest.Tree(
		gittest.TreeEntry{Mode: "100644", Name: "f", OID: gitobj.SHA1.Null()},
	))
	commit := r.Commit(tree, nil, "null entry")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
}

func TestCorruptLooseObject(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob, _, _ := r.SimpleHistory()
	r.Overwrite(blob, []byte("this is not a zlib stream"))
	sameAsGit(t, r)
}

func TestHashPathMismatch(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob, _, _ := r.SimpleHistory()
	// Store a different payload under the blob's original name.
	var body bytes.Buffer
	body.WriteString("blob 6\x00")
	body.WriteString("wrong\n")
	r.WriteLooseBytes(blob, body.Bytes())
	sameAsGit(t, r)
}

func TestCommitWithoutAuthor(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	blob := r.Blob("hello\n")
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: "f", OID: blob}))
	commit := r.WriteRaw("commit", []byte(
		"tree "+tree.String()+"\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n\nmsg\n"))
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
}

func TestCommitBadIdentities(t *testing.T) {
	gittest.RequireGit(t)
	cases := []struct {
		name  string
		lines string
	}{
		{"missing email", "author A U Thor 1700000000 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"missing space before email", "author A U Thor<author@example.com> 1700000000 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"bad name", "author >bad< <author@example.com> 1700000000 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"bad date", "author A U Thor <author@example.com> notanumber +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"zero padded date", "author A U Thor <author@example.com> 0700000000 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"date overflow", "author A U Thor <author@example.com> 99999999999999999999 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"bad timezone", "author A U Thor <author@example.com> 1700000000 +00\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
		{"missing committer", "author A U Thor <author@example.com> 1700000000 +0000\n"},
		{"multiple authors", "author A U Thor <author@example.com> 1700000000 +0000\nauthor A U Thor <author@example.com> 1700000000 +0000\ncommitter C O Mitter <committer@example.com> 1700000000 +0000\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gittest.New(t)
			blob := r.Blob("hello\n")
			tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: "f", OID: blob}))
			commit := r.WriteRaw("commit", []byte("tree "+tree.String()+"\n"+tc.lines+"\nmsg\n"))
			r.UpdateRef("refs/heads/master", commit)
			r.SetHEAD("refs/heads/master")
			sameAsGit(t, r)
		})
	}
}

func TestTagChecks(t *testing.T) {
	gittest.RequireGit(t)
	t.Run("valid", func(t *testing.T) {
		r := gittest.New(t)
		_, _, commit := r.SimpleHistory()
		tag := r.Tag(commit, "commit", "v1", "release")
		r.UpdateRef("refs/tags/v1", tag)
		sameAsGit(t, r)
		sameAsGit(t, r, "--tags")
	})
	t.Run("bad name", func(t *testing.T) {
		r := gittest.New(t)
		_, _, commit := r.SimpleHistory()
		tag := r.Tag(commit, "commit", "has space", "release")
		r.UpdateRef("refs/tags/v1", tag)
		sameAsGit(t, r)
		sameAsGit(t, r, "--strict")
	})
	t.Run("missing tagger", func(t *testing.T) {
		r := gittest.New(t)
		_, _, commit := r.SimpleHistory()
		tag := r.WriteRaw("tag", []byte("object "+commit.String()+"\ntype commit\ntag v1\n\nmsg\n"))
		r.UpdateRef("refs/tags/v1", tag)
		sameAsGit(t, r)
	})
	t.Run("bad type", func(t *testing.T) {
		r := gittest.New(t)
		_, _, commit := r.SimpleHistory()
		tag := r.WriteRaw("tag", []byte("object "+commit.String()+
			"\ntype bogus\ntag v1\ntagger C O Mitter <committer@example.com> 1700000000 +0000\n\nmsg\n"))
		r.UpdateRef("refs/tags/v1", tag)
		sameAsGit(t, r)
	})
}

func TestPackedRepository(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	for i := 0; i < 20; i++ {
		r.Blob(strings.Repeat("filler ", i+1))
	}
	r.Git("repack", "-adq")
	sameAsGit(t, r)
	sameAsGit(t, r, "--unreachable")
	sameAsGit(t, r, "--connectivity-only")
}

// TestBigFileThreshold covers the blob git hashes by streaming rather than
// reading into memory. It has to stay a differential test: the streamed object
// reaches the checks with no payload, so a mistake here shows up as a missing
// or extra line rather than as a crash.
func TestBigFileThreshold(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.Git("config", "core.bigFileThreshold", "1k")
	big := r.Blob(strings.Repeat("payload\n", 512))
	tree := r.WriteRaw("tree", gittest.Tree(gittest.TreeEntry{Mode: "100644", Name: "big", OID: big}))
	commit := r.Commit(tree, nil, "one big file")
	r.UpdateRef("refs/heads/master", commit)
	r.SetHEAD("refs/heads/master")
	sameAsGit(t, r)
	r.Git("repack", "-adq")
	sameAsGit(t, r)
	sameAsGit(t, r, "--strict")
}

func TestUnknownObjectType(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.WriteRaw("bogus", []byte("content"))
	sameAsGit(t, r)
}

func TestBrokenRef(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	_, _, commit := r.SimpleHistory()
	missing := r.Commit(gitobj.SHA1.EmptyTree, []gitobj.OID{commit}, "deleted later")
	r.UpdateRef("refs/heads/broken", missing)
	r.Delete(missing)
	sameAsGit(t, r)
}

// defaultTestOptions returns options that write into buffers instead of the
// process's own streams.
func defaultTestOptions(t *testing.T, dir string) *fsckcmd.Options {
	t.Helper()
	o := fsckcmd.DefaultOptions()
	o.Dir = dir
	o.Stdout = &bytes.Buffer{}
	o.Stderr = &bytes.Buffer{}
	return o
}

// runWith runs a configured check and collects its output.
func runWith(o *fsckcmd.Options) gittest.Result {
	code := fsckcmd.Run(o)
	return gittest.Result{
		Stdout: o.Stdout.(*bytes.Buffer).String(),
		Stderr: o.Stderr.(*bytes.Buffer).String(),
		Code:   code,
	}
}

// TestIndexWithACacheTree covers the index a real checkout has.
func TestIndexWithACacheTree(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.Git("read-tree", "HEAD")
	r.Blob("loose and unreferenced\n")

	sameAsGit(t, r)
	sameAsGit(t, r, "--connectivity-only")
	sameAsGit(t, r, "--cache")
	sameAsGit(t, r, "--name-objects")
	sameAsGit(t, r, "--strict")
	sameAsGit(t, r, "--unreachable")
}
