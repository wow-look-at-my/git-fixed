package repair

// The scan. It reads the repository and says what is wrong, as data rather
// than as a report. Nothing here writes.

import (
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
	"github.com/wow-look-at-my/git-fixed/internal/progress"
	"github.com/wow-look-at-my/go-containers/concurrentbag"
	"github.com/wow-look-at-my/go-containers/concurrentmap"
)

// Need says who wants an object that is not usable.
type Need struct {
	// Ref names the reference that leads here, empty when nothing does.
	Ref string
	// Path is the route from that reference, as "<commit>:<dir>/<file>".
	Path string
}

// BadObject is one object the repository cannot produce.
type BadObject struct {
	OID gitobj.OID
	// Type is what the object must be, from the link that named it, or
	// TypeNone when only its name is known.
	Type gitobj.Type
	// Corrupt is set when a file for this object exists but will not decode.
	// A corrupt object has a file to quarantine; a missing one does not.
	Corrupt bool
	// Files are the paths holding the unusable copies, absolute.
	Files []string
	// Needs are the references and paths that want this object.
	Needs []Need
}

// BadRef is a reference the repository cannot use.
type BadRef struct {
	Name string
	// Path is the loose ref file, empty for a packed ref.
	Path string
	// Malformed is set when the ref file itself will not parse. The object it
	// meant to name may be perfectly fine.
	Malformed bool
	// Missing is the object the ref names, when that object is the problem.
	Missing gitobj.OID
}

// Damage is everything one scan found.
type Damage struct {
	// Derived are the cache files that will not parse. Every one is
	// rebuildable, so every one is safe to displace.
	Derived []string
	// Objects are the objects that are corrupt or gone.
	Objects []BadObject
	// Refs are the references that will not resolve.
	Refs []BadRef
	// Packs are the packfiles that will not verify.
	Packs []BadPack
	// Index is .git/index when it will not parse, with the reason. The index
	// is not a derived file: it holds staged work that exists nowhere else.
	Index *BadIndex
	// PackedRefs is packed-refs when it will not parse, with the reason.
	PackedRefs *BadPackedRefs
	// Unreachable counts the dangling and unreachable objects, which are not
	// damage. It is carried so a report can say so out loud.
	Unreachable int
}

// Empty reports whether the scan found nothing to repair.
func (d *Damage) Empty() bool {
	return len(d.Derived) == 0 && len(d.Objects) == 0 && len(d.Refs) == 0 &&
		len(d.Packs) == 0 && d.Index == nil && d.PackedRefs == nil
}

// scanner holds one scan's state.
//
// Both tables are keyed on the object name itself. Keying them on its hex
// spelling cost a 40-byte string for every tree entry in the repository, most
// of them thrown away at once, and kept one per object for the whole walk.
type scanner struct {
	repo *gitrepo.Repo
	db   *odb.DB

	bad  *concurrentmap.Map[gitobj.OID, *BadObject]
	seen *concurrentmap.Map[gitobj.OID, bool]
	// queue is a bag rather than a stack: the walk does not care what order
	// it reaches objects in, and a bag shards where a single head contends.
	queue *concurrentbag.Bag[queued]
	// anyBad is raised the moment an object first fails to read. Until then
	// every route offered to knownBad is about an object that is fine, and one
	// atomic load is the whole cost of saying so. concurrentmap.IsEmpty takes a
	// read lock on every shard to answer the same question.
	anyBad atomic.Bool
	// pending counts what is queued plus what a worker is still reading, so
	// an empty bag is not mistaken for a finished walk.
	pending atomic.Int64

	// trusted holds the packs that have been read end to end, object by
	// object, with every one of them decoding and hashing to its own name --
	// by this scan's own pack pass, or by the fsck the caller ran before it.
	//
	// It is what lets the walk stop inflating blobs. nil means nothing has
	// been read and nothing may be taken on trust.
	trusted map[string]bool
	meters  Meters
}

type queued struct {
	oid gitobj.OID
	typ gitobj.Type
	ref string
	// path is the route from that reference, kept as a link back to the
	// containing tree rather than as a whole string. see pathNode.
	path *pathNode
}

