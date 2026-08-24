package odb

// What one pack entry costs, and what it derives rather than holds.

import (
	"reflect"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// TestAPackEntryStaysSmall guards the number the comment on packEntry states.
// A pack's layout and the caller's object table are alive at the same moment
// and describe the same objects, so a field here is a field per object.
func TestAPackEntryStaysSmall(t *testing.T) {
	assert.Equal(t, uintptr(24), unsafe.Sizeof(packEntry{}))
	// Nothing in it may be a pointer either: tens of millions would be marked on every collection.
	assert.False(t, holdsPointer(reflect.TypeOf(packEntry{})), "packEntry")
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

// TestAnEntryDerivesWhereItsPayloadStarts covers the two fields the entry no
// longer holds: the header length stands in for the offset of the zlib stream,
// and an entry ends where the next one starts.
func TestAnEntryDerivesWhereItsPayloadStarts(t *testing.T) {
	l := &packLayout{
		ents: []packEntry{
			{off: 12, hdr: 3},
			{off: 40, hdr: 22},
		},
		trailer: 100,
	}
	assert.Equal(t, int64(15), l.ents[0].dataOff())
	assert.Equal(t, int64(62), l.ents[1].dataOff())
	assert.Equal(t, int64(40), l.end(0), "an entry ends where the next one starts")
	assert.Equal(t, int64(100), l.end(1), "the last one ends at the trailer")
}

// TestATypeSurvivesItsByte keeps git's negative types readable out of the byte
// they are stored in, because a bad entry is marked with one.
func TestATypeSurvivesItsByte(t *testing.T) {
	for _, typ := range []gitobj.Type{
		gitobj.TypeBad, gitobj.TypeNone, gitobj.TypeCommit, gitobj.TypeTree,
		gitobj.TypeBlob, gitobj.TypeTag, gitobj.TypeOfsDelta, gitobj.TypeRefDelta,
	} {
		var e packEntry
		e.setType(typ)
		assert.Equal(t, typ, e.objType())
	}
}
