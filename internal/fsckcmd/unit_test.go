package fsckcmd

import (
	"bytes"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

func TestParseSize(t *testing.T) {
	n, err := parseSize("1024")
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	n, err = parseSize("0")
	require.NoError(t, err)
	assert.Zero(t, n)

	_, err = parseSize("")
	assert.Error(t, err)
	_, err = parseSize("1k")
	assert.Error(t, err, "the message-id limit takes plain digits only")
	_, err = parseSize("-1")
	assert.Error(t, err)
}

func TestLinkTypeName(t *testing.T) {
	assert.Equal(t, "commit", linkTypeName(gitobj.TypeCommit))
	assert.Equal(t, "tree", linkTypeName(gitobj.TypeTree))
	assert.Equal(t, "unknown", linkTypeName(gitobj.TypeAny))
	assert.Equal(t, "unknown", linkTypeName(gitobj.TypeBad))
}

func TestHashSlots(t *testing.T) {
	// git prints the size of its own object hash, which starts at 32 and
	// doubles once it is half full.
	tab := newObjTable()
	assert.Equal(t, int64(0), tab.HashSlots())
	for i := range 40 {
		var oid gitobj.OID
		oid.N = 20
		oid.H[0] = byte(i)
		oid.H[1] = byte(i >> 8)
		tab.Lookup(oid, gitobj.TypeNone)
		assert.GreaterOrEqual(t, tab.HashSlots(), int64(32))
	}
	assert.Equal(t, int64(40), tab.Len())
}

func TestReporterVerbosef(t *testing.T) {
	var buf bytes.Buffer
	r := newReporter(&bytes.Buffer{}, &buf)
	r.Verbosef("Checking %s", "abc")
	assert.Equal(t, "Checking abc\n", buf.String())
}

func TestReporterOrdersByKey(t *testing.T) {
	var out, errBuf bytes.Buffer
	r := newReporter(&out, &errBuf)
	r.Outf(sortKey{phase: phaseConnectivity, group: 1}, "third")
	r.Errf(sortKey{phase: phaseObjects, group: 2}, "second")
	r.Outf(sortKey{phase: phaseObjects, group: 1}, "first")
	r.Flush()
	assert.Equal(t, "first\nthird\n", out.String())
	assert.Equal(t, "second\n", errBuf.String())
}

func TestSortKeyOrder(t *testing.T) {
	a, _ := gitobj.SHA1.Parse("0000000000000000000000000000000000000001")
	b, _ := gitobj.SHA1.Parse("0000000000000000000000000000000000000002")
	keys := []sortKey{
		{phase: phaseConnectivity, oid: a},
		{phase: phaseObjects, group: 1, oid: b},
		{phase: phaseObjects, group: 1, oid: a},
		{phase: phaseObjects, group: 0, pos: 9},
		{phase: phaseObjects, group: 0, pos: 2},
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	assert.Equal(t, int64(2), keys[0].pos)
	assert.Equal(t, int64(9), keys[1].pos)
	assert.Equal(t, a, keys[2].oid)
	assert.Equal(t, b, keys[3].oid)
	assert.Equal(t, phaseConnectivity, keys[4].phase)
}

// newTestRun builds the minimum of a run needed to exercise the option
// plumbing, which does not touch a repository.
func newTestRun(stderr *bytes.Buffer) *run {
	o := DefaultOptions()
	o.Stderr = stderr
	return &run{o: o, fsck: fsck.NewOptions(gitobj.SHA1)}
}

func TestNoteFatalMsg(t *testing.T) {
	var errBuf bytes.Buffer
	r := newTestRun(&errBuf)
	assert.Empty(t, r.died())
	r.noteFatalMsg("first")
	r.noteFatalMsg("second")
	assert.Equal(t, "first", r.died(), "the first fatal condition is the one git dies on")
}

func TestSetMsgType(t *testing.T) {
	var errBuf bytes.Buffer
	r := newTestRun(&errBuf)
	assert.Zero(t, r.setMsgType("missingemail", "ignore"))
	assert.Equal(t, fsck.SevIgnore, r.fsck.Severity(fsck.MsgMissingEmail))

	assert.Zero(t, r.setMsgType("largepathname", "warn:1024"))
	assert.Equal(t, 1024, r.fsck.MaxTreeEntryLen)
	assert.Equal(t, fsck.SevWarn, r.fsck.Severity(fsck.MsgLargePathname))
}

func TestSetMsgTypeErrors(t *testing.T) {
	for _, c := range []struct {
		name, value, want string
	}{
		{"nosuchid", "warn", "fatal: Unhandled message id: nosuchid\n"},
		{"largepathname", "warn:x", "fatal: unable to parse max tree entry len: x\n"},
		{"missingemail", "loud", "fatal: Unknown fsck message type: 'loud'\n"},
	} {
		var errBuf bytes.Buffer
		r := newTestRun(&errBuf)
		assert.Equal(t, 128, r.setMsgType(c.name, c.value), c.name)
		assert.Equal(t, c.want, errBuf.String())
	}
}

func TestSetMsgTypeCannotDemoteFatal(t *testing.T) {
	// git refuses to make a fatal check anything but an error, because the
	// parser cannot continue past one.
	require.Equal(t, fsck.SevFatal, fsck.MsgNulInHeader.DefaultSeverity())

	var errBuf bytes.Buffer
	r := newTestRun(&errBuf)
	assert.Equal(t, 128, r.setMsgType("nulinheader", "warn"))
	assert.Equal(t, "fatal: Cannot demote nulinheader to warn\n", errBuf.String())

	errBuf.Reset()
	r = newTestRun(&errBuf)
	assert.Zero(t, r.setMsgType("nulinheader", "error"))
	assert.Empty(t, errBuf.String())
}