// pathNode is one step of the route to an object, sharing everything above it
// with its siblings.
//
// A joined string per entry looks harmless and is not: a directory of a
// thousand entries copies its own path a thousand times, and every copy stays
// in the queue until its object is read. A node is one pointer and one name,
// and the whole route above it is one pointer away.
//
// It is only ever rendered for an object that turns out to be bad.
type pathNode struct {
	parent *pathNode
	name   string
}

// String renders the route, from the reference down.
func (p *pathNode) String() string {
	if p == nil {
		return ""
	}
	var parts []string
	for n := p; n != nil; n = n.parent {
		parts = append(parts, n.name)
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), ":") {
			b.WriteByte('/')
		}
		b.WriteString(parts[i])
	}
	return b.String()
}

// need renders what a report says about one queued object.
func (q queued) need() Need { return Need{Ref: q.ref, Path: q.path.String()} }

// Scan reads the repository and reports what is damaged. meters draws the two
// passes that take the time, or is the zero value when nobody asked.
func Scan(repo *gitrepo.Repo, db *odb.DB, meters Meters) (*Damage, error) {
	return scan(repo, db, meters, nil)
}

// Meters is where a scan draws its progress. A scan of a broken repository
// reads every pack and then every object a reference leads to, which is a
// second pass over everything the fsck before it already read, and until this
// existed it printed nothing for the whole of it.
type Meters struct {
	// Stderr is where the meters are drawn. A nil writer, or Show left
	// false, means no meter is started at all.
	Stderr io.Writer
	Show   bool
}

// start begins one meter, or returns nil when this scan draws none.
func (m Meters) start(title string, total int64) *progress.Meter {
	if !m.Show || m.Stderr == nil {
		return nil
	}
	return progress.Start(m.Stderr, title, total)
}

// scan reads the repository, skipping the passes the caller's own fsck has
// already made.
//
// The two it can skip are the whole cost of a scan: verifying every packfile,
// and reading every object a reference leads to. Over a healthy repository of
// 229,960 objects they took a scan from 0.7s to 3.2s while finding nothing, and
// over a hundred million objects they are twenty minutes each.
//
// What may be skipped is decided bit by bit, because a verdict's bits answer
// different questions. A repository whose references are broken, whose
// commit-graph is wrong, or whose index will not parse has had every one of its
// objects read and approved by the fsck that found those faults, and reading
// them all again finds nothing.
//
// Everything else still runs, because fsck does not look at all of it. git
// never verifies info/packs, which is a cache for dumb HTTP clients, so a
// corrupt one leaves fsck happy and is still a file to put right.
func scan(repo *gitrepo.Repo, db *odb.DB, meters Meters, v *Verdict) (*Damage, error) {
	s := &scanner{
		repo:   repo,
		db:     db,
		bad:    concurrentmap.New[gitobj.OID, *BadObject](),
		seen:   concurrentmap.New[gitobj.OID, bool](),
		queue:  concurrentbag.New[queued](),
		meters: meters,
	}
	d := &Damage{}
	s.scanDerived(d)
	// Every pack the caller's fsck read end to end is a pack this does not
	// read again, whatever else that fsck was unhappy about. One corrupt
	// loose object used to condemn every pack in the repository to a second
	// full read.
	s.trustNamed(v.verifiedPacks())
	s.scanPacks(d)
	s.scanIndexes(d)
	// scanRefs first: reading the references is what makes git's own reader
	// pass over packed-refs, and its verdict on that file is what the check
	// below reports.
	s.scanRefs(d)
	s.scanPackedRefs(d)
	if !v.refsReach() {
		s.walk()
	}
	s.collect(d)
	return d, nil
}

// scanRefs reads every reference and queues the objects they name.
func (s *scanner) scanRefs(d *Damage) {
	for _, wt := range s.repo.Worktrees() {
		for _, ref := range s.repo.Refs(wt.Dir) {
			s.checkRef(d, wt.Dir, ref)
		}
		target, oid, ok := s.repo.Head(wt.Dir)
		switch {
		case !ok && target == "":
			d.Refs = append(d.Refs, BadRef{
				Name:      wt.RefName("HEAD"),
				Path:      filepath.Join(wt.Dir, "HEAD"),
				Malformed: true,
			})
		case ok:
			s.want(oid, gitobj.TypeAny, wt.RefName("HEAD"), nil)
		}
	}
}

