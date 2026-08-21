package fsckcmd

// The object pass: every loose file and every pack, checked in parallel.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
	"github.com/wow-look-at-my/git-fixed/internal/progress"
)

// checkObjectDirs walks every object directory and every pack. This is the
// heaviest phase and the one that parallelizes best: each loose fanout
// directory and each pack is independent work.
func (r *run) checkObjectDirs() {
	for i, dir := range r.db.Dirs {
		r.checkLooseDir(i, dir.Path, dir.Display)
	}
	if !r.o.CheckFull {
		return
	}
	// One meter spans every pack, as git's does: its total is the whole
	// repository's packed object count, not this pack's.
	packs := r.db.Packs()
	total := int64(0)
	for _, p := range packs {
		if p.OpenErr == nil {
			total += int64(p.Num)
		}
	}
	m := r.meterOn("Checking objects", total)
	defer m.Finish()
	for pi, p := range packs {
		r.checkPack(pi, p, m)
	}
}

// checkLooseDir checks every loose object under one object directory.
func (r *run) checkLooseDir(group int, path, shown string) {
	if r.o.Verbose {
		r.rep.Verbosef("Checking object directory")
	}
	hexsz := r.repo.Algo.HexSize
	type job struct {
		oid   gitobj.OID
		path  string
		shown string
		// sub is the fanout directory this object came from, which is what
		// the meter counts: git measures this phase in directories, so a
		// repository with a handful of loose objects is done at once.
		sub int
	}
	var (
		jobs  []job
		cruft []string
	)
	for i := 0; i < 256; i++ {
		sub := fmt.Sprintf("%02x", i)
		entries, err := os.ReadDir(filepath.Join(path, sub))
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			full := filepath.Join(path, sub, name)
			shownFull := filepath.Join(shown, sub, name)
			if oid, ok := r.repo.Algo.Parse(sub + name); ok && len(name) == hexsz-2 {
				jobs = append(jobs, job{oid: oid, path: full, shown: shownFull, sub: i})
				continue
			}
			if !strings.HasPrefix(name, "tmp_obj_") {
				cruft = append(cruft, shownFull)
			}
		}
	}
	for _, c := range cruft {
		r.rep.Errf(sortKey{phase: phaseObjects, group: group}, "bad sha1 file: %s", c)
	}
	m := r.meterOn("Checking object directories", 256)
	r.parallel(len(jobs), func(i int) {
		j := jobs[i]
		r.checkLooseObject(sortKey{phase: phaseObjects, group: group, oid: j.oid}, j.oid, j.path, j.shown)
		m.Advance(int64(j.sub) + 1)
	})
	m.Advance(256)
	m.Finish()
}

// checkLooseObject reads one loose object and checks it, following git's
// fsck_loose().
func (r *run) checkLooseObject(key sortKey, oid gitobj.OID, path, shown string) {
	res := odb.ReadLoose(path, shown, oid, r.repo.Algo, r.db.BigFileThreshold)
	for _, msg := range res.Errors {
		r.rep.Errf(key, "error: %s", msg)
	}
	failed := res.Failed
	if failed {
		if res.HashMismatch {
			r.rep.Errf(key, "error: %s: hash-path mismatch, found at: %s", res.RealOID, shown)
		} else if len(res.Errors) > 0 || res.Contents == nil {
			r.rep.Errf(key, "error: %s: object corrupt or missing: %s", oid, shown)
		}
	}
	if failed {
		r.fail(ErrorObject)
		r.noteDamaged(oid)
		return
	}
	e, _, ok := r.objs.Lookup(oid, res.Type)
	if !ok || !r.parsable(key, oid, res.Type, res.Contents) {
		r.fail(ErrorObject)
		r.rep.Errf(key, "error: %s: object could not be parsed: %s", oid, shown)
		return
	}
	e.ClearFlags(flagReachable | flagSeen)
	e.SetFlag(flagHasObj)
	r.checkObject(key, e, res.Type, res.Contents)
}

// parsable reports whether git's parse_object_buffer() would accept the object.
// A commit or tag whose header cannot be read is rejected there, before any
// fsck check runs.
func (r *run) parsable(key sortKey, oid gitobj.OID, typ gitobj.Type, buf []byte) bool {
	switch typ {
	case gitobj.TypeCommit, gitobj.TypeTag:
		_, errs := walkLinks(typ, oid, buf, r.repo.Algo, "", false)
		for _, msg := range errs {
			r.rep.Errf(key, "error: %s", msg)
		}
		return len(errs) == 0
	}
	return true
}

