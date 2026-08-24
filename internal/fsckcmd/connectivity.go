package fsckcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// checkConnectivity walks everything reachable from the roots, then reports on
// every object the run knows about. It is git's check_connectivity().
func (r *run) checkConnectivity() {
	r.traverseReachable()
	if r.o.ConnectivityOnly && (r.o.ShowDangling || r.o.WriteLostFound) {
		r.markUnreachableReferents()
	}
	if r.o.Verbose {
		r.rep.Verbosef("Checking connectivity (%d objects)", r.objs.HashSlots())
	}
	n := int(r.objs.Len())
	if r.o.Verbose {
		// The verbose line names each object as it is checked, and that order is part of what the reader is watching.
		for i := range n {
			e := r.objs.At(uint32(i))
			r.rep.Verbosef("Checking %s", r.fsck.Describe(r.oid(e)))
			r.checkOneObject(e)
		}
		return
	}
	// Every object in the repository passes through here.
	r.parallel(n, func(i int) { r.checkOneObject(r.objs.At(uint32(i))) })
}

// checkOneObject reports on one object's reachability.
func (r *run) checkOneObject(e *objEntry) {
	if e.Flags()&flagReachable != 0 {
		r.checkReachableObject(e)
	} else {
		r.checkUnreachableObject(e)
	}
}

// traverseReachable walks out from the roots. Every worker draws from one
// shared stack rather than taking a turn at each level: history is usually
// long and narrow, so a level at a time would leave three workers idle and pay
// a barrier per commit.
func (r *run) traverseReachable() {
	stack := r.pending
	r.pending = nil
	if len(stack) == 0 {
		return
	}
	// git counts the objects it walks here, with no total to measure them against.
	m := r.meterDelayed("Checking connectivity", 0)
	defer m.Finish()
	workers := r.o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers == 1 {
		for len(stack) > 0 {
			e := stack[len(stack)-1]
			stack = append(stack[:len(stack)-1], r.traverseOne(e)...)
			m.Step()
		}
		return
	}

	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	// active counts the workers holding objects that could still yield more.
	active := 0
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker keeps its own stack and only visits the shared one when it runs dry or has a surplus.
			local := make([]*objEntry, 0, walkBatch*2)
			// holding says this worker is counted in active.
			holding := false
			for {
				if len(local) == 0 {
					mu.Lock()
					if holding {
						active--
						holding = false
						if len(stack) > 0 || active == 0 {
							cond.Broadcast()
						}
					}
					for len(stack) == 0 && active > 0 {
						cond.Wait()
					}
					if len(stack) == 0 {
						mu.Unlock()
						cond.Broadcast()
						return
					}
					n := min(len(stack), walkBatch)
					local = append(local, stack[len(stack)-n:]...)
					stack = stack[:len(stack)-n]
					active++
					holding = true
					mu.Unlock()
				}

				e := local[len(local)-1]
				local = local[:len(local)-1]
				local = append(local, r.traverseOne(e)...)
				m.Step()

				// Hand back a surplus so the others have something to take.
				// History is narrow in places, and a worker that hoarded its
				// discoveries would leave every other core idle.
				if len(local) > 2*walkBatch {
					mu.Lock()
					stack = append(stack, local[:len(local)-walkBatch]...)
					cond.Broadcast()
					mu.Unlock()
					local = append(local[:0], local[len(local)-walkBatch:]...)
				}
			}
		}()
	}
	wg.Wait()
}

// walkBatch is how many objects a worker claims from the shared stack at once.
const walkBatch = 64

// traverseOne reads one object and marks everything it points at.
func (r *run) traverseOne(e *objEntry) []*objEntry {
	typ := r.ensureType(e)
	if typ == gitobj.TypeBlob || typ == gitobj.TypeNone {
		// A blob points at nothing, so git's walk returns immediately.
		return nil
	}
	key := sortKey{phase: phaseConnectivity, oid: r.oid(e)}
	if edges, cached := r.objs.Edges(e); cached {
		// Nothing to print alongside them: an object only has edges recorded once the object pass has read it.
		var out []*objEntry
		for _, ed := range edges {
			if !ed.ok() {
				out = r.markLinkInto(key, e, ed.typ(), false, nil, false, out)
				continue
			}
			// A resolved edge implies the type its target holds. see objTable.Edges.
			target := r.objs.At(ed.index())
			out = r.markLinkInto(key, e, target.Type(), false, target, true, out)
		}
		return out
	}
	_, buf, err := r.readObject(r.oid(e))
	if err != nil {
		r.rep.Errf(key, "error: Unknown object type for %s", r.fsck.Describe(r.oid(e)))
		return nil
	}
	links, parseErrs := walkLinks(typ, r.oid(e), buf, r.repo.Algo, r.fsck.ObjectName(r.oid(e)), r.o.NameObjects)
	for _, msg := range parseErrs {
		r.rep.Errf(key, "error: %s", msg)
	}
	var out []*objEntry
	for _, l := range links {
		if l.name != "" {
			r.fsck.PutObjectName(l.oid, "%s", l.name)
		}
		target, _, ok := r.objs.Lookup(l.oid, l.typ)
		out = r.markLinkInto(key, e, l.typ, l.viaTag, target, ok, out)
	}
	return out
}

