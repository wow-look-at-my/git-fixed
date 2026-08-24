package fsckcmd

import (
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Flags on a tracked object, matching the ones builtin/fsck.c defines.
const (
	flagReachable = 1 << 0
	flagSeen      = 1 << 1
	flagHasObj    = 1 << 2
	flagUsed      = 1 << 3
	// flagWalked says the object pass recorded this object's edges.
	flagWalked = 1 << 4
)

// objEntry is one object the run knows about, whether or not it exists.
type objEntry struct {
	// edgeRef and edgeLen name what the object pass found this object points at.
	edgeRef uint64

	OID     gitobj.OID
	edgeLen uint32
	typ     atomic.Int32
	flags   atomic.Uint32
}

// edgeSpan names a run of edges in the arena: the slab in the high half of ref.
type edgeSpan struct {
	ref uint64
	n   uint32
}

// edge is one resolved reference, in the compact form the connectivity walk needs.
type edge uint64

const (
	edgeResolved  edge = 1 << 32
	edgeViaTag    edge = 1 << 33
	edgeTypeShift      = 34
	edgeTypeMask  edge = 0xf << edgeTypeShift
)

// makeEdge builds one reference. target means nothing when resolved is false.
func makeEdge(target uint32, resolved bool, typ gitobj.Type, viaTag bool) edge {
	e := edge(target) | edge(typ&0xf)<<edgeTypeShift
	if resolved {
		e |= edgeResolved
	}
	if viaTag {
		e |= edgeViaTag
	}
	return e
}

// index is the target's place in the table.
func (e edge) index() uint32 { return uint32(e) }

// ok reports whether the reference resolved.
func (e edge) ok() bool { return e&edgeResolved != 0 }

// viaTag marks a tag's target, which git accepts at any type.
func (e edge) viaTag() bool { return e&edgeViaTag != 0 }

// typ is the type the reference implies.
func (e edge) typ() gitobj.Type { return gitobj.Type((e & edgeTypeMask) >> edgeTypeShift) }

// SetEdges records what the object pass found.
func (e *objEntry) SetEdges(span edgeSpan) {
	e.edgeRef, e.edgeLen = span.ref, span.n
	e.SetFlag(flagWalked)
}

// Edges returns the recorded references, and reports whether the object pass
// recorded any.
func (t *objTable) Edges(e *objEntry) ([]edge, bool) {
	if e.Flags()&flagWalked == 0 {
		return nil, false
	}
	return t.arena.at(edgeSpan{ref: e.edgeRef, n: e.edgeLen}), true
}

// Type returns the object's type, which may be an expectation recorded by whatever referenced it rather than.
func (e *objEntry) Type() gitobj.Type { return gitobj.Type(e.typ.Load()) }

// SetType records a type, keeping the first one seen.
func (e *objEntry) SetType(t gitobj.Type) { e.typ.CompareAndSwap(int32(gitobj.TypeNone), int32(t)) }

// Flags reads the object's flags.
func (e *objEntry) Flags() uint32 { return e.flags.Load() }

// SetFlag turns one flag on and reports whether it was already set.
func (e *objEntry) SetFlag(f uint32) bool {
	for {
		old := e.flags.Load()
		if old&f != 0 {
			return true
		}
		if e.flags.CompareAndSwap(old, old|f) {
			return false
		}
	}
}

// ClearFlags turns flags off.
func (e *objEntry) ClearFlags(f uint32) {
	for {
		old := e.flags.Load()
		if e.flags.CompareAndSwap(old, old&^f) {
			return
		}
	}
}

// objSlabSize is how many entries one allocation holds.
const (
	objSlabBits = 12
	objSlabSize = 1 << objSlabBits
)

type objSlab = [objSlabSize]objEntry

// objTable holds every object the run has heard of.
//
// It is not a Go map. An object name is itself a hash, spread uniformly by
// construction, so a table that takes four of its bytes as the hash needs no
// hash function at all: with a map, Lookup was a quarter of the whole run on a
// million-object repository. Entries come from slabs so that a million objects
// cost a few hundred allocations, and each one has an index an edge can hold
// without a pointer.
//
// see docs/architecture.md
type objTable struct {
	shards []objShard
	mask   uint32
	// arena holds every edge the object pass records.
	arena edgeArena
	// created counts objects the way git's object hash does.
	created atomic.Int64

	// start is how many slots a shard's table is made with. see newObjTable.
	start int

	// slabs is replaced rather than written in place, so a reader holding an index follows it without a lock.
	slabs  atomic.Pointer[[]*objSlab]
	slabMu sync.Mutex
}

// slot is one place in a shard's open-addressed table.
type slot struct {
	key uint32
	idx uint32
}

type objShard struct {
	mu    sync.Mutex
	slots []slot
	mask  uint32
	used  int
	// pad keeps one shard's lock off the cache line of the next one.
	_ [16]byte
}

// objShardMin is the smallest a shard's table is made.
const objShardMin = 8

// shardCount picks how finely to split the table.
func shardCount() int {
	n := 64 * runtime.GOMAXPROCS(0)
	size := 256
	for size < n && size < 1<<16 {
		size <<= 1
	}
	return size
}

// newObjTable builds the table. expect is how many objects the run already
// knows the repository holds, from the pack indexes, or zero.
//
// Growing a shard means rehashing it, and a shard that starts at eight slots
// and ends at eight thousand rehashes its contents ten times over: on a
// million-object repository that was five percent of the whole run, spent
// arriving at a size that was known before it started.
func newObjTable(expect int64) *objTable {
	n := shardCount()
	t := &objTable{shards: make([]objShard, n), mask: uint32(n - 1)}
	t.start = objShardMin
	// Twice the expected share of a shard, because the table doubles at half
	// full and a shard that lands exactly on its size would double at once.
	for want := 2 * (expect/int64(n) + 1); int64(t.start) < want && t.start < 1<<24; {
		t.start <<= 1
	}
	// The slot tables themselves are made on first write.
	return t
}

// shard picks an object's shard from the first two bytes of its name.
func (t *objTable) shard(oid gitobj.OID) *objShard {
	h := uint32(oid.H[0])<<8 | uint32(oid.H[1])
	return &t.shards[h&t.mask]
}

// slotKey is the hash: where a name starts probing.
func slotKey(oid gitobj.OID) uint32 { return binary.LittleEndian.Uint32(oid.H[4:8]) }

// At returns the object at an index. Indices run from zero to Len.
func (t *objTable) At(i uint32) *objEntry {
	s := t.slabs.Load()
	return &(*s)[i>>objSlabBits][i&(objSlabSize-1)]
}

// newEntry takes the next index and returns the entry it names.
func (t *objTable) newEntry(oid gitobj.OID) (*objEntry, uint32) {
	idx := uint32(t.created.Add(1) - 1)
	s := t.slabs.Load()
	if s == nil || int(idx>>objSlabBits) >= len(*s) {
		s = t.growSlabs(idx)
	}
	e := &(*s)[idx>>objSlabBits][idx&(objSlabSize-1)]
	e.OID = oid
	return e, idx
}

// growSlabs adds slabs until one covers idx. Several goroutines reach here at
// once, each holding a different index, so it takes the lock and looks again.
func (t *objTable) growSlabs(idx uint32) *[]*objSlab {
	t.slabMu.Lock()
	defer t.slabMu.Unlock()
	want := int(idx>>objSlabBits) + 1
	var cur []*objSlab
	if s := t.slabs.Load(); s != nil {
		if len(*s) >= want {
			return s
		}
		cur = *s
	}
	next := make([]*objSlab, len(cur), max(want, 2*len(cur)))
	copy(next, cur)
	for len(next) < want {
		next = append(next, new(objSlab))
	}
	t.slabs.Store(&next)
	return &next
}

// Get returns an object the run already knows about, or nil.
func (t *objTable) Get(oid gitobj.OID) *objEntry {
	sh := t.shard(oid)
	key := slotKey(oid)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.slots == nil {
		return nil
	}
	for i := key & sh.mask; ; i = (i + 1) & sh.mask {
		sl := &sh.slots[i]
		if sl.idx == 0 {
			return nil
		}
		if sl.key == key {
			if e := t.At(sl.idx - 1); e.OID == oid {
				return e
			}
		}
	}
}

// Lookup is git's lookup_object() plus its lookup_<type>() wrappers: it creates
// the entry when it is new, and refuses when the type contradicts what the
// entry already holds. It returns the entry and its index.
func (t *objTable) Lookup(oid gitobj.OID, want gitobj.Type) (*objEntry, uint32, bool) {
	sh := t.shard(oid)
	key := slotKey(oid)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.slots == nil {
		sh.slots = make([]slot, t.start)
		sh.mask = uint32(t.start - 1)
	}
	i := key & sh.mask
	for {
		sl := &sh.slots[i]
		if sl.idx == 0 {
			break
		}
		if sl.key == key {
			if e := t.At(sl.idx - 1); e.OID == oid {
				return reconcile(e, sl.idx-1, want)
			}
		}
		i = (i + 1) & sh.mask
	}
	e, idx := t.newEntry(oid)
	if want != gitobj.TypeAny {
		e.typ.Store(int32(want))
	}
	sh.slots[i] = slot{key: key, idx: idx + 1}
	sh.used++
	if sh.used*2 >= len(sh.slots) {
		sh.grow()
	}
	return e, idx, true
}

// reconcile applies the type the reference implies to an entry that already
// exists.
func reconcile(e *objEntry, idx uint32, want gitobj.Type) (*objEntry, uint32, bool) {
	if want == gitobj.TypeAny {
		return e, idx, true
	}
	cur := gitobj.Type(e.typ.Load())
	if cur == gitobj.TypeNone {
		e.typ.Store(int32(want))
		return e, idx, true
	}
	if cur != want {
		// git's object_as_type() returns NULL here, and its caller treats that as a broken link.
		return nil, 0, false
	}
	return e, idx, true
}

// grow doubles the shard and puts every slot back. A slot carries its own hash,
// so nothing here reads an entry.
func (sh *objShard) grow() {
	old := sh.slots
	sh.slots = make([]slot, len(old)*2)
	sh.mask = uint32(len(sh.slots) - 1)
	for _, sl := range old {
		if sl.idx == 0 {
			continue
		}
		i := sl.key & sh.mask
		for sh.slots[i].idx != 0 {
			i = (i + 1) & sh.mask
		}
		sh.slots[i] = sl
	}
}

// Len is the number of objects the run knows about.
func (t *objTable) Len() int64 { return t.created.Load() }

// HashSlots mirrors the size of git's internal object hash.
func (t *objTable) HashSlots() int64 {
	size := int64(0)
	for i := int64(0); i < t.created.Load(); i++ {
		if size-1 <= (i+1)*2 {
			if size < 32 {
				size = 32
			} else {
				size *= 2
			}
		}
	}
	return size
}
