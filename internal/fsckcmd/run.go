// Package fsckcmd implements git-fsck.
//
// The phases follow builtin/fsck.c, but each one runs its work in parallel and
// puts the output back in order before printing it.
//
// see docs/architecture.md
package fsckcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wow-look-at-my/git-fixed/internal/fsck"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
	"github.com/wow-look-at-my/git-fixed/internal/gitrepo"
	"github.com/wow-look-at-my/git-fixed/internal/odb"
)

// Exit status bits, the same ones builtin/fsck.c returns.
const (
	ErrorObject         = 001
	ErrorReachable      = 002
	ErrorPack           = 004
	ErrorRefs           = 010
	ErrorCommitGraph    = 020
	ErrorMultiPackIndex = 040
	ErrorPackRevIndex   = 0100
	ErrorBitmap         = 0200
)

// Options are the command's settings, one field per git fsck option.
type Options struct {
	ShowRoot         bool
	ShowTags         bool
	ShowUnreachable  bool
	IncludeReflogs   bool
	CheckFull        bool
	ConnectivityOnly bool
	Strict           bool
	KeepCacheObjects bool
	WriteLostFound   bool
	Verbose          bool
	ShowDangling     bool
	NameObjects      bool
	ShowProgress     bool
	Args             []string

	// Workers is how many goroutines decode and check objects at once.
	Workers int

	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// DefaultOptions returns the settings git starts from.
func DefaultOptions() *Options {
	return &Options{
		IncludeReflogs: true,
		CheckFull:      true,
		ShowDangling:   true,
		Workers:        runtime.GOMAXPROCS(0),
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
	}
}

// run holds one execution's state.
type run struct {
	o      *Options
	repo   *gitrepo.Repo
	db     *odb.DB
	objs   *objTable
	rep    *reporter
	fsck   *fsck.Options
	errors atomic.Uint32

	defaultRefs atomic.Int64

	fatalMu  sync.Mutex
	fatalMsg string

	pendingMu sync.Mutex
	pending   []*objEntry
}

func (r *run) fail(bits uint32) { r.errors.Or(bits) }

// Run performs the whole check and returns the status git would exit with.
func Run(o *Options) int {
	rep := newReporter(o.Stdout, o.Stderr)
	repo, err := gitrepo.Open(o.Dir)
	if err != nil {
		fmt.Fprintf(o.Stderr, "fatal: %s\n", err)
		return 128
	}
	db, err := odb.Open(repo.ObjectsDir, repo.DisplayObjectsDir, repo.Algo, !o.ConnectivityOnly)
	if err != nil {
		fmt.Fprintf(o.Stderr, "fatal: %s\n", err)
		return 128
	}
	defer db.Close()
	db.BigFileThreshold = repo.Config.Int("core.bigfilethreshold", 512*1024*1024)

	r := &run{o: o, repo: repo, db: db, objs: newObjTable(), rep: rep}
	r.fsck = fsck.NewOptions(repo.Algo)
	r.fsck.Strict = o.Strict
	if o.NameObjects {
		r.fsck.EnableObjectNames()
	}
	r.fsck.Error = r.fsckError
	if code := r.applyConfig(); code != 0 {
		return code
	}

	// die reports the one condition git exits 128 for, after printing what
	// the run has already found.
	die := func() int {
		rep.Flush()
		fmt.Fprintf(o.Stderr, "fatal: %s\n", r.died())
		return 128
	}

	if o.ConnectivityOnly {
		r.markForConnectivity()
	} else {
		r.checkObjectDirs()
		r.finishDeferredBlobs()
	}
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	r.handleArgs()
	if len(o.Args) == 0 {
		r.defaultHeads()
		o.KeepCacheObjects = true
	}
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	if o.KeepCacheObjects {
		if code := r.checkIndexes(); code != 0 {
			rep.Flush()
			return code
		}
	}
	rep.Flush()

	r.checkPackRevIndexes()
	r.verifyBitmapFiles()
	rep.Flush()

	r.checkConnectivity()
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	r.verifyGraphFiles()
	rep.Flush()
	if r.died() != "" {
		return die()
	}

	return int(r.errors.Load())
}

// applyConfig reads the fsck.* settings, which decide how loud each check is.
func (r *run) applyConfig() int {
	for _, e := range r.repo.Config.Entries() {
		name, ok := strings.CutPrefix(strings.ToLower(e.Key), "fsck.")
		if !ok || e.Value == nil {
			continue
		}
		if name == "skiplist" {
			if err := r.loadSkiplist(*e.Value); err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: %s\n", err)
				return 128
			}
			continue
		}
		if code := r.setMsgType(name, *e.Value); code != 0 {
			return code
		}
	}
	return 0
}

