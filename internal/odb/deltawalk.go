package odb

// The delta walk: one pack's chains, decoded from the top down under a memory budget. see docs/pack-verification.md

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// walker decodes delta chains for one pack.
type walker struct {
	p      *Pack
	l      *packLayout
	o      *VerifyOpts
	emit   func(gitobj.OID, string)
	object func(gitobj.OID, gitobj.Type, int64, []byte)
	// budget is the decoded base data every worker on this pack may still hold between them. see DefaultChainBudget.
	budget atomic.Int64
}

// frame is one level of an in-progress delta chain.
type frame struct {
	entry int32
	data  []byte
	next  int32 // position in childList of the next child to visit
}

// take reserves room to hold one decoded base. The bottom of a worker's stack
// is always allowed: a worker that could hold nothing would decode nothing.
func (w *walker) take(depth int, n int64) bool {
	if depth == 0 {
		return true
	}
	for {
		cur := w.budget.Load()
		if cur < n {
			return false
		}
		if w.budget.CompareAndSwap(cur, cur-n) {
			return true
		}
	}
}

// give hands a reservation back.
func (w *walker) give(depth int, n int64) {
	if depth == 0 {
		return
	}
	w.budget.Add(n)
}

// walkChain decodes one base object and every delta built on it, reusing the
// parent's buffer instead of decoding a chain again for each of its children.
func (w *walker) walkChain(root int32, in *Inflater) {
	l, p := w.l, w.p
	e := &l.ents[root]
	if e.typ == gitobj.TypeBad {
		return
	}
	if e.typ == gitobj.TypeBlob && w.o.BigFileThreshold > 0 && e.size >= w.o.BigFileThreshold &&
		len(l.children(root)) == 0 {
		w.finishStreamed(root)
		return
	}
	typ, data, err := w.materializeRoot(e, in)
	if err != nil {
		oid := p.OIDAt(e.idx)
		cannotUnpack(w.emit, p, l, oid, *e)
		failSubtree(w.emit, p, l, root)
		return
	}
	w.finish(root, typ, data)
	if len(l.children(root)) == 0 {
		return
	}
	// Anything the spread could not afford to hold comes back here, already checked.
	deferred := w.spread(root, typ, data, in, nil)
	for len(deferred) > 0 {
		d := deferred[len(deferred)-1]
		deferred = deferred[:len(deferred)-1]
		data, err := w.rebuild(d, in)
		if err != nil {
			cannotUnpack(w.emit, p, l, p.OIDAt(l.ents[d].idx), l.ents[d])
			failSubtree(w.emit, p, l, d)
			continue
		}
		deferred = w.spread(d, typ, data, in, deferred)
	}
}

// spread builds every delta standing on base, and every delta standing on those,
// in one pass down the chain. base is spread's to drop.
//
// It holds one decoded object per level, and releases a level as soon as
// nothing below will read it again: descending into a node's last child hands
// the buffer over rather than stacking a second one on top of it, which is the
// whole of a chain that never branches. Past the budget a child is returned for
// the caller to rebuild later instead of being held here.
func (w *walker) spread(base int32, typ gitobj.Type, data []byte, in *Inflater, deferred []int32) []int32 {
	l, p := w.l, w.p
	stack := []frame{{entry: base, data: data, next: l.childStart[base]}}
	for len(stack) > 0 {
		i := len(stack) - 1
		top := &stack[i]
		end := l.childStart[top.entry+1]
		if top.next >= end {
			w.give(i, int64(len(top.data)))
			stack = stack[:i]
			continue
		}
		child := l.childList[top.next]
		top.next++
		last := top.next >= end
		ce := &l.ents[child]
		if ce.typ == gitobj.TypeBad {
			continue
		}
		delta, err := in.Inflate(p, ce.dataOff, ce.size)
		var out []byte
		if err == nil {
			out, err = applyDelta(top.data, delta)
		}
		if err != nil {
			cannotUnpack(w.emit, p, l, p.OIDAt(ce.idx), *ce)
			failSubtree(w.emit, p, l, child)
			continue
		}
		w.finish(child, typ, out)
		if l.childStart[child+1] == l.childStart[child] {
			continue
		}
		if last {
			// The parent has no more children, so nothing will read it again.
			w.give(i, int64(len(top.data)))
			if !w.take(i, int64(len(out))) {
				deferred = append(deferred, child)
				stack = stack[:i]
				continue
			}
			stack[i] = frame{entry: child, data: out, next: l.childStart[child]}
			continue
		}
		if !w.take(i+1, int64(len(out))) {
			deferred = append(deferred, child)
			continue
		}
		stack = append(stack, frame{entry: child, data: out, next: l.childStart[child]})
	}
	return deferred
}

