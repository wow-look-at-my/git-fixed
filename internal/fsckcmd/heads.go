package fsckcmd

import (
	"fmt"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
)

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
		e := r.objs.Get(oid)
		if e == nil || e.Flags()&flagHasObj == 0 {
			r.rep.Errf(key, "error: %s: object missing", oid)
			r.fail(ErrorObject)
			continue
		}
		e.SetFlag(flagUsed)
		r.fsck.PutObjectName(oid, "%s", arg)
		r.markReachable(e)
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
		target, oid, ok := r.repo.Head(wt.Dir)
		if r.headLink(name, target, oid, ok) && !oid.IsNull() {
			r.handleRef(name, oid, false)
		}
		if r.o.IncludeReflogs {
			r.handleReflogs(wt)
		}
	}
	if r.defaultRefs.Load() == 0 {
		r.rep.Errf(sortKey{phase: phaseHeads, group: 1 << 20}, "notice: No default references")
		r.o.ShowUnreachable = false
	}
}

// handleRef is git's fsck_handle_ref().
func (r *run) handleRef(refname string, oid gitobj.OID, broken bool) {
	key := sortKey{phase: phaseHeads, oid: oid}
	var e *objEntry
	if !broken {
		e = r.objs.Get(oid)
	}
	if e == nil || e.Flags()&flagHasObj == 0 {
		r.rep.Errf(key, "error: %s: invalid sha1 pointer %s", refname, oid)
		r.fail(ErrorReachable)
		return
	}
	if e.Type() != gitobj.TypeCommit && fsck.IsBranchRef(refname) {
		r.rep.Errf(key, "error: %s: not a commit", refname)
		r.fail(ErrorRefs)
	}
	r.defaultRefs.Add(1)
	e.SetFlag(flagUsed)
	r.fsck.PutObjectName(oid, "%s", refname)
	r.markReachable(e)
}

// headLink is git's fsck_head_link(): it checks what HEAD points at without
// caring yet whether that object exists.
func (r *run) headLink(name, target string, oid gitobj.OID, ok bool) bool {
	key := sortKey{phase: phaseHeads}
	if r.o.Verbose {
		r.rep.Verbosef("Checking %s link", name)
	}
	if !ok {
		r.fail(ErrorRefs)
		r.rep.Errf(key, "error: invalid %s", name)
		return false
	}
	detached := target == "HEAD" || target == name
	if !detached && !strings.HasPrefix(target, "refs/heads/") {
		r.fail(ErrorRefs)
		r.rep.Errf(key, "error: %s points to something strange (%s)", name, target)
		return false
	}
	if oid.IsNull() {
		if detached {
			r.fail(ErrorRefs)
			r.rep.Errf(key, "error: %s: detached HEAD points at nothing", name)
			return false
		}
		r.rep.Errf(key, "notice: %s points to an unborn branch (%s)", name, strings.TrimPrefix(target, "refs/heads/"))
	}
	return true
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
		if r.o.Verbose {
			r.rep.Verbosef("Checking cache tree of %s", path)
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
		r.fsckIndex(idx, path, wt.IsMain)
	}
	return 0
}

func (r *run) fsckIndex(idx *gitrepo.Index, path string, isCurrent bool) {
	key := sortKey{phase: phaseIndex}
	for _, ce := range idx.Entries {
		if ce.Mode&0o170000 == 0o160000 {
			continue // a submodule commit is not this repository's object
		}
		e, ok := r.objs.Lookup(ce.OID, gitobj.TypeBlob)
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
		if e.Type() != gitobj.TypeTree {
			r.objError(key, ct.OID, "non-tree in cache-tree")
		}
	}
	for _, child := range ct.Children {
		r.fsckCacheTree(key, child, path)
	}
}