// setMsgType applies one fsck.<msgid> setting.
func (r *run) setMsgType(name, value string) int {
	id, ok := fsck.MsgIDByName(name)
	if !ok {
		fmt.Fprintf(r.o.Stderr, "fatal: Unhandled message id: %s\n", name)
		return 128
	}
	if id == fsck.MsgLargePathname {
		// This one may carry its own limit, as in "warn:1024".
		if colon := strings.IndexByte(value, ':'); colon >= 0 {
			limit, err := parseSize(value[colon+1:])
			if err != nil {
				fmt.Fprintf(r.o.Stderr, "fatal: unable to parse max tree entry len: %s\n", value[colon+1:])
				return 128
			}
			r.fsck.MaxTreeEntryLen = limit
			value = value[:colon]
		}
	}
	sev, ok := fsck.ParseSeverity(value)
	if !ok {
		fmt.Fprintf(r.o.Stderr, "fatal: Unknown fsck message type: '%s'\n", value)
		return 128
	}
	if sev != fsck.SevError && id.DefaultSeverity() == fsck.SevFatal {
		fmt.Fprintf(r.o.Stderr, "fatal: Cannot demote %s to %s\n", name, value)
		return 128
	}
	r.fsck.SetSeverity(id, sev)
	return 0
}

func parseSize(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// loadSkiplist reads the file of object names fsck must stay quiet about.
func (r *run) loadSkiplist(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not open '%s'", path)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		oid, ok := r.repo.Algo.Parse(line)
		if !ok {
			return fmt.Errorf("invalid object name: %s", line)
		}
		r.fsck.AddSkip(oid)
	}
	return nil
}

