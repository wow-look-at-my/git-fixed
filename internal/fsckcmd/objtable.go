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

// metaFlagShift is where the flags sit in meta. The type has the byte below them.
const metaFlagShift = 8

// objEntry is an object the run knows about, whether or not it exists. Its
// size is fixed, and every field is a field per object in the repository.
// see docs/architecture.md
type objEntry struct {
	// edgeRef and edgeLen name what the object pass found this object points at. see docs/architecture.md
	edgeRef uint64

	edgeLen uint32
	// meta is the type in its low byte and the flags above it: keeping them in separate words would round the entry up.
	meta atomic.Uint32

	// hash is the name without the length, which belongs to the algorithm.
	hash [gitobj.MaxRawSize]byte
}

// oid rebuilds the object's name from the repository's hash algorithm.
func (r *run) oid(e *objEntry) gitobj.OID {
	return gitobj.OID{H: e.hash, N: uint8(r.repo.Algo.RawSize)}
}

// edgeSpan names a run of edges in the arena: the slab in the high half of ref.
type edgeSpan struct {
	ref uint64
	n   uint32
}

// edge is a reference, in the form the walk needs: a per-tree-entry record. see docs/architecture.md
type edge uint32

const (
	// edgeUnresolved marks a reference the table refused, and the low bits then carry the type it implied.
	edgeUnresolved edge = 1 << 31
	edgeTypeMask   edge = 0xf
	// maxObjIndex is the largest table index an edge can name.
	maxObjIndex = int64(edgeUnresolved) - 1
)

// makeEdge builds a reference. target means nothing when resolved is false,
// and typ means nothing when it is true. see Edges.
func makeEdge(target uint32, resolved bool, typ gitobj.Type) edge {
	if !resolved {
		return edgeUnresolved | edge(typ)&edgeTypeMask
	}
	return edge(target)
}

// index is the target's place in the table. It answers for a resolved edge.
func (e edge) index() uint32 { return uint32(e) }

// ok reports whether the reference resolved.
func (e edge) ok() bool { return e&edgeUnresolved == 0 }

// typ is the type the reference implies. It answers for an unresolved edge.
func (e edge) typ() gitobj.Type { return gitobj.Type(e & edgeTypeMask) }

// SetEdges records what the object pass found.
func (e *objEntry) SetEdges(span edgeSpan) {
	e.edgeRef, e.edgeLen = span.ref, span.n
	e.SetFlag(flagWalked)
}

// Edges returns the recorded references, and whether the pass recorded any.
// A resolved edge holds no type: its target's type is the type it implied.
func (t *objTable) Edges(e *objEntry) ([]edge, bool) {
	if e.Flags()&flagWalked == 0 {
		return nil, false
	}
	return t.arena.at(edgeSpan{ref: e.edgeRef, n: e.edgeLen}), true
}

// Type is what referenced this object expects, when the database has not said. The byte is signed, and stores gitobj.TypeBad as negative.
func (e *objEntry) Type() gitobj.Type { return gitobj.Type(int8(e.meta.Load())) }

// SetType records a type, keeping whichever type was seen earliest.
func (e *objEntry) SetType(t gitobj.Type) {
	for {
		old := e.meta.Load()
		if int8(old) != int8(gitobj.TypeNone) {
			return
		}
		if e.meta.CompareAndSwap(old, old&^0xff|uint32(uint8(t))) {
			return
		}
	}
}

// Flags reads the object's flags.
func (e *objEntry) Flags() uint32 { return e.meta.Load() >> metaFlagShift }

// SetFlag turns a flag on and reports whether it was already set.
func (e *objEntry) SetFlag(f uint32) bool {
	f <<= metaFlagShift
	for {
		old := e.meta.Load()
		if old&f != 0 {
			return true
		}
		if e.meta.CompareAndSwap(old, old|f) {
			return false
		}
	}
}

// ClearFlags turns flags off.
func (e *objEntry) ClearFlags(f uint32) {
	f <<= metaFlagShift
	for {
		old := e.meta.Load()
		if e.meta.CompareAndSwap(old, old&^f) {
			return
		}
	}
}

// objSlabSize is how many entries each allocation holds.
const (
	objSlabBits = 12
	objSlabSize = 1 << objSlabBits
)

type objSlab = [objSlabSize]objEntry

// objTable holds every object the run has heard of.
//
// It is not a Go map. An object name is itself a hash, so a slice of its own bytes serves as the hash and no hash
// function is needed: with a map, Lookup dominated the profile on a large run. Entries come from slabs.
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

// slot is a place in a shard's open-addressed table.
type slot struct {
	key uint32
	idx uint32
}

type objShard struct {
	mu    sync.Mutex
	slots []slot
	mask  uint32
	used  int
	// pad keeps a shard's lock off the cache line of its neighbor.
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
// knows the repository holds, from the pack indexes, or an unset count.
//
// Growing a shard means rehashing it, and a shard that starts at objShardMin
// slots and grows into the thousands rehashes its contents many times over: on
// a large repository that cost a measurable share of the whole run, spent
// arriving at a size that was known before it started.
func newObjTable(expect int64) *objTable {
	n := shardCount()
	t := &objTable{shards: make([]objShard, n), mask: uint32(n - 1)}
	t.start = objShardMin
	// The expected share of a shard, doubled: the table doubles at half full, so landing exactly on it would double again.
	for want := 2 * (expect/int64(n) + 1); int64(t.start) < want && t.start < 1<<24; {
		t.start <<= 1
	}
	// The slot tables themselves are made lazily, on the initial write.
	return t
}

// shard picks an object's shard from the leading bytes of its name.
func (t *objTable) shard(oid gitobj.OID) *objShard {
	h := uint32(oid.H[0])<<8 | uint32(oid.H[1])
	return &t.shards[h&t.mask]
}

// slotKey is the hash: where a name starts probing.
func slotKey(oid gitobj.OID) uint32 { return binary.LittleEndian.Uint32(oid.H[4:8]) }

// At returns the object at an index, counting from the table's start up to Len.
func (t *objTable) At(i uint32) *objEntry {
	s := t.slabs.Load()
	return &(*s)[i>>objSlabBits][i&(objSlabSize-1)]
}

// newEntry takes the next index and returns the entry it names.
func (t *objTable) newEntry(oid gitobj.OID) (*objEntry, uint32) {
	n := t.created.Add(1) - 1
	if n > maxObjIndex {
		// An edge names its target by this index, so an index past the top bit would resolve to another object.
		panic("git-fixed: this repository holds more objects than the object table can name")
	}
	idx := uint32(n)
	s := t.slabs.Load()
	if s == nil || int(idx>>objSlabBits) >= len(*s) {
		s = t.growSlabs(idx)
	}
	e := &(*s)[idx>>objSlabBits][idx&(objSlabSize-1)]
	e.hash = oid.H
	return e, idx
}

// growSlabs adds slabs until the growth covers idx. Several goroutines can
// reach here concurrently, each holding a different index, so it takes the
// lock and looks again.
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
			if e := t.At(sl.idx - 1); e.hash == oid.H {
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
			if e := t.At(sl.idx - 1); e.hash == oid.H {
				return reconcile(e, sl.idx-1, want)
			}
		}
		i = (i + 1) & sh.mask
	}
	e, idx := t.newEntry(oid)
	if want != gitobj.TypeAny {
		e.SetType(want)
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
	cur := e.Type()
	if cur == gitobj.TypeNone {
		e.SetType(want)
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
