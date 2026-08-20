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
	// flagRare says run.rare holds this object's bad tree entries or its
	// parse errors, which almost no object has.
	flagRare = 1 << 5
)

// objEntry is one object the run knows about, whether or not it exists.
//
// There is one of these per object, so a field here costs megabytes on a large
// repository. It is 64 bytes. The two rare things an object can carry -- a tree
// entry whose mode names no kind of object, and a commit or tag that will not
// parse -- live in run.rare, not in bytes every object pays.
type objEntry struct {
	OID   gitobj.OID
	typ   atomic.Int32
	flags atomic.Uint32

	// edges is what the object pass found this object points at. Keeping
	// them saves the connectivity walk from inflating and parsing every
	// object a second time. git keeps the whole parsed object for the same
	// reason; an edge is a fraction of the memory.
	//
	// see docs/architecture.md
	edges []edge
}

// edge is one resolved reference, in the compact form the connectivity walk
// needs.
//
// It names its target by index into the table's slabs, not by pointer, and it
// holds no string, so an edge contains no pointer at all. There is one edge per
// tree entry, which is tens of millions of them on a large repository, and a
// pointer in this structure puts every one of them under the collector on every
// cycle.
//
// The target's index sits in the low 32 bits. Above it are the bit that says
// the reference resolved, the bit that marks a tag's target, and the type the
// reference implies. That type is only ever commit, tree, blob or tag, so four
// bits hold it.
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

// ok reports whether the reference resolved. The table hands back no entry when
// the target's type contradicts what the reference implies, which git reports
// as a broken link.
func (e edge) ok() bool { return e&edgeResolved != 0 }

// viaTag marks a tag's target, which git accepts at any type.
func (e edge) viaTag() bool { return e&edgeViaTag != 0 }

// typ is the type the reference implies.
func (e edge) typ() gitobj.Type { return gitobj.Type((e & edgeTypeMask) >> edgeTypeShift) }

// SetEdges records what the object pass found. Only the goroutine that won the
// flagSeen race calls this.
func (e *objEntry) SetEdges(edges []edge) {
	e.edges = edges
	e.SetFlag(flagWalked)
}

// Edges returns the recorded references, and reports whether the object pass
// recorded any.
func (e *objEntry) Edges() ([]edge, bool) {
	if e.Flags()&flagWalked == 0 {
		return nil, false
	}
	return e.edges, true
}

// Type returns the object's type, which may be an expectation recorded by
// whatever referenced it rather than something read from the database.
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

// objSlabSize is how many entries one allocation holds. A repository has
// millions of objects, and one allocation each was both memory the run did not
// need and, through the collector, a good part of the time it took.
const (
	objSlabBits = 12
	objSlabSize = 1 << objSlabBits
)

type objSlab = [objSlabSize]objEntry

// objTable holds every object the run has heard of.
//
// It is not a Go map. An object name is itself a hash, spread uniformly by
// construction, so a table that takes eight of its bytes as the hash needs no
// hash function at all: with a map, Lookup was a quarter of the whole run on a
// million-object repository. Entries come from slabs so that a million objects
// cost a few hundred allocations, and each one has an index an edge can hold
// without a pointer.
//
// see docs/architecture.md
type objTable struct {
	shards []objShard
	mask   uint32
	// created counts objects the way git's object hash does, which is what
	// its verbose "Checking connectivity (N objects)" line reports. It is
	// also where the next entry's index comes from, so indices are dense
	// and the phases that visit every object can just count.
	created atomic.Int64

	// slabs is replaced rather than written in place, so a reader holding an
	// index follows it without a lock.
	slabs  atomic.Pointer[[]*objSlab]
	slabMu sync.Mutex
}

// slot is one place in a shard's open-addressed table. It holds eight bytes of
// the name, which decide whether a probe is worth following into an entry, and
// the entry's index plus one, so the zero value is an empty slot. Nothing here
// is a pointer, so the table costs the collector nothing either.
type slot struct {
	key uint64
	idx uint32
	_   uint32
}

type objShard struct {
	mu    sync.Mutex
	slots []slot
	mask  uint32
	used  int
	// pad keeps one shard's lock off the cache line of the next one. Two
	// threads on neighbouring shards otherwise fight over a cache line while
	// holding different locks, which looks like contention that is not there.
	_ [16]byte
}

// objShardInit is where a shard's table starts. There is one table per shard
// and tens of thousands of shards on a large machine, so a repository with a
// handful of objects must not pay for all of them.
const objShardInit = 8

// shardCount picks how finely to split the table.
//
// The table is written once per tree entry, which is millions of times in a
// large repository, so two workers landing on one shard is a cost that scales
// with the core count. A fixed 256 was fine on four cores and poor on ninety
// six, where a third of accesses would collide. Sixty four shards per core
// keeps that near one percent wherever it runs.
func shardCount() int {
	n := 64 * runtime.GOMAXPROCS(0)
	size := 256
	for size < n && size < 1<<16 {
		size <<= 1
	}
	return size
}

func newObjTable() *objTable {
	n := shardCount()
	// The slot tables are made on first write. A big repository fills every
	// shard, and a small one should not pay to build tens of thousands of
	// tables it will never use.
	return &objTable{shards: make([]objShard, n), mask: uint32(n - 1)}
}

// shard picks an object's shard from the first two bytes of its name. One byte
// only distinguishes 256 of them, which would waste every shard past that.
func (t *objTable) shard(oid gitobj.OID) *objShard {
	h := uint32(oid.H[0])<<8 | uint32(oid.H[1])
	return &t.shards[h&t.mask]
}

// slotKey is the hash. An object name is already uniform, so the table takes it
// as it is rather than running a hash function over it.
func slotKey(oid gitobj.OID) uint64 { return binary.LittleEndian.Uint64(oid.H[:8]) }

// slotPos is where a key starts probing. It uses the bytes the shard did not:
// the first two chose the shard, and reusing them here would leave most of a
// shard's table unreachable.
func slotPos(key uint64) uint32 { return uint32(key >> 32) }

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
	for i := slotPos(key) & sh.mask; ; i = (i + 1) & sh.mask {
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
		sh.slots = make([]slot, objShardInit)
		sh.mask = objShardInit - 1
	}
	i := slotPos(key) & sh.mask
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
		// git's object_as_type() returns NULL here, and its caller
		// treats that as a broken link.
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
		i := slotPos(sl.key) & sh.mask
		for sh.slots[i].idx != 0 {
			i = (i + 1) & sh.mask
		}
		sh.slots[i] = sl
	}
}

// Len is the number of objects the run knows about.
func (t *objTable) Len() int64 { return t.created.Load() }

// HashSlots mirrors the size of git's internal object hash, which is the number
// its verbose connectivity line prints. The table starts at 32 entries and
// doubles once it is half full.
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