// fsckError renders one finding the way builtin/fsck.c's callback does. ctx is
// the sort key of whatever pass produced the finding.
func (r *run) fsckError(o *fsck.Options, ctx any, oid gitobj.OID, objType gitobj.Type, sev fsck.Severity, _ fsck.MsgID, message string) int {
	key, _ := ctx.(sortKey)
	key.oid = oid
	if sev == fsck.SevWarn {
		r.rep.Errf(key, "warning in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
		return 0
	}
	r.rep.Errf(key, "error in %s %s: %s", r.printableType(oid, objType), o.Describe(oid), message)
	r.fail(ErrorObject)
	return 1
}

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
	var badLinks []link
	var parseErrs []string
	broken := false
	linkCount := 0
	if typ == gitobj.TypeTree {
		scratch, _ := treeScratch.Get().(*[]fsck.TreeEntry)
		if scratch == nil {
			scratch = new([]fsck.TreeEntry)
		}
		entries, treeErr := fsck.ParseTreeInto(*scratch, buf, r.repo.Algo)
		*scratch = entries
		edges, badLinks, broken = r.treeEdges(key, e.OID, entries)
		linkCount = len(edges)
		ret := r.fsck.TreeEntries(key, e.OID, entries, treeErr)
		treeScratch.Put(scratch)
		r.recordEdges(e, edges, badLinks, parseErrs)
		if broken {
			r.objError(key, e.OID, "broken links")
		}
		if ret != 0 {
			return
		}
	} else {
		var links []link
		links, parseErrs = walkLinks(typ, e.OID, buf, r.repo.Algo, r.fsck.ObjectName(e.OID), r.o.NameObjects)
		linkCount = len(links)
		broken = len(parseErrs) > 0
		for _, msg := range parseErrs {
			r.rep.Errf(key, "error: %s", msg)
		}
		for _, l := range links {
			target, ok := r.objs.Lookup(l.oid, l.typ)
			if !ok {
				broken = true
			} else {
				target.SetFlag(flagUsed)
			}
			edges = append(edges, edge{target: target, typ: l.typ, viaTag: l.viaTag})
		}
		r.recordEdges(e, edges, badLinks, parseErrs)
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
func (r *run) recordEdges(e *objEntry, edges []edge, bad []link, errs []string) {
	if r.o.NameObjects {
		// --name-objects builds each name from the path the walk took to
		// reach an object, so a recorded edge cannot carry it. The walk
		// re-reads the object in that case.
		return
	}
	e.SetEdges(edges, bad, errs)
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
	for pi, p := range r.db.Packs() {
		r.checkPack(pi, p)
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
				jobs = append(jobs, job{oid: oid, path: full, shown: shownFull})
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
	r.parallel(len(jobs), func(i int) {
		j := jobs[i]
		r.checkLooseObject(sortKey{phase: phaseObjects, group: group, oid: j.oid}, j.oid, j.path, j.shown)
	})
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
	if res.TypeName != "" && res.Type == gitobj.TypeBad {
		r.rep.Errf(key, "error: %s: object is of unknown type '%s': %s", res.RealOID, res.TypeName, shown)
		failed = true
	}
	if failed {
		r.fail(ErrorObject)
		return
	}
	e, ok := r.objs.Lookup(oid, res.Type)
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
func (r *run) checkPack(group int, p *odb.Pack) {
	key := func(oid gitobj.OID, pos int64) sortKey {
		return sortKey{phase: phaseObjects, group: 1 + group, pos: pos, oid: oid}
	}
	ok := p.Verify(odb.VerifyOpts{
		Workers:          r.o.Workers,
		BigFileThreshold: r.db.BigFileThreshold,
		Emit: func(oid gitobj.OID, text string) {
			if oid.Valid() && strings.HasPrefix(text, "cannot unpack ") {
				// git's reader has already put this object on the
				// pack's bad list, so the next read of it dies.
				r.db.MarkBadPacked(oid)
			}
			r.rep.Errf(key(oid, 0), "error: %s", text)
		},
		Object: func(oid gitobj.OID, typ gitobj.Type, size int64, data []byte) {
			e, lookupOK := r.objs.Lookup(oid, typ)
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
					e, _ := r.objs.Lookup(oid, gitobj.TypeAny)
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
			e, _ := r.objs.Lookup(p.OIDAt(i), gitobj.TypeAny)
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

// ensureType fills in an object's type when nothing has said what it is yet.
// Only --connectivity-only leaves types unknown, because it never reads the
// objects; git resolves them the same way, when a reference or a walk first
// needs to know.
func (r *run) ensureType(e *objEntry) gitobj.Type {
	if t := e.Type(); t != gitobj.TypeNone {
		return t
	}
	t, _, err := r.db.Info(e.OID)
	if err != nil {
		return gitobj.TypeNone
	}
	e.SetType(t)
	return t
}

// readObject reads an object and notices the one failure git treats as fatal.
func (r *run) readObject(oid gitobj.OID) (gitobj.Type, []byte, error) {
	typ, buf, err := r.db.Read(oid)
	r.noteFatal(err)
	return typ, buf, err
}

// noteFatal remembers the first condition git would die on. Work already under
// way finishes, and the run stops at the end of the phase, which is as close as
// a parallel implementation gets to git's immediate exit.
func (r *run) noteFatal(err error) {
	var fatal *odb.FatalError
	if err == nil || !errors.As(err, &fatal) {
		return
	}
	r.fatalMu.Lock()
	if r.fatalMsg == "" {
		r.fatalMsg = fatal.Msg
	}
	r.fatalMu.Unlock()
}

// noteFatalMsg records a fatal condition the caller found itself, rather than
// one that came back from the object database.
func (r *run) noteFatalMsg(msg string) {
	r.fatalMu.Lock()
	if r.fatalMsg == "" {
		r.fatalMsg = msg
	}
	r.fatalMu.Unlock()
}

// died reports whether the run has hit a fatal condition.
func (r *run) died() string {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalMsg
}
