package fsckcmd

// Where the recorded edges live.

import (
	"sync"
	"sync/atomic"
)

// edgeSlabSize is how many edges one slab holds.
const edgeSlabSize = 8192

// edgeArena hands out room for edges and keeps every slab it made.
type edgeArena struct {
	slabs  atomic.Pointer[[][]edge]
	mu     sync.Mutex
	chunks sync.Pool
}

// edgeChunk is the part of one slab a worker is handing out from.
type edgeChunk struct {
	slab uint32
	buf  []edge
	used int
}

// alloc returns room for n edges and the span that names it. The returned slice
// has no spare capacity, so appending past n allocates rather than writing over
// the next object's edges.
func (a *edgeArena) alloc(n int) (edgeSpan, []edge) {
	if n == 0 {
		return edgeSpan{}, nil
	}
	if n > edgeSlabSize {
		// A tree with more entries than a whole slab gets its own.
		slab, buf := a.newSlab(n)
		return edgeSpan{ref: uint64(slab) << 32, n: uint32(n)}, buf
	}
	c, _ := a.chunks.Get().(*edgeChunk)
	if c == nil || len(c.buf)-c.used < n {
		// What is left of the old chunk is dropped: at most one object's edges out of a slab holding thousands.
		slab, buf := a.newSlab(edgeSlabSize)
		c = &edgeChunk{slab: slab, buf: buf}
	}
	out := c.buf[c.used : c.used+n : c.used+n]
	span := edgeSpan{ref: uint64(c.slab)<<32 | uint64(c.used), n: uint32(n)}
	c.used += n
	a.chunks.Put(c)
	return span, out
}

// at returns the edges a span names.
func (a *edgeArena) at(span edgeSpan) []edge {
	if span.n == 0 {
		return nil
	}
	slabs := a.slabs.Load()
	if slabs == nil {
		return nil
	}
	slab := (*slabs)[span.ref>>32]
	pos := uint32(span.ref)
	return slab[pos : pos+span.n : pos+span.n]
}

// newSlab makes one slab and returns its index.
//
// The list of slabs grows by doubling, and a slab that fits in what is already
// there is written into the spare capacity the readers cannot see: a reader
// holding an older list only ever indexes a slab that was in it.
func (a *edgeArena) newSlab(n int) (uint32, []edge) {
	buf := make([]edge, n)
	a.mu.Lock()
	defer a.mu.Unlock()
	var cur [][]edge
	if s := a.slabs.Load(); s != nil {
		cur = *s
	}
	if len(cur) == cap(cur) {
		grown := make([][]edge, len(cur), max(8, 2*len(cur)))
		copy(grown, cur)
		cur = grown
	}
	cur = append(cur, buf)
	a.slabs.Store(&cur)
	return uint32(len(cur) - 1), buf
}
