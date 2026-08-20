package fsckcmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
)

// writeRef puts raw bytes in a ref file, which is how a test builds content git
// itself would never write.
func writeRef(t *testing.T, r *gittest.Repo, name, content string) {
	t.Helper()
	path := filepath.Join(r.GitDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o777))
	gittest.WriteOver(t, path, []byte(content))
}

// TestRefContentChecks covers the checks git runs over the ref files
// themselves. Each case is one message id from "git refs verify".
func TestRefContentChecks(t *testing.T) {
	gittest.RequireGit(t)
	for _, c := range []struct {
		name    string
		refname string
		content string
	}{
		{"badRefOid", "refs/heads/bad", "0000000000000000000000000000000000000000\n"},
		{"badRefContent", "refs/heads/bad", "not an object name\n"},
		{"emptyRefFile", "refs/heads/bad", ""},
		{"refMissingNewline", "refs/heads/bare", "%s"},
		{"trailingRefContent", "refs/heads/extra", "%s garbage\n"},
		{"trailingNewlines", "refs/heads/extra", "%s\n\n"},
		{"symrefMissingNewline", "refs/heads/sym", "ref: refs/heads/master"},
		{"symrefTrailingContent", "refs/heads/sym", "ref: refs/heads/master  \n"},
		{"badReferentName", "refs/heads/sym", "ref: refs/heads/bad..name\n"},
		{"symrefTargetIsNotARef", "refs/heads/sym", "ref: not-a-ref\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := gittest.New(t)
			_, _, commit := r.SimpleHistory()
			writeRef(t, r, c.refname, strings.ReplaceAll(c.content, "%s", commit.String()))
			sameAsGit(t, r)
		})
	}
}

// TestRefNameChecks covers a ref whose own name is not one a ref may have.
func TestRefNameChecks(t *testing.T) {
	gittest.RequireGit(t)
	for _, name := range []string{"refs/heads/bad name", "refs/heads/bad..name", "refs/heads/.hidden"} {
		t.Run(name, func(t *testing.T) {
			r := gittest.New(t)
			_, _, commit := r.SimpleHistory()
			writeRef(t, r, name, commit.String()+"\n")
			sameAsGit(t, r)
		})
	}
}

// TestSymlinkRef covers a symref stored the deprecated way, as a symbolic link.
func TestSymlinkRef(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	link := filepath.Join(r.GitDir(), "refs", "heads", "link")
	require.NoError(t, os.Symlink(filepath.Join(r.GitDir(), "refs", "heads", "master"), link))
	sameAsGit(t, r)
}

// TestRefFiletype covers a ref that is neither a regular file nor a symlink.
func TestRefFiletype(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	path := filepath.Join(r.GitDir(), "refs", "heads", "fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o666))
	sameAsGit(t, r)
}

// TestPackedRefsChecks covers the packed-refs file, which carries its own set
// of message ids.
func TestPackedRefsChecks(t *testing.T) {
	gittest.RequireGit(t)
	for _, c := range []struct {
		name    string
		content string
	}{
		{"emptyPackedRefsFile", ""},
		{"badPackedRefHeader", "# not the header git writes\n"},
		{"badPackedRefEntry", "not-an-oid refs/heads/master\n"},
		{"noSpaceAfterOid", "%s\n"},
		{"packedRefEntryNotTerminated", "%s refs/heads/master"},
		{"badRefNameInEntry", "%s refs/heads/bad name\n"},
		{"badPeeledOid", "%s refs/tags/v1\n^not-an-oid\n"},
		{"unsorted", "# pack-refs with: peeled fully-peeled sorted \n%s refs/heads/zzz\n%s refs/heads/aaa\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := gittest.New(t)
			_, _, commit := r.SimpleHistory()
			content := strings.ReplaceAll(c.content, "%s", commit.String())
			gittest.WriteOver(t, filepath.Join(r.GitDir(), "packed-refs"), []byte(content))
			sameAsGit(t, r)
		})
	}
}
