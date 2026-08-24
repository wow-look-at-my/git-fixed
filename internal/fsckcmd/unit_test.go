package fsckcmd

import (
	"bytes"
	"reflect"
	"sort"
	"testing"
	"unsafe"

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
	// git prints the size of its own object hash, which starts at 32 and doubles once it is half full.
	tab := newObjTable(0)
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

// TestObjTableTellsNamesApartByMoreThanItsHash keeps the table honest about
// what its four bytes are for. They pick a slot and rule a slot out; they never
// stand in for the name. These names share every byte the table looks at, so
// each one has to be told apart by the whole of it.
func TestObjTableTellsNamesApartByMoreThanItsHash(t *testing.T) {
	tab := newObjTable(0)
	want := map[gitobj.OID]*objEntry{}
	for i := range 500 {
		var oid gitobj.OID
		oid.N = 20
		// Everything the shard and the slot are chosen from is identical.
		oid.H[18] = byte(i)
		oid.H[19] = byte(i >> 8)
		e, idx, ok := tab.Lookup(oid, gitobj.TypeBlob)
		require.True(t, ok)
		require.Equal(t, oid.H, e.hash)
		require.Same(t, e, tab.At(idx))
		want[oid] = e
	}
	assert.Equal(t, int64(500), tab.Len(), "500 distinct names are 500 objects")
	for oid, e := range want {
		assert.Same(t, e, tab.Get(oid), "a name must find its own entry again")
	}
}

// TestObjTableSizesItselfFromWhatItIsTold keeps the table from rehashing its way
// up to a size that was known before it started.
func TestObjTableSizesItselfFromWhatItIsTold(t *testing.T) {
	small, big := newObjTable(0), newObjTable(1<<20)
	assert.Equal(t, objShardMin, small.start, "nothing known means start small")
	assert.Greater(t, big.start, objShardMin)
	assert.Equal(t, 0, big.start&(big.start-1), "a mask needs a power of two")
	assert.GreaterOrEqual(t, int64(big.start)*int64(len(big.shards)), int64(2<<20),
		"room for twice what it was told, because the table doubles at half full")
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
	// git refuses to make a fatal check anything but an error, because the parser cannot continue past one.
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

// TestTheTypeAndTheFlagsShareAWord keeps the two halves of meta apart. They are
// one word so that the entry is 48 bytes, and a flag that reached the type byte
// would rename an object's type.
func TestTheTypeAndTheFlagsShareAWord(t *testing.T) {
	var e objEntry
	assert.Equal(t, gitobj.TypeNone, e.Type())
	assert.Zero(t, e.Flags())

	for _, f := range []uint32{flagReachable, flagSeen, flagHasObj, flagUsed, flagWalked} {
		assert.False(t, e.SetFlag(f), "the flag was not set before")
		assert.True(t, e.SetFlag(f), "and is set now")
	}
	assert.Equal(t, gitobj.TypeNone, e.Type(), "no flag reaches the type")

	e.SetType(gitobj.TypeTree)
	assert.Equal(t, gitobj.TypeTree, e.Type())
	e.SetType(gitobj.TypeBlob)
	assert.Equal(t, gitobj.TypeTree, e.Type(), "the first type seen is the one kept")

	e.ClearFlags(flagReachable | flagSeen)
	assert.Equal(t, uint32(flagHasObj|flagUsed|flagWalked), e.Flags())
	assert.Equal(t, gitobj.TypeTree, e.Type(), "clearing a flag leaves the type alone")

	var bad objEntry
	bad.SetType(gitobj.TypeBad)
	assert.Equal(t, gitobj.TypeBad, bad.Type(), "git's negative types survive the byte")
}

// TestAnUnresolvedEdgeKeepsTheTypeItImplied guards what the walk prints about a
// link that leads nowhere: the edge is all it has left of the reference.
func TestAnUnresolvedEdgeKeepsTheTypeItImplied(t *testing.T) {
	resolved := makeEdge(1234, true, gitobj.TypeTree)
	require.True(t, resolved.ok())
	assert.Equal(t, uint32(1234), resolved.index())

	for _, typ := range []gitobj.Type{gitobj.TypeCommit, gitobj.TypeTree, gitobj.TypeBlob, gitobj.TypeTag} {
		e := makeEdge(1234, false, typ)
		require.False(t, e.ok(), "an unresolved edge names no target")
		assert.Equal(t, typ, e.typ())
	}
	// git's negative types have no spelling, and neither did the wider edge that came before this one.
	assert.Equal(t, "unknown", linkTypeName(makeEdge(0, false, gitobj.TypeAny).typ()))
	assert.Equal(t, "unknown", linkTypeName(makeEdge(0, false, gitobj.TypeBad).typ()))
}

// TestAnObjectEntryStaysSmall guards the number the comment on objEntry states.
func TestAnObjectEntryStaysSmall(t *testing.T) {
	assert.Equal(t, uintptr(48), unsafe.Sizeof(objEntry{}))
	assert.Equal(t, uintptr(4), unsafe.Sizeof(edge(0)))
}

// TestAnObjectEntryHoldsNoPointer guards the other half of what an entry costs.
func TestAnObjectEntryHoldsNoPointer(t *testing.T) {
	assert.False(t, holdsPointer(reflect.TypeOf(objEntry{})), "objEntry")
	assert.False(t, holdsPointer(reflect.TypeOf(slot{})), "slot")
	assert.False(t, holdsPointer(reflect.TypeOf(edge(0))), "edge")
}

// holdsPointer reports whether a value of this type has anything in it the
// collector has to follow.
func holdsPointer(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.Slice, reflect.String,
		reflect.Map, reflect.Chan, reflect.Func, reflect.Interface:
		return true
	case reflect.Struct:
		for i := range t.NumField() {
			if holdsPointer(t.Field(i).Type) {
				return true
			}
		}
	case reflect.Array:
		return holdsPointer(t.Elem())
	}
	return false
}
