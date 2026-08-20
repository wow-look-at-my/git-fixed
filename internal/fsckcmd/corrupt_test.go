package fsckcmd_test

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gittest"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/go-containers/set"
)

// TestCorruptLooseObjects is the oracle for internal/zlibmsg. git links the
// real zlib and prints its complaint, so a repository full of loose objects
// broken one byte at a time compares this implementation's whole report against
// zlib's own vocabulary, without having to name any of the messages here.
//
// The payloads make zlib choose a different block type for each one, so a
// corruption lands in a stored block, in the built-in alphabets, and in a
// block's own alphabets.
func TestCorruptLooseObjects(t *testing.T) {
	gittest.RequireGit(t)
	source := rand.New(rand.NewSource(1)) //nolint:gosec // a fixture, not a secret
	random := make([]byte, 400)
	_, err := source.Read(random)
	require.NoError(t, err)

	payloads := map[string][]byte{
		"tiny":         []byte("x"),
		"repetitive":   bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 40),
		"incompressib": random,
	}

	r := gittest.New(t)
	r.SimpleHistory()
	written := 0
	for name, payload := range payloads {
		for off := range 40 {
			for _, value := range []byte{0x00, 0x55, 0xff} {
				// The marker gives every variant its own object
				// name, so one repository holds all of them.
				marked := append([]byte(fmt.Sprintf("%s/%d/%d\n", name, off, value)), payload...)
				oid, raw := looseObject(t, marked)
				if off >= len(raw) || raw[off] == value {
					continue
				}
				raw[off] = value
				r.WriteObjectFile(oid, raw)
				written++
			}
		}
	}
	require.NotZero(t, written)
	t.Logf("%d corrupted loose objects", written)

	// A whole-slice comparison of thousands of lines is unreadable, so
	// report only the lines one side has and the other does not.
	want := r.GitFsck()
	got := ours(t, r.Dir)
	assert.Equal(t, want.Code, got.Code, "exit status differs from git fsck")
	missing, extra := difference(want.Lines(), got.Lines())
	assert.Empty(t, missing, "lines git printed and this did not")
	assert.Empty(t, extra, "lines this printed and git did not")
	t.Logf("%d lines agreed", len(want.Lines()))
}

// difference returns the lines only want holds and the lines only got holds. It
// keeps at most a few of each, because a report of every one is unreadable.
func difference(want, got []string) (missing, extra []string) {
	const show = 8
	inGot := set.New[string](len(got))
	inGot.AddRange(got...)
	inWant := set.New[string](len(want))
	inWant.AddRange(want...)
	for _, line := range want {
		if !inGot.Contains(line) && len(missing) < show {
			missing = append(missing, line)
		}
	}
	for _, line := range got {
		if !inWant.Contains(line) && len(extra) < show {
			extra = append(extra, line)
		}
	}
	return missing, extra
}

// TestCorruptPackBytes breaks one byte of a packfile at a time and requires the
// whole report to match git's. A corrupt pack produces several different
// reports depending on where the damage is: the pack's own checksum, an
// entry's CRC, the complaint zlib makes, the entry that will not decode, and
// the death of whoever reads that object by name afterwards.
func TestCorruptPackBytes(t *testing.T) {
	gittest.RequireGit(t)
	for _, part := range []int{2, 5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 95} {
		t.Run(fmt.Sprintf("at%d%%", part), func(t *testing.T) {
			r := gittest.New(t)
			for i := range 4 {
				r.Write("f", fmt.Sprintf("the quick brown fox jumps over the lazy dog %d\n", i))
				r.Git("add", "f")
				r.Git("commit", "-qm", fmt.Sprintf("revision %d", i))
			}
			r.Git("repack", "-adq")
			packs, err := filepath.Glob(filepath.Join(r.GitDir(), "objects", "pack", "*.pack"))
			require.NoError(t, err)
			require.Len(t, packs, 1)
			data, err := os.ReadFile(packs[0])
			require.NoError(t, err)
			at := len(data) * part / 100
			data[at] ^= 0xff
			gittest.WriteOver(t, packs[0], data)
			sameAsGit(t, r)
		})
	}
}

// looseObject returns the name git gives a blob and the bytes of its file.
func looseObject(t *testing.T, payload []byte) (gitobj.OID, []byte) {
	t.Helper()
	oid := odb.Hash(gitobj.SHA1, gitobj.TypeBlob, payload)
	body := append([]byte(fmt.Sprintf("blob %d\x00", len(payload))), payload...)
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return oid, buf.Bytes()
}

// TestUnreadableIndex covers the fatal git dies with when the index will not
// parse, and above all the path it names.
//
// git works from the top of the worktree and writes ".git/index". This
// implementation opened an absolute path and printed that, which is a
// divergence in a message a person reads on their worst day. Nothing caught it
// until the two were run side by side over a truncated index.
func TestUnreadableIndex(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.New(t)
	r.SimpleHistory()
	r.Git("read-tree", "HEAD")

	index := filepath.Join(r.GitDir(), "index")
	data, err := os.ReadFile(index)
	require.NoError(t, err)
	require.Greater(t, len(data), 12, "git wrote no index to truncate")
	gittest.WriteOver(t, index, data[:8])

	res := sameAsGit(t, r)
	assert.Equal(t, 128, res.Code, "an index that will not parse is fatal, not a finding")
	assert.Contains(t, res.Stderr, ".git/index: index file smaller than expected")
	assert.NotContains(t, res.Stderr, r.Dir, "the message named the absolute path this process opened")
}
