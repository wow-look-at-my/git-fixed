package fsckcmd_test

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand"
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
