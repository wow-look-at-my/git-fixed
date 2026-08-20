package fsckcmd

import (
	"fmt"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
)

// snapshotRefs reads the object each starting point names, which is what git
// does before it opens the object database at all. Nothing here reports
// anything: the later passes say what is wrong with a reference. The one thing
// this pass does produce is git's death on a packed object that will not
// decode, before the object pass has printed a word.
func (r *run) snapshotRefs() {
	read := func(oid gitobj.OID) {
		if oid.Valid() && !oid.IsNull() {
			_, _, _ = r.readObject(oid)
		}
	}
	if len(r.o.Args) > 0 {
		for _, arg := range r.o.Args {
			if oid, ok := r.repo.Algo.Parse(arg); ok {
				read(oid)
			}
		}
		return
	}
	for _, ref := range r.repo.Refs(r.repo.CommonDir) {
		read(ref.OID)
	}
	for _, wt := range r.repo.Worktrees() {
		if _, oid, ok := r.repo.Head(wt.Dir); ok {
			read(oid)
		}
	}
}

// handleArgs treats each command-line argument as a starting point.
func (r *run) handleArgs() {
	key := sortKey{phase: phaseHeads}
	for _, arg := range r.o.Args {
		oid, ok := r.repo.Algo.Parse(arg)
		if !ok {
			r.rep.Errf(key, "error: invalid parameter: expected sha1, got '%s'", arg)
			r.fail(ErrorObject)
			continue
		}
		// git hands the argument to the same code that handles a
		// reference, so an object that is not there is reported as a
		// reference pointing at nothing rather than as a missing object.
		r.handleRef(arg, oid, false)
	}
}

// defaultHeads walks references, HEAD, and reflogs for every worktree, which is
// git's get_default_heads().
func (r *run) defaultHeads() {
	for _, ref := range r.repo.Refs(r.repo.CommonDir) {
		r.handleRef(ref.Name, ref.OID, ref.Broken)
	}
	for _, wt := range r.repo.Worktrees() {
		name := wt.RefName("HEAD")
		// Whether HEAD is well formed is the ref database's business,
		// and checkRefs has already said so. Here it is only a starting
		// point, and one that resolves to nothing is simply not one.
		_, oid, ok := r.repo.Head(wt.Dir)
		if ok && !oid.IsNull() {
			r.handleRef(name, oid, false)
		}
		if r.o.IncludeReflogs {
			r.handleReflogs(wt)
		}
	}
}

// noteNoHeads reports that nothing became a starting point. git counts an
// object named on the command line the same way it counts a reference, so an
// argument that does not resolve leaves the run with no heads at all.
func (r *run) noteNoHeads() {
	if r.defaultRefs.Load() != 0 {
		return
	}
	r.rep.Errf(sortKey{phase: phaseHeads, group: 1 << 20}, "notice: No default references")
	r.o.ShowUnreachable = false
}

// handleRef is git's fsck_handle_ref().
func (r *run) handleRef(refname string, oid gitobj.OID, broken bool) {
	key := sortKey{phase: phaseHeads, oid: oid}
	var e *objEntry
	if !broken {
		e = r.objs.Get(oid)
	}
	if e == nil || e.Flags()&flagHasObj == 0 {
		// git parses the object again here, so an object that failed to
		// parse in the object pass reports its complaint a second time.
		r.reparse(key, oid)
		r.rep.Errf(key, "error: %s: invalid sha1 pointer %s", refname, oid)
		r.fail(ErrorReachable)
		return
	}
	if r.ensureType(e) != gitobj.TypeCommit && fsck.IsBranchRef(refname) {
		r.rep.Errf(key, "error: %s: not a commit", refname)
		r.fail(ErrorRefs)
	}
	r.defaultRefs.Add(1)
	e.SetFlag(flagUsed)
	r.fsck.PutObjectName(oid, "%s", refname)
	r.markReachable(e)
}

// handleReflogs walks one worktree's reflogs, which are also starting points.
func (r *run) handleReflogs(wt *gitrepo.Worktree) {
	for _, name := range r.repo.ReflogNames(wt.Dir) {
		refname := wt.RefName(name)
		for _, ent := range r.repo.Reflog(wt.Dir, name) {
			if r.o.Verbose {
				r.rep.Verbosef("Checking reflog %s->%s", ent.Old, ent.New)
			}
			r.handleReflogOID(refname, ent.Old, 0)
			r.handleReflogOID(refname, ent.New, ent.Timestamp)
		}
	}
}