// checkRef judges one reference.
func (s *scanner) checkRef(d *Damage, worktreeDir string, ref Ref) {
	name := ref.Name
	switch {
	case ref.Symref != "":
		// A symref that points nowhere is a broken ref, but only when its
		// target does not exist. A branch that has never been created is a
		// normal state for HEAD to be in.
		return
	case ref.Broken || !ref.OID.Valid():
		d.Refs = append(d.Refs, BadRef{
			Name:      name,
			Path:      s.refPath(worktreeDir, name),
			Malformed: true,
		})
	default:
		s.want(ref.OID, gitobj.TypeAny, name, nil)
	}
}

// Ref is the reference shape the scan reads, aliased so callers of this package
// do not have to import gitrepo for it.
type Ref = gitrepo.Ref

// refPath finds the loose file for a reference, empty when it is packed.
func (s *scanner) refPath(worktreeDir, name string) string {
	for _, dir := range []string{worktreeDir, s.repo.CommonDir} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Lstat(path); err == nil {
			return path
		}
	}
	return ""
}

// want queues an object the repository must be able to produce.
func (s *scanner) want(oid gitobj.OID, typ gitobj.Type, ref string, path *pathNode) {
	if !oid.Valid() {
		return
	}
	if !s.seen.TryAdd(oid, true) {
		// Record the extra need anyway: a report that names every route to a
		// missing object is worth more than one that names the first.
		if s.knownBad(oid) {
			s.recordNeed(oid, Need{Ref: ref, Path: path.String()})
		}
		return
	}
	// Counted before it is queued. The other order lets a worker find an
	// empty bag between the two and call the walk finished.
	s.pending.Add(1)
	s.queue.Add(queued{oid: oid, typ: typ, ref: ref, path: path})
}

// wantEntry is want for a tree entry, which is where nearly every call comes
// from.
//
// It takes the containing tree's node and the entry's own bytes instead of a
// finished pathNode, because an object that has been queued already needs
// neither. History shares its trees: the same subtree hangs off every commit
// that did not change it, and the same blob hangs off thousands of trees, so
// most calls here are about an object the walk has met before. Each of those
// was costing a node, a string copy of the name, and a walk up the whole route
// to build a string that was dropped on the next line. Rendering a route only
// for an object that turns out to be bad is the whole reason pathNode exists.
func (s *scanner) wantEntry(oid gitobj.OID, typ gitobj.Type, ref string, parent *pathNode, name []byte) {
	if !oid.Valid() {
		return
	}
	if !s.seen.TryAdd(oid, true) {
		if s.knownBad(oid) {
			s.recordNeed(oid, Need{Ref: ref, Path: entryPath(parent, name)})
		}
		return
	}
	s.pending.Add(1)
	s.queue.Add(queued{oid: oid, typ: typ, ref: ref, path: &pathNode{parent: parent, name: string(name)}})
}

// entryPath renders the route to one tree entry.
func entryPath(parent *pathNode, name []byte) string {
	return (&pathNode{parent: parent, name: string(name)}).String()
}

// knownBad reports whether this object has already been found unreadable.
//
// It is one atomic load, then a read where Compute would take the shard's write
// lock. Nearly every
// call is about an object that is perfectly fine, and taking a write lock to
// discover that -- once per repeated tree entry, on every worker -- serializes
// the walk behind a map that has nothing to say.
//
// An object that becomes bad after this asks is missed, and was missed before:
// the walk reads an object after every route to it has been offered, so a
// route seen early is not held anywhere to be attached later. The object itself
// is still reported, with the routes that came after it was read.
func (s *scanner) knownBad(oid gitobj.OID) bool {
	return s.anyBad.Load() && s.bad.Contains(oid)
}

