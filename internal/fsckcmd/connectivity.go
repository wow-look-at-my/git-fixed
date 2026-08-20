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
		// The verbose line names each object as it is checked, and that
		// order is part of what the reader is watching, so this stays on one
		// goroutine when it is asked for.
		for i := range n {
			e := r.objs.At(uint32(i))
			r.rep.Verbosef("Checking %s", r.fsck.Describe(e.OID))
			r.checkOneObject(e)
		}
		return
	}
	// Every object in the repository passes through here. The checks touch
	// only their own entry and the reporter, both of which are safe to share,
	// so this runs across the workers like the phases before it.
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
	// git counts the objects it walks here, with no total to measure them
	// against, and stays quiet for a second first.
	m := r.meterDelayed("Checking connectivity", 0)
	defer m.finish()
	workers := r.o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers == 1 {
		for len(stack) > 0 {
			e := stack[len(stack)-1]
			stack = append(stack[:len(stack)-1], r.traverseOne(e)...)
			m.step()
		}
		return
	}

	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	// active counts the workers holding objects that could still yield more.
	// A worker that finds the stack empty must wait while any of them can.
	active := 0
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker keeps its own stack and only visits the shared one
			// when it runs dry or has a surplus. Taking the shared lock once
			// per object made it the limit on how many cores the walk could
			// use: with a worker per core, every core pays for that one lock
			// twice per object. A batch divides that traffic by its size.
			local := make([]*objEntry, 0, walkBatch*2)
			// holding says this worker is counted in active. It stays counted
			// for as long as it has local work, because that work can still
			// produce more for everyone else.
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
				m.step()

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
// Large enough that the lock is not the limit on many cores, small enough that
// a shallow history still spreads across them.
const walkBatch = 64

// traverseOne reads one object and marks everything it points at.
func (r *run) traverseOne(e *objEntry) []*objEntry {
	typ := r.ensureType(e)
	if typ == gitobj.TypeBlob || typ == gitobj.TypeNone {
		// A blob points at nothing, so git's walk returns immediately.
		return nil
	}
	key := sortKey{phase: phaseConnectivity, oid: e.OID}
	if edges, cached := e.Edges(); cached {
		// Nothing to print alongside them: an object only has edges
		// recorded once the object pass has read it, and the object pass
		// rejects one whose links will not parse before it gets that far.
		var out []*objEntry
		for _, ed := range edges {
			var target *objEntry
			if ed.ok() {
				target = r.objs.At(ed.index())
			}
			out = r.markLinkInto(key, e, ed.typ(), ed.viaTag(), target, ed.ok(), out)
		}
		return out
	}
	_, buf, err := r.readObject(e.OID)
	if err != nil {
		r.rep.Errf(key, "error: Unknown object type for %s", r.fsck.Describe(e.OID))
		return nil
	}
	links, parseErrs := walkLinks(typ, e.OID, buf, r.repo.Algo, r.fsck.ObjectName(e.OID), r.o.NameObjects)
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
			r.printableType(parent.OID, parent.Type()), r.fsck.Describe(parent.OID))
		r.rep.Outf(key, "broken link from %7s %s", linkTypeName(typ), "unknown")
		r.fail(ErrorReachable)
		return sink
	}
	if !viaTag && target.Type() != gitobj.TypeNone && target.Type() != typ {
		r.objError(key, parent.OID, "wrong object type in link")
	}
	if target.SetFlag(flagReachable) {
		return sink
	}
	if target.Flags()&flagHasObj == 0 {
		// A file that is present but unreadable is not a broken link:
		// the object check already complained about the file itself, and
		// the report below still calls the object missing.
		if !r.db.Has(target.OID) {
			r.rep.Outf(key, "broken link from %7s %s\n              to %7s %s",
				r.printableType(parent.OID, parent.Type()), r.fsck.Describe(parent.OID),
				r.printableType(target.OID, target.Type()), r.fsck.Describe(target.OID))
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
			t, _, err := r.readObject(e.OID)
			if err != nil {
				return
			}
			e.SetType(t)
			typ = t
		}
		if typ == gitobj.TypeBlob {
			return
		}
		if edges, cached := e.Edges(); cached {
			for _, ed := range edges {
				if ed.ok() {
					r.objs.At(ed.index()).SetFlag(flagUsed)
				}
			}
			return
		}
		_, buf, err := r.readObject(e.OID)
		if err != nil {
			return
		}
		links, _ := walkLinks(typ, e.OID, buf, r.repo.Algo, "", false)
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
	// Being in a pack is proof enough for git, which does not re-open the
	// pack here. A loose file that exists but does not decode still counts
	// as missing, and the object pass has already said why.
	if r.db.HasPacked(e.OID) {
		return
	}
	r.rep.Outf(sortKey{phase: phaseConnectivity, group: 1, oid: e.OID}, "missing %s %s",
		r.printableType(e.OID, e.Type()), r.fsck.Describe(e.OID))
	r.fail(ErrorReachable)
}

// checkUnreachableObject reports an object nothing reaches. git shows the tips
// of unreachable history by default and the whole set only when asked.
func (r *run) checkUnreachableObject(e *objEntry) {
	if e.Flags()&flagHasObj == 0 {
		// Missing and unreachable at once is not worth a word: nothing
		// can reach it, so nothing misses it.
		return
	}
	key := sortKey{phase: phaseConnectivity, group: 1, oid: e.OID}
	if r.o.ShowUnreachable {
		r.rep.Outf(key, "unreachable %s %s", r.printableType(e.OID, e.Type()), r.fsck.Describe(e.OID))
		return
	}
	if e.Flags()&flagUsed != 0 {
		// Something unreachable points at it, so it is not a tip.
		return
	}
	if r.o.ShowDangling {
		r.rep.Outf(key, "dangling %s %s", r.printableType(e.OID, e.Type()), r.fsck.Describe(e.OID))
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
	name := filepath.Join(dir, r.fsck.Describe(e.OID))
	f, err := os.Create(name)
	if err != nil {
		r.rep.Errf(key, "error: could not create lost-found")
		return
	}
	defer f.Close()
	if e.Type() == gitobj.TypeBlob {
		if _, data, err := r.readObject(e.OID); err == nil {
			if _, err := f.Write(data); err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: could not write '%s'\n", name)
			}
		}
		return
	}
	fmt.Fprintf(f, "%s\n", r.fsck.Describe(e.OID))
}
