package fsckcmd

import (
	"sort"
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
)

// objEntry is one object the run knows about, whether or not it exists.
type objEntry struct {
	OID   gitobj.OID
	typ   atomic.Int32
	flags atomic.Uint32
	seq   int64

	// edges is what the object pass found this object points at. Keeping
	// them saves the connectivity walk from inflating and parsing every
	// object a second time. git keeps the whole parsed object for the same
	// reason; an edge is a fraction of the memory.
	//
	// see docs/architecture.md
	edges []edge
	// badEdges and edgeErrs carry the two rare cases, which need the
	// strings a message prints. Keeping them out of edge is what lets a
	// large repository hold one edge per tree entry for the whole run.
	badEdges []link
	edgeErrs []string
	walked   atomic.Bool
}

// edge is one resolved reference, in the compact form the connectivity walk
// needs. It holds no strings, so the collector has almost nothing to scan.
type edge struct {
	target *objEntry
	typ    gitobj.Type
	viaTag bool
}

// ok reports whether the reference resolved. The table hands back no entry when
// the target's type contradicts what the reference implies, which git reports
// as a broken link.
func (e edge) ok() bool { return e.target != nil }

// SetEdges records what the object pass found. Only the goroutine that won the
// flagSeen race calls this.
func (e *objEntry) SetEdges(edges []edge, bad []link, errs []string) {
	e.edges = edges
	e.badEdges = bad
	e.edgeErrs = errs
	e.walked.Store(true)
}

// Edges returns the recorded references, and reports whether the object pass
// recorded any.
func (e *objEntry) Edges() ([]edge, []link, []string, bool) {
	if !e.walked.Load() {
		return nil, nil, nil, false
	}
	return e.edges, e.badEdges, e.edgeErrs, true
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

// objTable holds every object the run has heard of. It is sharded so the
// parallel phases can insert without queueing behind one lock.
type objTable struct {
	shards [256]objShard
	seq    atomic.Int64
	// created counts objects the way git's object hash does, which is what
	// its verbose "Checking connectivity (N objects)" line reports.
	created atomic.Int64
}

type objShard struct {
	mu sync.Mutex
	m  map[gitobj.OID]*objEntry
}

func newObjTable() *objTable {
	t := &objTable{}
	for i := range t.shards {
		t.shards[i].m = make(map[gitobj.OID]*objEntry)
	}
	return t
}

func (t *objTable) shard(oid gitobj.OID) *objShard { return &t.shards[oid.H[0]] }

// Get returns an object the run already knows about, or nil.
func (t *objTable) Get(oid gitobj.OID) *objEntry {
	s := t.shard(oid)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[oid]
}

// Lookup is git's lookup_object() plus its lookup_<type>() wrappers: it creates
// the entry when it is new, and refuses when the type contradicts what the
// entry already holds.
func (t *objTable) Lookup(oid gitobj.OID, want gitobj.Type) (*objEntry, bool) {
	s := t.shard(oid)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[oid]
	if !ok {
		e = &objEntry{OID: oid, seq: t.seq.Add(1)}
		if want != gitobj.TypeAny {
			e.typ.Store(int32(want))
		}
		s.m[oid] = e
		t.created.Add(1)
		return e, true
	}
	if want == gitobj.TypeAny {
		return e, true
	}
	cur := gitobj.Type(e.typ.Load())
	if cur == gitobj.TypeNone {
		e.typ.Store(int32(want))
		return e, true
	}
	if cur != want {
		// git's object_as_type() returns NULL here, and its caller
		// treats that as a broken link.
		return nil, false
	}
	return e, true
}

// All returns every object, ordered by name so a report is reproducible.
func (t *objTable) All() []*objEntry {
	var out []*objEntry
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		for _, e := range s.m {
			out = append(out, e)
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OID.Compare(out[j].OID) < 0 })
	return out
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