// recordNeed adds another route to an object already known to be bad.
func (s *scanner) recordNeed(oid gitobj.OID, need Need) {
	s.bad.Compute(oid, func(old *BadObject, loaded bool) (*BadObject, bool) {
		if !loaded {
			// Nothing to add to, and nothing to create: this object read
			// back perfectly well.
			return nil, true
		}
		old.Needs = appendNeed(old.Needs, need)
		return old, false
	})
}

// walk reads every queued object and follows what it points at, so the scan
// reaches everything a reference needs rather than only the tips.
func (s *scanner) walk() {
	// There is no total to count against: the queue grows as the walk finds
	// what each object points at, so the number of objects it will reach is
	// not known until it has reached them. git's own connectivity meter
	// counts the same way, and for the same reason.
	// The walk reads each object it reaches once, so the repository's own
	// object count is an upper bound on what it will read. A meter counting
	// against it finishes at or below 100%, and until this had a total it
	// showed a rising number that said nothing about how far along it was.
	m := s.meters.start("Checking what the references reach", s.objectCount())
	defer m.Finish()

	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.walkWorker(m)
		}()
	}
	wg.Wait()
}

// trustNamed records the packs somebody else has already read end to end.
func (s *scanner) trustNamed(paths []string) {
	s.trusted = make(map[string]bool, len(paths))
	for _, p := range paths {
		s.trusted[p] = true
	}
}

// trust records one more pack this scan verified for itself.
func (s *scanner) trust(path string) {
	if s.trusted == nil {
		s.trusted = map[string]bool{}
	}
	s.trusted[path] = true
}

// provenBlob reports whether this object is a blob that has already been read
// and hashed.
//
// A blob points at nothing, so the walk asks it one question: can the
// repository produce it. A pack that has been read end to end has answered that
// for every object in it, by decoding each one and requiring it to hash to the
// name the index gives it. Inflating a blob again to hear the same answer is
// the longest part of this pass, and most of a repository's bytes are blobs.
//
// The type comes from the object's own header rather than from the tree entry
// that named it. A tree that calls a tree a blob is a fault of its own, and it
// must not be able to talk the walk out of a whole subtree.
func (s *scanner) provenBlob(oid gitobj.OID) bool {
	if len(s.trusted) == 0 {
		return false
	}
	typ, p, ok := s.db.TypeInPack(oid)
	return ok && typ == gitobj.TypeBlob && s.trusted[p.File]
}

// walkWorker reads what the bag holds until the walk is finished.
//
// An empty bag does not mean the walk is over: another worker may be part way
// through a tree that will queue a thousand more objects. pending counts both
// what is waiting and what is being read, so zero is the only honest answer to
// "is there any more".
func (s *scanner) walkWorker(m *progress.Meter) {
	for {
		q, ok := s.queue.TryTake()
		if !ok {
			if s.pending.Load() == 0 {
				return
			}
			// Somebody else is still reading. Stand aside rather than
			// spin on the bag's heads.
			runtime.Gosched()
			continue
		}
		m.Step()
		if s.provenBlob(q.oid) {
			// Nothing to read and nothing to follow. This is most of the
			// repository's bytes.
			s.pending.Add(-1)
			continue
		}
		if typ, data, err := s.db.Read(q.oid); err != nil {
			s.note(q, err)
		} else {
			switch typ {
			case gitobj.TypeCommit:
				s.walkCommit(q, data)
			case gitobj.TypeTree:
				s.walkTree(q, data)
			case gitobj.TypeTag:
				s.walkTag(q, data)
			}
		}
		// Last, so that everything this object queued is counted before
		// this one stops counting.
		s.pending.Add(-1)
	}
}

// note records that an object could not be read.
func (s *scanner) note(q queued, err error) {
	need := q.need()
	// copiesOf stats the object directories, so it runs outside the shard
	// lock. A second worker reaching the same object builds the same answer
	// and one of the two is dropped.
	files, corrupt := s.copiesOf(q.oid)
	s.bad.Compute(q.oid, func(old *BadObject, loaded bool) (*BadObject, bool) {
		if !loaded {
			old = &BadObject{OID: q.oid, Type: q.typ, Files: files, Corrupt: corrupt}
		}
		old.Needs = appendNeed(old.Needs, need)
		return old, false
	})
	// Raised after the object is in the map, so a reader that sees the flag
	// finds the object behind it.
	s.anyBad.Store(true)
	_ = err
}