func (r *run) handleReflogOID(refname string, oid gitobj.OID, timestamp int64) {
	if !oid.Valid() || oid.IsNull() {
		return
	}
	key := sortKey{phase: phaseHeads, group: 1, oid: oid}
	e := r.objs.Get(oid)
	if e == nil || e.Flags()&flagHasObj == 0 {
		r.rep.Errf(key, "error: %s: invalid reflog entry %s", refname, oid)
		r.fail(ErrorReachable)
		return
	}
	if timestamp != 0 {
		r.fsck.PutObjectName(oid, "%s@{%d}", refname, timestamp)
	}
	e.SetFlag(flagUsed)
	r.markReachable(e)
}

// checkIndexes walks every worktree's index, which is git's fsck_index().
func (r *run) checkIndexes() int {
	for _, wt := range r.repo.Worktrees() {
		path := wt.IndexPath()
		// Every message below names the index the way git names it. git
		// works from the top of the worktree and writes ".git/index"; this
		// process opened an absolute path and must not print it.
		shown := r.repo.Shown(path)
		if r.o.Verbose {
			r.rep.Verbosef("Checking cache tree of %s", shown)
		}
		idx, errs, err := r.repo.ReadIndex(path)
		for _, e := range errs {
			r.rep.Errf(sortKey{phase: phaseIndex}, "error: %s", e)
		}
		if err != nil {
			r.rep.Flush()
			fmt.Fprintf(r.o.Stderr, "fatal: %s\n", err)
			return 128
		}
		for _, sig := range idx.Ignored {
			r.rep.Errf(sortKey{phase: phaseIndex}, "ignoring %s extension", sig)
		}
		r.fsckIndex(idx, shown, wt.IsMain)
	}
	return 0
}

func (r *run) fsckIndex(idx *gitrepo.Index, path string, isCurrent bool) {
	key := sortKey{phase: phaseIndex}
	for _, ce := range idx.Entries {
		if ce.Mode&0o170000 == 0o160000 {
			continue // a submodule commit is not this repository's object
		}
		e, _, ok := r.objs.Lookup(ce.OID, gitobj.TypeBlob)
		if !ok || e == nil {
			continue
		}
		e.SetFlag(flagUsed)
		prefix := ""
		if !isCurrent {
			prefix = path
		}
		r.fsck.PutObjectName(ce.OID, "%s:%s", prefix, ce.Name)
		r.markReachable(e)
	}
	if idx.CacheTree != nil {
		r.fsckCacheTree(key, idx.CacheTree, path)
	}
	for _, ru := range idx.ResolveUndo {
		for i := 0; i < 3; i++ {
			if ru.Mode[i] == 0 || ru.Mode[i]&0o170000 != 0o100000 {
				continue
			}
			e := r.objs.Get(ru.OID[i])
			if e == nil || e.Flags()&flagHasObj == 0 {
				r.rep.Errf(key, "error: %s: invalid sha1 pointer in resolve-undo of %s", ru.OID[i], path)
				r.fail(ErrorRefs)
				continue
			}
			e.SetFlag(flagUsed)
			r.fsck.PutObjectName(ru.OID[i], ":(%d):%s", i, ru.Path)
			r.markReachable(e)
		}
	}
}

func (r *run) fsckCacheTree(key sortKey, ct *gitrepo.CacheTree, path string) {
	if ct.EntryCount >= 0 {
		e := r.objs.Get(ct.OID)
		if e == nil || e.Flags()&flagHasObj == 0 {
			r.rep.Errf(key, "error: %s: invalid sha1 pointer in cache-tree of %s", ct.OID, path)
			r.fail(ErrorRefs)
			return
		}
		e.SetFlag(flagUsed)
		r.fsck.PutObjectName(ct.OID, ":")
		r.markReachable(e)
		// ensureType, not Type: --connectivity-only never reads the objects,
		// so the type is still unknown here and every cache-tree entry reads
		// as a non-tree. git's own parse_object() resolves it at this point
		// and reports nothing.
		if r.ensureType(e) != gitobj.TypeTree {
			r.objError(key, ct.OID, "non-tree in cache-tree")
		}
	}
	for _, child := range ct.Children {
		r.fsckCacheTree(key, child, path)
	}
}

// reparse reads an object and repeats whatever its parser complains about. git
// re-parses an object every time a reference names it, so a broken object is
// reported once per attempt rather than once in total.
func (r *run) reparse(key sortKey, oid gitobj.OID) {
	typ, buf, err := r.readObject(oid)
	if err != nil {
		return
	}
	_, errs := walkLinks(typ, oid, buf, r.repo.Algo, "", false)
	for _, msg := range errs {
		r.rep.Errf(key, "error: %s", msg)
	}
}