// rebuild decodes one entry from the bottom of its own chain, for a delta the
// walk could not afford to keep when it first passed it. It holds two objects
// at a time rather than the chain.
func (w *walker) rebuild(i int32, in *Inflater) ([]byte, error) {
	l := w.l
	var path []int32
	for j := i; ; {
		path = append(path, j)
		if l.parents[j] < 0 {
			break
		}
		j = l.parents[j]
	}
	_, data, err := w.materializeRoot(&l.ents[path[len(path)-1]], in)
	if err != nil {
		return nil, err
	}
	for k := len(path) - 2; k >= 0; k-- {
		e := &l.ents[path[k]]
		delta, err := in.Inflate(w.p, e.dataOff, e.size)
		if err != nil {
			return nil, err
		}
		if data, err = applyDelta(data, delta); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// materializeRoot decodes a non-delta entry, or a ref-delta whose base lives
// outside this pack.
func (w *walker) materializeRoot(e *packEntry, in *Inflater) (gitobj.Type, []byte, error) {
	p := w.p
	switch e.typ {
	case gitobj.TypeRefDelta, gitobj.TypeOfsDelta:
		// buildLayout marks an unresolved delta bad, and walkChain drops a bad entry before it reaches here.
		return gitobj.TypeBad, nil, badDeltaBase(e.dataOff, p.Path)
	}
	data, err := in.Inflate(p, e.dataOff, e.size)
	if err != nil {
		return gitobj.TypeBad, nil, err
	}
	return e.typ, data, nil
}

// finishStreamed hashes a blob past core.bigFileThreshold without holding it,
// which is what git's verify_packfile() does. It applies only to an entry
// nothing deltas against, because a base must be in memory to build a child
// from it. The caller sees a nil payload, and fsck reports a .gitmodules or
// .gitattributes blob this large as too large to parse, as git does.
func (w *walker) finishStreamed(i int32) {
	e := &w.l.ents[i]
	oid := w.p.OIDAt(e.idx)
	got, err := w.p.StreamHash(e.dataOff, e.size, e.typ)
	if err != nil {
		cannotUnpack(w.emit, w.p, w.l, oid, *e)
		return
	}
	if got != oid {
		w.emit(oid, fmt.Sprintf("packed %s from %s is corrupt", oid, w.p.Path))
		return
	}
	if w.object != nil {
		w.object(oid, e.typ, e.size, nil)
	}
	if w.o.Progress != nil {
		w.o.Progress()
	}
}

// finish hashes one decoded object and hands it to the caller.
func (w *walker) finish(i int32, typ gitobj.Type, data []byte) {
	e := &w.l.ents[i]
	oid := w.p.OIDAt(e.idx)
	if HashLiteral(w.p.Algo, typ.Name(), data) != oid {
		w.emit(oid, fmt.Sprintf("packed %s from %s is corrupt", oid, w.p.Path))
		return
	}
	if w.object != nil {
		w.object(oid, typ, int64(len(data)), data)
	}
	if w.o.Progress != nil {
		w.o.Progress()
	}
}

// StreamHash hashes a pack entry without holding its payload, for a blob past
// core.bigFileThreshold.
func (p *Pack) StreamHash(dataOff, size int64, typ gitobj.Type) (gitobj.OID, error) {
	in := &Inflater{}
	r, err := in.InflateStream(p, dataOff)
	if err != nil {
		return gitobj.OID{}, err
	}
	h := p.Algo.New()
	fmt.Fprintf(h, "%s %d", typ.Name(), size)
	h.Write([]byte{0})
	if _, err := io.Copy(h, io.LimitReader(r, size)); err != nil {
		return gitobj.OID{}, err
	}
	return gitobj.FromBytes(h.Sum(nil)), nil
}