// copiesOf finds the files holding an object, and reports whether any of them
// exists. A file that exists but will not decode is corrupt; no file at all
// means the object was deleted.
func (s *scanner) copiesOf(oid gitobj.OID) (files []string, corrupt bool) {
	name := oid.String()
	for _, dir := range s.db.Dirs {
		path := filepath.Join(dir.Path, name[:2], name[2:])
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	// A packed copy is not a file of its own, and a pack is never displaced
	// for one bad entry: the other objects in it are fine.
	return files, len(files) > 0
}

// walkCommit follows a commit's tree and parents.
func (s *scanner) walkCommit(q queued, data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if hex, ok := strings.CutPrefix(line, "tree "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeTree, q.ref, &pathNode{name: q.oid.String() + ":"})
			}
			continue
		}
		if hex, ok := strings.CutPrefix(line, "parent "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeCommit, q.ref, nil)
			}
		}
	}
}

// walkTree follows a tree's entries.
func (s *scanner) walkTree(q queued, data []byte) {
	entries, _ := fsck.ParseTree(data, s.repo.Algo)
	for _, e := range entries {
		if e.IsGitlink() {
			// A submodule's commit lives in the submodule, not here.
			continue
		}
		typ := gitobj.TypeBlob
		if e.IsDir() {
			typ = gitobj.TypeTree
		}
		s.wantEntry(e.OID, typ, q.ref, q.path, e.Name)
	}
}

// walkTag follows a tag to what it names.
func (s *scanner) walkTag(q queued, data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if hex, ok := strings.CutPrefix(line, "object "); ok {
			if oid, ok := s.repo.Algo.Parse(strings.TrimSpace(hex)); ok {
				s.want(oid, gitobj.TypeAny, q.ref, nil)
			}
			return
		}
	}
}

// appendNeed keeps the needs unique, so a report does not repeat itself.
func appendNeed(needs []Need, n Need) []Need {
	if n.Ref == "" && n.Path == "" {
		return needs
	}
	for _, have := range needs {
		if have == n {
			return needs
		}
	}
	return append(needs, n)
}

// collect turns the scan's tables into the answer, in a stable order.
func (s *scanner) collect(d *Damage) {
	for _, b := range s.bad.All() {
		d.Objects = append(d.Objects, *b)
	}
	sort.Slice(d.Objects, func(i, j int) bool {
		return d.Objects[i].OID.Compare(d.Objects[j].OID) < 0
	})
	sort.Slice(d.Refs, func(i, j int) bool { return d.Refs[i].Name < d.Refs[j].Name })
}

// Describe renders one bad object for a person to read.
func (b BadObject) Describe() string {
	state := "missing"
	if b.Corrupt {
		state = "corrupt"
	}
	var where []string
	for _, n := range b.Needs {
		switch {
		case n.Ref != "" && n.Path != "":
			where = append(where, fmt.Sprintf("%s -> %s", n.Ref, n.Path))
		case n.Ref != "":
			where = append(where, n.Ref)
		case n.Path != "":
			where = append(where, n.Path)
		}
	}
	if len(where) == 0 {
		return fmt.Sprintf("%s %s", state, b.OID)
	}
	return fmt.Sprintf("%s %s, needed by %s", state, b.OID, strings.Join(where, ", "))
}

// objectCount is how many objects the repository holds, packed and loose.
//
// It is the number the walk's meter counts against. A pack that will not open
// contributes nothing, which is right: the walk cannot read what is in it
// either.
func (s *scanner) objectCount() int64 {
	total := int64(0)
	for _, p := range s.db.Packs() {
		if p.OpenErr == nil {
			total += int64(p.Num)
		}
	}
	hexsz := s.repo.Algo.HexSize
	for _, dir := range s.db.Dirs {
		for i := range 256 {
			entries, err := os.ReadDir(filepath.Join(dir.Path, fmt.Sprintf("%02x", i)))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if len(e.Name()) == hexsz-2 {
					total++
				}
			}
		}
	}
	return total
}