// markLinkInto is markLink with the newly reachable objects collected into a
// per-worker slice instead of a shared queue.
func (r *run) markLinkInto(key sortKey, parent *objEntry, typ gitobj.Type, viaTag bool, target *objEntry, ok bool, sink []*objEntry) []*objEntry {
	if !ok || target == nil {
		r.rep.Outf(key, "broken link from %7s %s",
			r.printableType(r.oid(parent), parent.Type()), r.fsck.Describe(r.oid(parent)))
		r.rep.Outf(key, "broken link from %7s %s", linkTypeName(typ), "unknown")
		r.fail(ErrorReachable)
		// The link was refused on the type it implies, so the fault is the pair and not one object.
		r.notePartialDamage()
		return sink
	}
	if !viaTag && target.Type() != gitobj.TypeNone && target.Type() != typ {
		r.objError(key, r.oid(parent), "wrong object type in link")
	}
	if target.SetFlag(flagReachable) {
		return sink
	}
	if target.Flags()&flagHasObj == 0 {
		// A file that is present but unreadable is not a broken link:
		// the object check already complained about the file itself, and
		// the report below still calls the object missing.
		if !r.db.Has(r.oid(target)) {
			r.rep.Outf(key, "broken link from %7s %s\n              to %7s %s",
				r.printableType(r.oid(parent), parent.Type()), r.fsck.Describe(r.oid(parent)),
				r.printableType(r.oid(target), target.Type()), r.fsck.Describe(r.oid(target)))
			r.fail(ErrorReachable)
		}
		return sink
	}
	return append(sink, target)
}

// markUnreachableReferents marks what unreachable objects point at, so
// --dangling can tell a dropped tip from the objects hanging below it. Only
// --connectivity-only needs this, because otherwise the object pass already
// marked them.
func (r *run) markUnreachableReferents() {
	r.parallel(int(r.objs.Len()), func(i int) {
		e := r.objs.At(uint32(i))
		if e.Flags()&flagHasObj == 0 || e.Flags()&flagReachable != 0 {
			return
		}
		typ := e.Type()
		if typ == gitobj.TypeNone {
			t, _, err := r.readObject(r.oid(e))
			if err != nil {
				return
			}
			e.SetType(t)
			typ = t
		}
		if typ == gitobj.TypeBlob {
			return
		}
		if edges, cached := r.objs.Edges(e); cached {
			for _, ed := range edges {
				if ed.ok() {
					r.objs.At(ed.index()).SetFlag(flagUsed)
				}
			}
			return
		}
		_, buf, err := r.readObject(r.oid(e))
		if err != nil {
			return
		}
		links, _ := walkLinks(typ, r.oid(e), buf, r.repo.Algo, "", false)
		for _, l := range links {
			if target, _, ok := r.objs.Lookup(l.oid, l.typ); ok && target != nil {
				target.SetFlag(flagUsed)
			}
		}
	})
}

// checkReachableObject reports an object that something points at but that is
// not in the database.
func (r *run) checkReachableObject(e *objEntry) {
	if e.Flags()&flagHasObj != 0 {
		return
	}
	// Being in a pack is proof enough for git, which does not re-open the pack here.
	if r.db.HasPacked(r.oid(e)) {
		return
	}
	r.rep.Outf(sortKey{phase: phaseConnectivity, group: 1, oid: r.oid(e)}, "missing %s %s",
		r.printableType(r.oid(e), e.Type()), r.fsck.Describe(r.oid(e)))
	r.fail(ErrorReachable)
	r.noteDamaged(r.oid(e), true)
}

// checkUnreachableObject reports an object nothing reaches. git shows the tips
// of unreachable history by default and the whole set only when asked.
func (r *run) checkUnreachableObject(e *objEntry) {
	if e.Flags()&flagHasObj == 0 {
		// Missing and unreachable at once is not worth a word: nothing can reach it, so nothing misses it.
		return
	}
	key := sortKey{phase: phaseConnectivity, group: 1, oid: r.oid(e)}
	if r.o.ShowUnreachable {
		r.rep.Outf(key, "unreachable %s %s", r.printableType(r.oid(e), e.Type()), r.fsck.Describe(r.oid(e)))
		return
	}
	if e.Flags()&flagUsed != 0 {
		// Something unreachable points at it, so it is not a tip.
		return
	}
	if r.o.ShowDangling {
		r.rep.Outf(key, "dangling %s %s", r.printableType(r.oid(e), e.Type()), r.fsck.Describe(r.oid(e)))
	}
	if r.o.WriteLostFound {
		r.writeLostFound(key, e)
	}
}

// writeLostFound saves a dangling object under .git/lost-found, as git does.
func (r *run) writeLostFound(key sortKey, e *objEntry) {
	kind := "other"
	if e.Type() == gitobj.TypeCommit {
		kind = "commit"
	}
	dir := filepath.Join(r.repo.GitDir, "lost-found", kind)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		r.rep.Errf(key, "error: could not create lost-found")
		return
	}
	name := filepath.Join(dir, r.fsck.Describe(r.oid(e)))
	f, err := os.Create(name)
	if err != nil {
		r.rep.Errf(key, "error: could not create lost-found")
		return
	}
	defer f.Close()
	if e.Type() == gitobj.TypeBlob {
		if _, data, err := r.readObject(r.oid(e)); err == nil {
			if _, err := f.Write(data); err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: could not write '%s'\n", name)
			}
		}
		return
	}
	fmt.Fprintf(f, "%s\n", r.fsck.Describe(r.oid(e)))
}