// checkPack verifies one pack and checks every object in it.
func (r *run) checkPack(group int, p *odb.Pack, m *progress.Meter) {
	key := func(oid gitobj.OID, pos int64) sortKey {
		return sortKey{phase: phaseObjects, group: 1 + group, pos: pos, oid: oid}
	}
	ok := p.Verify(odb.VerifyOpts{
		Workers:          r.o.Workers,
		BigFileThreshold: r.db.BigFileThreshold,
		Progress:         m.Step,
		Emit: func(oid gitobj.OID, text string) {
			if oid.Valid() && strings.HasPrefix(text, "cannot unpack ") {
				// The pack check reports an entry that will not
				// decode and carries on to the next one. It puts
				// the object on the bad list, and whoever reads
				// it by name afterwards dies instead.
				r.db.MarkBadPacked(oid)
			}
			r.rep.Errf(key(oid, 0), "error: %s", text)
		},
		Object: func(oid gitobj.OID, typ gitobj.Type, size int64, data []byte) {
			e, _, lookupOK := r.objs.Lookup(oid, typ)
			k := key(oid, 0)
			if !lookupOK || !r.parsable(k, oid, typ, data) {
				r.fail(ErrorObject)
				r.rep.Errf(k, "error: %s: object corrupt or missing", oid)
				return
			}
			e.ClearFlags(flagReachable | flagSeen)
			e.SetFlag(flagHasObj)
			r.checkObject(k, e, typ, data)
		},
	})
	if !ok {
		r.fail(ErrorPack)
		return
	}
	if r.o.PackVerified != nil {
		r.o.PackVerified(p.File)
	}
}

// finishDeferredBlobs checks the .gitmodules and .gitattributes blobs a tree
// named but whose content this run has not seen yet. It is git's fsck_finish().
func (r *run) finishDeferredBlobs() {
	key := sortKey{phase: phaseObjects, group: 1 << 20}
	check := func(oids []gitobj.OID, kind string) {
		sort.Slice(oids, func(i, j int) bool { return oids[i].Compare(oids[j]) < 0 })
		for _, oid := range oids {
			typ, data, err := r.readObject(oid)
			if err != nil {
				// A tree named this blob, so something wants it and
				// the database will not produce it.
				r.noteDamaged(oid)
				if r.fsck.ReportMissingBlob(key, oid, kind) != 0 {
					r.fail(ErrorObject)
				}
				continue
			}
			if typ != gitobj.TypeBlob {
				if r.fsck.ReportNonBlob(key, oid, typ, kind) != 0 {
					r.fail(ErrorObject)
				}
				continue
			}
			if r.fsck.Blob(key, oid, data) != 0 {
				r.fail(ErrorObject)
			}
		}
	}
	gitmodules, gitattributes := r.fsck.PendingBlobs()
	check(gitmodules, ".gitmodules")
	check(gitattributes, ".gitattributes")
}

// markForConnectivity records every object that exists without reading it,
// which is what --connectivity-only does.
func (r *run) markForConnectivity() {
	hexsz := r.repo.Algo.HexSize
	for _, dir := range r.db.Dirs {
		for i := 0; i < 256; i++ {
			sub := fmt.Sprintf("%02x", i)
			entries, err := os.ReadDir(filepath.Join(dir.Path, sub))
			if err != nil {
				continue
			}
			for _, ent := range entries {
				if len(ent.Name()) != hexsz-2 {
					continue
				}
				if oid, ok := r.repo.Algo.Parse(sub + ent.Name()); ok {
					e, _, _ := r.objs.Lookup(oid, gitobj.TypeAny)
					e.SetFlag(flagHasObj)
				}
			}
		}
	}
	for _, p := range r.db.Packs() {
		if p.OpenErr != nil {
			continue
		}
		for i := uint32(0); i < p.Num; i++ {
			e, _, _ := r.objs.Lookup(p.OIDAt(i), gitobj.TypeAny)
			e.SetFlag(flagHasObj)
		}
	}
}

// parallel runs fn for every index below n, across the configured workers.
func (r *run) parallel(n int, fn func(i int)) {
	if n == 0 {
		return
	}
	workers := r.o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > n {
		workers = n
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}
