package fsckcmd

import (
	"fmt"
	"os"
	"path/filepath"
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
	for _, e := range r.objs.All() {
		if r.o.Verbose {
			r.rep.Verbosef("Checking %s", r.fsck.Describe(e.OID))
		}
		if e.Flags()&flagReachable != 0 {
			r.checkReachableObject(e)
		} else {
			r.checkUnreachableObject(e)
		}
	}
}

// traverseReachable walks out from the roots one level at a time, so each level
// spreads across every worker.
func (r *run) traverseReachable() {
	frontier := r.pending
	r.pending = nil
	for len(frontier) > 0 {
		var mu sync.Mutex
		var next []*objEntry
		r.parallel(len(frontier), func(i int) {
			local := r.traverseOne(frontier[i])
			if len(local) == 0 {
				return
			}
			mu.Lock()
			next = append(next, local...)
			mu.Unlock()
		})
		frontier = next
	}
}

// traverseOne reads one object and marks everything it points at.
func (r *run) traverseOne(e *objEntry) []*objEntry {
	typ := r.ensureType(e)
	if typ == gitobj.TypeBlob || typ == gitobj.TypeNone {
		// A blob points at nothing, so git's walk returns immediately.
		return nil
	}
	key := sortKey{phase: phaseConnectivity, oid: e.OID}
	_, buf, err := r.db.Read(e.OID)
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
		if l.badMode {
			r.rep.Errf(key, "error: in tree %s: entry %s has bad mode %.6o",
				r.fsck.Describe(e.OID), l.entry, l.rawMode)
			continue
		}
		if l.name != "" {
			r.fsck.PutObjectName(l.oid, "%s", l.name)
		}
		target, ok := r.objs.Lookup(l.oid, l.typ)
		out = r.markLinkInto(key, e, l, target, ok, out)
	}
	return out
}

// markLinkInto is markLink with the newly reachable objects collected into a
// per-worker slice instead of a shared queue.
func (r *run) markLinkInto(key sortKey, parent *objEntry, l link, target *objEntry, ok bool, sink []*objEntry) []*objEntry {
	if !ok || target == nil {
		r.rep.Outf(key, "broken link from %7s %s",
			r.printableType(parent.OID, parent.Type()), r.fsck.Describe(parent.OID))
		r.rep.Outf(key, "broken link from %7s %s", linkTypeName(l), "unknown")
		r.fail(ErrorReachable)
		return sink
	}
	if !l.viaTag && target.Type() != gitobj.TypeNone && target.Type() != l.typ {
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
	all := r.objs.All()
	r.parallel(len(all), func(i int) {
		e := all[i]
		if e.Flags()&flagHasObj == 0 || e.Flags()&flagReachable != 0 {
			return
		}
		typ := e.Type()
		if typ == gitobj.TypeNone {
			t, _, err := r.db.Read(e.OID)
			if err != nil {
				return
			}
			e.SetType(t)
			typ = t
		}
		if typ == gitobj.TypeBlob {
			return
		}
		_, buf, err := r.db.Read(e.OID)
		if err != nil {
			return
		}
		links, _ := walkLinks(typ, e.OID, buf, r.repo.Algo, "", false)
		for _, l := range links {
			if l.badMode {
				continue
			}
			if target, ok := r.objs.Lookup(l.oid, l.typ); ok && target != nil {
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
		if _, data, err := r.db.Read(e.OID); err == nil {
			if _, err := f.Write(data); err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: could not write '%s'\n", name)
			}
		}
		return
	}
	fmt.Fprintf(f, "%s\n", r.fsck.Describe(e.OID))
}
