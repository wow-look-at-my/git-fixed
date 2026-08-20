package fsckcmd

// The per-object check: what fsck says about one object once it has decoded.

import (
	"sync"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// printableType is git's printable_type(): the spelling of an object's type, or
// "unknown" when nothing has said what it is.
func (r *run) printableType(oid gitobj.OID, typ gitobj.Type) string {
	if typ == gitobj.TypeNone || typ == gitobj.TypeBad {
		if e := r.objs.Get(oid); e != nil {
			typ = e.Type()
		}
	}
	if name := typ.Name(); name != "" && typ != gitobj.TypeNone {
		return name
	}
	return "unknown"
}

// objError is git's objerror(): a problem with an object that is not one of the
// numbered checks.
func (r *run) objError(key sortKey, oid gitobj.OID, text string) {
	r.fail(ErrorObject)
	r.rep.Errf(key, "error in %s %s: %s", r.printableType(oid, gitobj.TypeNone), r.fsck.Describe(oid), text)
}

// checkObject runs the object checks and the link walk for one object. It is
// git's fsck_obj().
func (r *run) checkObject(key sortKey, e *objEntry, typ gitobj.Type, buf []byte) {
	if e.SetFlag(flagSeen) {
		return
	}
	if r.o.Verbose {
		r.rep.Verbosef("Checking %s %s", r.printableType(e.OID, typ), r.fsck.Describe(e.OID))
	}
	// git walks the links first, marking each target used, and complains
	// once if any of them does not resolve to the right kind of object.
	// A tree is decoded once here and handed to both the link walk and the
	// object checks, because decoding it twice was the single largest cost
	// of the object pass.
	var edges []edge
	broken := false
	linkCount := 0
	if typ == gitobj.TypeTree {
		scratch, _ := treeScratch.Get().(*[]fsck.TreeEntry)
		if scratch == nil {
			scratch = new([]fsck.TreeEntry)
		}
		entries, treeErr := fsck.ParseTreeInto(*scratch, buf, r.repo.Algo)
		*scratch = entries
		edges, broken = r.treeEdges(entries)
		linkCount = len(edges)
		ret := r.fsck.TreeEntries(key, e.OID, entries, treeErr)
		treeScratch.Put(scratch)
		r.recordEdges(e, edges)
		if broken {
			r.objError(key, e.OID, "broken links")
		}
		if ret != 0 {
			return
		}
	} else {
		// The errors are dropped: parsable() ran this same walk before
		// this object got here and rejected it if the walk had anything to
		// say, which is where git rejects it too, in parse_object_buffer()
		// before any check runs.
		links, _ := walkLinks(typ, e.OID, buf, r.repo.Algo, r.fsck.ObjectName(e.OID), r.o.NameObjects)
		linkCount = len(links)
		for _, l := range links {
			target, idx, ok := r.objs.Lookup(l.oid, l.typ)
			if !ok {
				broken = true
			} else {
				target.SetFlag(flagUsed)
			}
			edges = append(edges, makeEdge(idx, ok, l.typ, l.viaTag))
		}
		r.recordEdges(e, edges)
		if broken {
			r.objError(key, e.OID, "broken links")
		}
		if r.fsck.Object(key, e.OID, typ, buf) != 0 {
			return
		}
	}
	if typ == gitobj.TypeCommit && r.o.ShowRoot && linkCount == 1 {
		r.rep.Outf(key, "root %s", r.fsck.Describe(e.OID))
	}
	if typ == gitobj.TypeTag && r.o.ShowTags {
		if _, info := r.fsck.TagWithInfo(key, e.OID, buf); info.Object.Valid() {
			r.rep.Outf(key, "tagged %s %s (%s) in %s",
				r.printableType(info.Object, info.TargetType),
				r.fsck.Describe(info.Object), info.Name, r.fsck.Describe(e.OID))
		}
	}
}

// treeScratch lends each worker one entry slice, so decoding a tree does not
// allocate one per tree.
var treeScratch sync.Pool

// recordEdges keeps the references for the connectivity walk, unless the names
// that walk prints make them useless.
func (r *run) recordEdges(e *objEntry, edges []edge) {
	if r.o.NameObjects {
		// --name-objects builds each name from the path the walk took to
		// reach an object, so a recorded edge cannot carry it. The walk
		// re-reads the object in that case.
		return
	}
	e.SetEdges(edges)
}

// markReachable is git's mark_object_reachable(): a root has no parent to blame
// for a link that leads nowhere.
func (r *run) markReachable(e *objEntry) {
	if e == nil || e.SetFlag(flagReachable) {
		return
	}
	if e.Flags()&flagHasObj == 0 {
		return
	}
	r.pending = append(r.pending, e)
}

func linkTypeName(typ gitobj.Type) string {
	if n := typ.Name(); n != "" && typ != gitobj.TypeAny {
		return n
	}
	return "unknown"
}
